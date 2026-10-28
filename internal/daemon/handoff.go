package daemon

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/redact"
	"github.com/willesq/orchestrator/internal/session"
	"github.com/willesq/orchestrator/internal/sources"
	"github.com/willesq/orchestrator/internal/supervisor"
)

// UnitHandoff is the supervised unit that does the work hooks hand off:
// storing finished sessions' transcripts and edited documents.
const UnitHandoff = "handoff"

// Variables only so tests need not wait out real intervals.
var (
	// handoffEvery is how often hand-offs are looked for.
	handoffEvery = 10 * time.Second
	// handoffSettle is how old an entry must be before it is taken, so the
	// hook that wrote it has finished rewriting it.
	handoffSettle = 5 * time.Second
)

// Bounds on hand-offs.
const (
	handoffTimeout = 10 * time.Minute
	// maxHandoffAttempts tries, backing off from handoffBackoff and doubling,
	// span about two hours, so a store that is down for a while loses nothing.
	maxHandoffAttempts = 8
	handoffBackoff     = 30 * time.Second
	// handoffPerTick bounds the work one tick takes on, so a backlog cannot
	// keep the unit from noticing it should stop.
	handoffPerTick = 20
	// maxHandoffDoc is the largest document stored again after an edit.
	maxHandoffDoc = 4 << 20
)

// errSummaryQueueFull defers a session whose summary could not be queued.
var errSummaryQueueFull = errors.New("summary queue full")

// due reports whether an entry that failed attempts times may be tried again.
func due(attempts int, updated time.Time) bool {
	if attempts == 0 {
		return true
	}
	return time.Since(updated) >= handoffBackoff<<min(attempts-1, 10)
}

// sessionLocks serialize the record writes of one session between the hand-off
// and the summarizer, so a stale excerpt never lands over a new summary.
var sessionLocks [64]sync.Mutex

func lockSession(id string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	m := &sessionLocks[h.Sum32()%uint32(len(sessionLocks))]
	m.Lock()
	return m.Unlock
}

// handoffUnit does hand-offs as they appear, including any left from before
// orchd last started. summaries is nil when summaries are off.
func handoffUnit(ing summaryIngester, summaries *summaryQueue, dir string, canonical sources.CanonicalFunc) supervisor.Unit {
	return supervisor.Unit{
		Name:   UnitHandoff,
		Detail: "stores transcripts and documents that hooks hand off",
		Run: func(ctx context.Context, beat func()) error {
			beat()
			go func() {
				t := time.NewTicker(handoffEvery)
				defer t.Stop()
				for {
					cutoff := time.Now().Add(-handoffSettle)
					storeSessions(ctx, ing, summaries, dir, cutoff)
					storeFiles(ctx, ing, dir, cutoff, canonical)
					beat()
					select {
					case <-ctx.Done():
						return
					case <-t.C:
					}
				}
			}()
			return nil
		},
	}
}

// storeSessions stores the handed-off sessions older than cutoff that are due.
func storeSessions(ctx context.Context, ing summaryIngester, summaries *summaryQueue, dir string, cutoff time.Time) {
	pending, err := handoff.Sessions(dir, cutoff)
	if err != nil {
		log.Printf("handoff: list sessions: %v", err)
		return
	}
	done := 0
	for _, s := range pending {
		if ctx.Err() != nil || done == handoffPerTick {
			return
		}
		if !due(s.Attempts, s.Updated) {
			continue
		}
		done++
		var err error
		if !s.Stored {
			err = storeSession(ctx, ing, s)
		}
		// The summarizer reads the transcript, so it gets only a valid one, as
		// the SummarizeSession RPC requires.
		if err == nil && s.Summarize && summaries != nil && validTranscript(s.TranscriptPath) &&
			!summaries.enqueue(summaryJob{SessionID: s.ID, TranscriptPath: s.TranscriptPath, Tags: s.Tags}) {
			// The summarizer is busy (a backfill can fill its queue): keep the
			// entry for the summary alone and try again after the backoff.
			s.Stored, s.Record = true, false
			err = errSummaryQueueFull
		}
		switch {
		case err == nil:
			_ = handoff.DoneSession(dir, s)
		case s.Attempts+1 >= maxHandoffAttempts:
			log.Printf("handoff: giving up on session %s, kept in handoff/failed: %v", s.ID, err)
			_ = handoff.FailSession(dir, s)
		default:
			log.Printf("handoff: session %s failed, will retry: %v", s.ID, err)
			_ = handoff.RetrySession(dir, s)
		}
	}
}

// storeSession stores a session's transcript, and its record when the hook
// could not. A transcript that is gone or not a session transcript is dropped.
func storeSession(ctx context.Context, ing summaryIngester, s handoff.Session) error {
	if !validTranscript(s.TranscriptPath) {
		log.Printf("handoff: session %s: transcript %q is missing or not a .jsonl file; skipped", s.ID, s.TranscriptPath)
		return nil
	}
	msgs, err := session.ParseRedacted(s.TranscriptPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, handoffTimeout)
	defer cancel()
	start, end := session.Span(msgs)
	if s.Record {
		if err := storeRecord(ctx, ing, s, start, end); err != nil {
			return err
		}
	}
	// Each chunk is dated by the turns it holds, so a session spanning days is
	// found under the day each part happened.
	_, err = ing.IngestText(ctx, session.TranscriptText(msgs), rag.Meta{
		SourceURI: "session://" + s.ID + "/transcript", SourceType: "session_transcript", Tags: s.Tags,
		Timeline: true, StartedAt: start, EndedAt: end,
	})
	return err
}

