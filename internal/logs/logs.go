// Package logs keeps orchd's recent log lines in memory, tagged with the part
// of orchd (or the child process) that wrote them, so clients can follow them
// live, all together or one source at a time. The log file the service
// manager keeps is unchanged; this is a view onto the same lines plus the
// output of child processes such as Qdrant.
package logs

import (
	"bytes"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Entry is one log line.
type Entry struct {
	Seq     uint64
	Time    time.Time
	Source  string
	Message string
}

// maxMessage caps one stored line; a runaway line cannot fill the buffer.
const maxMessage = 2000

// Ring holds the most recent entries, oldest overwritten first.
type Ring struct {
	mu      sync.Mutex
	buf     []Entry
	start   int // index of the oldest entry
	count   int
	seq     uint64
	sources map[string]bool
	now     func() time.Time
}

// NewRing keeps the last size entries.
func NewRing(size int) *Ring {
	return &Ring{buf: make([]Entry, max(size, 1)), sources: map[string]bool{}, now: time.Now}
}

// Add stores one line from source. Empty lines are dropped.
func (r *Ring) Add(source, message string) {
	message = Clean(message)
	if message == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	e := Entry{Seq: r.seq, Time: r.now(), Source: source, Message: message}
	if r.count < len(r.buf) {
		r.buf[(r.start+r.count)%len(r.buf)] = e
		r.count++
	} else {
		r.buf[r.start] = e
		r.start = (r.start + 1) % len(r.buf)
	}
	r.sources[source] = true
}

// Since returns entries after seq, oldest first, optionally from one source,
// at most limit of them. When more match, the newest limit are returned, so a
// client that fell behind catches up with the present rather than the past.
// It also reports the last sequence number stored and the oldest still held.
func (r *Ring) Since(after uint64, source string, limit int) (entries []Entry, last, oldest uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 0; i < r.count; i++ {
		e := r.buf[(r.start+i)%len(r.buf)]
		if e.Seq > after && (source == "" || e.Source == source) {
			entries = append(entries, e)
		}
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	if r.count > 0 {
		oldest = r.buf[r.start].Seq
	}
	return entries, r.seq, oldest
}

// Sources lists every source seen, sorted.
func (r *Ring) Sources() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.sources))
	for s := range r.sources {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

var (
	ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	// The standard logger's "2006/01/02 15:04:05 " prefix; the entry has its
	// own time.
	stdTime = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(\.\d+)? `)
	prefix  = regexp.MustCompile(`^([a-z][a-z0-9-]{1,30}): `)
)

// Clean strips ANSI colour codes and control or format characters, trims
// space, and caps the length.
func Clean(s string) string {
	s = ansi.ReplaceAllString(s, "")
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s))
	if len(s) > maxMessage {
		s = strings.ToValidUTF8(s[:maxMessage], "") + "…"
	}
	return s
}

// aliases maps a log line's prefix to the orchd part that writes it, where the
// two differ (unit names are what `orch restart` and the app show).
var aliases = map[string]string{
	"docs":    "docs-scheduler",
	"sources": "sources-scheduler",
	"queue":   "workflow-worker",
	"goose":   "database",
	"embed":   "rag",
}

// knownSources are the prefixes taken as a source as they are.
var knownSources = map[string]bool{
	"orchd": true, "qdrant": true, "sources-scheduler": true, "docs-scheduler": true, "pr-babysitter": true,
	"routing-evaluator": true, "handoff": true, "session-summarizer": true, "self-update": true,
	"workflow-worker": true, "rag": true, "database": true,
}

var migration = regexp.MustCompile(`^OK +\d+_\S+\.sql\b`)

// Attribute splits a standard-logger line into its source and message:
// "docs: synced 3 repos" is ("docs-scheduler", "synced 3 repos"). Lines with no
// known prefix belong to orchd; goose's levelled lines to the database.
func Attribute(line string) (source, message string) {
	line = stdTime.ReplaceAllString(strings.TrimSpace(line), "")
	if m := prefix.FindStringSubmatch(line); m != nil {
		name := m[1]
		if alias, ok := aliases[name]; ok {
			name = alias
		}
		if knownSources[name] {
			return name, line[len(m[0]):]
		}
	}
	for _, level := range []string{"INFO ", "WARN ", "ERROR "} {
		if strings.HasPrefix(line, level) {
			return "database", line
		}
	}
	// goose reports each migration it applies as "OK   0001_name.sql (…)".
	if migration.MatchString(line) {
		return "database", line
	}
	return "orchd", line
}

// lineWriter turns writes into whole lines; a partial line waits for the next
// write. A line that grows past the cap without a newline is flushed as is.
type lineWriter struct {
	mu   sync.Mutex
	pend []byte
	emit func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pend = append(w.pend, p...)
	for {
		i := bytes.IndexByte(w.pend, '\n')
		if i < 0 {
			if len(w.pend) > 4*maxMessage {
				w.emit(string(w.pend))
				w.pend = w.pend[:0]
			}
			return len(p), nil
		}
		w.emit(string(w.pend[:i]))
		w.pend = w.pend[i+1:]
		// Release a consumed buffer: re-slicing would keep whatever array the
		// largest write ever grew alive for good.
		if len(w.pend) == 0 {
			w.pend = nil
		}
	}
}

// LogWriter is the standard logger's output: each line is attributed to its
// source (Attribute) and stored.
func (r *Ring) LogWriter() io.Writer {
	return &lineWriter{emit: func(line string) {
		source, message := Attribute(line)
		r.Add(source, message)
	}}
}

// SourceWriter stores every line written to it under source, for a child
// process's output.
func (r *Ring) SourceWriter(source string) io.Writer {
	return &lineWriter{emit: func(line string) { r.Add(source, line) }}
}
