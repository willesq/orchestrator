// Package rageval measures retrieval quality against a golden set: queries
// paired with the source URIs that should come back. It scores a run by where
// the first expected source ranks, so a change to chunking, ranking or the
// embedding model can be judged by number rather than by feel.
package rageval

import (
	"context"
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
	Expect     []string `yaml:"expect" json:"expect"`
	Scope      string   `yaml:"scope,omitempty" json:"scope,omitempty"`
	SourceType string   `yaml:"source_type,omitempty" json:"source_type,omitempty"`
	Tags       []string `yaml:"tags,omitempty" json:"tags,omitempty"`
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

// LoadSet reads a golden set; a missing file is an empty set.
func LoadSet(path string) (Set, error) {
	var s Set
	b, err := readCapped(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := yaml.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
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

// SaveSet writes a golden set, replacing the file atomically.
func SaveSet(path string, s Set) error {
	b, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return writeAtomic(path, b)
}

// Hit is one search result, reduced to what scoring needs.
type Hit struct {
	SourceURI string  `json:"source_uri"`
	Score     float32 `json:"score"`
}

// Searcher runs one case's query and returns its hits, best first.
type Searcher func(ctx context.Context, c Case, topK int) ([]Hit, error)

// Result is how one case did.
type Result struct {
	Query string `json:"query"`
	// Rank is the 1-based position of the first expected source among the
	// distinct sources returned; 0 means it was not found in the top k.
	Rank int      `json:"rank"`
	Top  []string `json:"top"` // distinct sources returned, best first
	Err  string   `json:"error,omitempty"`
}

// Report is a whole run.
type Report struct {
	TopK    int      `json:"top_k"`
	Results []Result `json:"results"`
	HitAt1  float64  `json:"hit_at_1"`
	Recall  float64  `json:"recall_at_k"`
	MRR     float64  `json:"mrr"`
}

// Run scores every case. Hits are ranked by distinct source, since several
// chunks of one document should count once. A case whose search fails counts
// as a miss and records the error.
func Run(ctx context.Context, s Set, topK int, search Searcher) Report {
	r := Report{TopK: topK}
	for _, c := range s.Cases {
		// A cancelled run stops rather than sending every remaining query.
		if ctx.Err() != nil {
			break
		}
		res := Result{Query: c.Query}
		hits, err := search(ctx, c, topK)
		if err != nil {
			res.Err = err.Error()
		}
		seen := map[string]bool{}
		for _, h := range hits {
			if seen[h.SourceURI] {
				continue
			}
			seen[h.SourceURI] = true
			res.Top = append(res.Top, h.SourceURI)
			if res.Rank == 0 && matchesAny(h.SourceURI, c.Expect) {
				res.Rank = len(res.Top)
			}
		}
		r.Results = append(r.Results, res)
	}
	if n := float64(len(r.Results)); n > 0 {
		for _, res := range r.Results {
			if res.Rank == 1 {
				r.HitAt1++
			}
			if res.Rank > 0 {
				r.Recall++
				r.MRR += 1 / float64(res.Rank)
			}
		}
		r.HitAt1 /= n
		r.Recall /= n
		r.MRR /= n
	}
	return r
}

func matchesAny(uri string, patterns []string) bool {
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(uri, prefix) {
				return true
			}
		} else if uri == p {
			return true
		}
	}
	return false
}

// Change is one case whose rank moved between two runs.
type Change struct {
	Query    string
	From, To int // 0 = missed
}

// Better reports whether the case improved.
func (c Change) Better() bool { return c.To != 0 && (c.From == 0 || c.To < c.From) }

// Compare lists cases present in both runs whose rank changed, in run order.
func Compare(before, after Report) []Change {
	prev := map[string]int{}
	for _, r := range before.Results {
		prev[r.Query] = r.Rank
	}
	var out []Change
	for _, r := range after.Results {
		from, ok := prev[r.Query]
		if ok && from != r.Rank {
			out = append(out, Change{Query: r.Query, From: from, To: r.Rank})
		}
	}
	return out
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