// storeRecord stores the session record as it is on disk. The summarizer
// writes its summary there before storing it, under the same lock, so the
// record read here is never older than the one stored. A record written
// before sessions were dated takes start and end from the transcript, and is
// saved with them.
func storeRecord(ctx context.Context, ing summaryIngester, s handoff.Session, start, end time.Time) error {
	defer lockSession(s.ID)()
	rec, err := session.ReadRecord(paths.SessionsDir(), s.ID)
	if err != nil {
		return nil // no record to store
	}
	if rec.StartedAt == "" && rec.EndedAt == "" && !(start.IsZero() && end.IsZero()) {
		rec.SetSpan(start, end)
		_ = session.WriteRecord(paths.SessionsDir(), rec)
	}
	recStart, recEnd := rec.Span()
	_, err = ing.IngestText(ctx, rec.Text(), rag.Meta{
		SourceURI: "session://" + s.ID + "/record", SourceType: "session_record", Tags: s.Tags,
		StartedAt: recStart, EndedAt: recEnd,
	})
	return err
}

// storeFiles stores the handed-off documents older than cutoff that are due.
// canonical, when set, names the repo:// URIs a file's doc is synced under.
func storeFiles(ctx context.Context, ing summaryIngester, dir string, cutoff time.Time, canonical sources.CanonicalFunc) {
	pending, err := handoff.Files(dir, cutoff)
	if err != nil {
		log.Printf("handoff: list files: %v", err)
		return
	}
	home, _ := os.UserHomeDir()
	done := 0
	for _, f := range pending {
		if ctx.Err() != nil || done == handoffPerTick {
			return
		}
		if !due(f.Attempts, f.Updated) {
			continue
		}
		done++
		err := storeFile(ctx, ing, f, home, canonical)
		switch {
		case err == nil:
			_ = handoff.DoneFile(dir, f)
		case f.Attempts+1 >= maxHandoffAttempts:
			log.Printf("handoff: giving up on %s, kept in handoff/failed: %v", f.Path, err)
			_ = handoff.FailFile(dir, f)
		default:
			log.Printf("handoff: %s failed, will retry: %v", f.Path, err)
			_ = handoff.RetryFile(dir, f)
		}
	}
}

// storeFile stores an edited document as it is now. The entry names only a
// path, so what it may be is decided again here, after resolving symlinks, and
// the file read is the one that was checked.
func storeFile(ctx context.Context, ing summaryIngester, f handoff.File, home string, canonical sources.CanonicalFunc) error {
	sourceType, uri, ok := handoff.Classify(f.Path, home)
	if !ok {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(f.Path)
	if err != nil {
		return nil // deleted since the edit
	}
	if _, _, ok := handoff.Classify(resolved, home); !ok {
		return nil
	}
	// Mapped from the resolved path: a link inside a repo to a note outside
	// it is that note, not a repo doc.
	if synced, err := canonicalStored(ctx, ing, canonical, uri, resolved); synced || err != nil {
		return err
	}
	raw, ok, err := readRegular(resolved, maxHandoffDoc)
	if err != nil || !ok {
		return err
	}
	text, redactions := redact.Redact(string(raw))
	// A file full of secrets is likely a keystore, not documentation.
	if redactions > redact.Threshold {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, handoffTimeout)
	defer cancel()
	_, err = ing.IngestText(ctx, text, rag.Meta{SourceURI: uri, SourceType: sourceType})
	return err
}

// canonicalStore tells whether a source is stored and removes one
// (implemented by *rag.Service).
type canonicalStore interface {
	HasSource(ctx context.Context, uri string) (bool, error)
	DeleteSource(ctx context.Context, uri string) (int, error)
}

// canonicalStored reports whether an edited doc is already stored under the
// repo:// URI the docs or sources sync gives it. Such a doc is not stored
// again under its path (uri): the copy would duplicate it in every search,
// and a worktree's would carry a branch's text beside the default branch's.
// The edit reaches the store when it is merged and synced. A copy stored
// before this check existed is removed. A doc no sync has stored, such as an
// untracked plan, is still stored under its path.
func canonicalStored(ctx context.Context, ing summaryIngester, canonical sources.CanonicalFunc, uri string, paths ...string) (bool, error) {
	cs, ok := ing.(canonicalStore)
	if canonical == nil || !ok {
		return false, nil
	}
	for _, p := range paths {
		for _, c := range canonical(ctx, p) {
			has, err := cs.HasSource(ctx, c)
			if err != nil {
				return false, err
			}
			if has {
				_, err := cs.DeleteSource(ctx, uri)
				return true, err
			}
		}
	}
	return false, nil
}

// readRegular reads path if it is a regular file of at most limit bytes,
// checking the open file itself so it cannot be swapped after the check.
// ok is false for anything else.
func readRegular(path string, limit int64) (b []byte, ok bool, err error) {
	fd, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, nil
	}
	defer fd.Close()
	info, err := fd.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, false, nil
	}
	b, err = io.ReadAll(io.LimitReader(fd, limit))
	return b, err == nil, err
}

// validTranscript reports whether path names an existing .jsonl file, so an
// entry cannot feed an arbitrary file into the store.
func validTranscript(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}
