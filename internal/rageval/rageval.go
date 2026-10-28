// Package rageval measures retrieval quality against a golden set: queries
// paired with the source URIs that should come back. It scores a run by where
// the first expected source ranks (recall@k, MRR, nDCG@10), and compares two
// runs with a paired bootstrap, so a change to chunking, ranking or the
// embedding model can be judged by number rather than by feel.
package rageval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Case is one query and the sources that answer it.
type Case struct {
	Query string `yaml:"query" json:"query"`
	// Expect lists source URIs, any of which counts as a hit. A trailing "*"
	// matches any URI with that prefix.
	Expect []string `yaml:"expect" json:"expect"`
	// Span, when set, must appear in the hit's text for the hit to count:
	// the source alone is then only a "source hit".
	Span       string   `yaml:"span,omitempty" json:"span,omitempty"`
	Scope      string   `yaml:"scope,omitempty" json:"scope,omitempty"`
	SourceType string   `yaml:"source_type,omitempty" json:"source_type,omitempty"`
	Tags       []string `yaml:"tags,omitempty" json:"tags,omitempty"`
	// Section groups cases in the report: hard, legacy, seed-decision,
	// seed-heading, seed-ident, or anything a user picks.
	Section string `yaml:"section,omitempty" json:"section,omitempty"`
	// Note says why the case exists; it is not scored.
	Note string `yaml:"note,omitempty" json:"note,omitempty"`
}

// Set is a golden set.
type Set struct {
	Cases []Case `yaml:"cases"`
}

// Limits on what a golden set or saved run may hold.
const (
	maxFileBytes = 4 << 20
	maxCases     = 1000
)

// readCapped reads a file of at most maxFileBytes.
func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFileBytes {
		return nil, fmt.Errorf("%s is larger than %d MiB", path, maxFileBytes>>20)
	}
	return b, nil
}

// IsJSONL reports whether a set file is stored one JSON case per line.
func IsJSONL(path string) bool { return strings.EqualFold(filepath.Ext(path), ".jsonl") }

// LoadSet reads a golden set; a missing file is an empty set. A .jsonl file
// holds one case per line (blank lines and lines starting with # are
// skipped); anything else is YAML with a top-level cases list.
func LoadSet(path string) (Set, error) {
	var s Set
	b, err := readCapped(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if IsJSONL(path) {
		s, err = parseJSONL(b, path)
	} else if err = yaml.Unmarshal(b, &s); err != nil {
		err = fmt.Errorf("%s: %w", path, err)
	}
	if err != nil {
		return Set{}, err
	}
	if len(s.Cases) > maxCases {
		return s, fmt.Errorf("%s has %d cases; the limit is %d", path, len(s.Cases), maxCases)
	}
	for i, c := range s.Cases {
		if strings.TrimSpace(c.Query) == "" || len(c.Expect) == 0 {
			return s, fmt.Errorf("%s: case %d needs a query and at least one expected source", path, i+1)
		}
	}
	return s, nil
}

func parseJSONL(b []byte, path string) (Set, error) {
	var s Set
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), maxFileBytes)
	line := 0
	for sc.Scan() {
		line++
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if len(s.Cases) == maxCases {
			return s, fmt.Errorf("%s has more than %d cases; the limit is %d", path, maxCases, maxCases)
		}
		var c Case
		dec := json.NewDecoder(strings.NewReader(t))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return s, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		s.Cases = append(s.Cases, c)
	}
	if err := sc.Err(); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// SaveSet writes a golden set, replacing the file atomically, as JSONL or
// YAML by the file's extension.
func SaveSet(path string, s Set) error {
	if !IsJSONL(path) {
		b, err := yaml.Marshal(s)
		if err != nil {
			return err
		}
		return writeAtomic(path, b)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, c := range s.Cases {
		if err := enc.Encode(c); err != nil {
			return err
		}
	}
	return writeAtomic(path, buf.Bytes())
}

// runName is a saved run's name; it becomes a file name.
var runName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func runPath(dir, name string) (string, error) {
	if !runName.MatchString(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("run name %q must be letters, digits, '.', '_' or '-'", name)
	}
	return filepath.Join(dir, name+".json"), nil
}

// SaveRun stores a report under name in dir.
func SaveRun(dir, name string, r Report) error {
	p, err := runPath(dir, name)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(p, b)
}

// LoadRun reads a saved report.
func LoadRun(dir, name string) (Report, error) {
	var r Report
	p, err := runPath(dir, name)
	if err != nil {
		return r, err
	}
	b, err := readCapped(p)
	if errors.Is(err, fs.ErrNotExist) {
		return r, fmt.Errorf("no saved run %q", name)
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("saved run %q: %w", name, err)
	}
	return r, nil
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
