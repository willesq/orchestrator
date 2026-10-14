package rag

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// MigrateReport describes a migration to a hybrid collection, planned or done.
type MigrateReport struct {
	From, To  string
	Sources   int // distinct sources in the old collection
	OldChunks int
	NewChunks int // chunks the sources make with today's chunking
	// Skipped counts sources not copied: no stored text, too large, or written
	// through to the new collection while the copy ran (so it holds them, newer).
	Skipped int
	// Malformed counts sources whose stored chunk order could not be trusted
	// (missing or repeated indices); their text is kept whole, in point-id
	// order, which may not be the original order.
	Malformed int
	// Orphans counts chunks with no source URI, which cannot be migrated.
	Orphans int
	// Removed counts sources the target held that no longer exist (left by an
	// interrupted earlier run).
	Removed int
	// Oversize counts sources too large to migrate. An applied run refuses to
	// switch while any remain, since searches would lose them.
	Oversize int
	// Problems names some sources that were skipped or malformed, and why.
	Problems []string
	Applied  bool
}

// Commit records a finished migration durably, before searches switch to it.
// If it fails, the switch does not happen.
type Commit func(from, to string) error

// collectionName is a safe Qdrant collection name.
var collectionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidCollectionName reports whether name is a safe collection name.
func ValidCollectionName(name string) bool { return collectionName.MatchString(name) }

// maxSourceBytes caps one source's rebuilt text. Stored text is untrusted, and
// one enormous source would otherwise hold the migration's memory and its
// embedding time.
var maxSourceBytes = 16 << 20

// maxProblems bounds how many sources the report names.
const maxProblems = 20

func (r *MigrateReport) problem(format string, args ...any) {
	if len(r.Problems) < maxProblems {
		r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
	}
}

// MigrateHybrid copies the active collection into a new hybrid one (dense
// plus BM25 sparse vectors), re-chunking each source's stored text with
// today's token-sized chunker and re-embedding it. Without apply it only plans:
// it reads and re-chunks, and writes nothing.
//
// With apply:
//   - The new collection is added as a shadow that every write also reaches,
//     after in-flight writes drain, and only then are sources listed, so no
//     write falls between the two.
//   - Each source is copied from the active store as it is at that moment,
//     under the same per-source lock live writes take, so the two never
//     interleave. A source a live write has already fully written to the
//     shadow is left as it is.
//   - Sources the target holds that no longer exist (an interrupted earlier
//     run) are removed.
//   - commit records the switch; only if it succeeds does the new collection
//     become active. The old collection is never modified.
//
// A marker in StateDir tells an interrupted run, which can be resumed, from a
// finished one, whose target is refused: re-copying into it would revert any
// writes made since it went live.
func (s *Service) MigrateHybrid(ctx context.Context, target string, apply bool, commit Commit) (MigrateReport, error) {
	src := s.active()
	if target == "" {
		target = src.Collection() + "-hybrid"
	}
	rep := MigrateReport{From: src.Collection(), To: target}
	if !ValidCollectionName(target) {
		return rep, fmt.Errorf("collection name %q must be letters, digits, '_' or '-'", target)
	}
	if target == src.Collection() {
		return rep, errors.New("the target must differ from the active collection")
	}
	switch l, err := src.Layout(ctx); {
	case err != nil:
		return rep, err
	case l == LayoutUnknown:
		return rep, fmt.Errorf("collection %q does not exist; nothing to migrate", src.Collection())
	case l == LayoutHybrid:
		return rep, fmt.Errorf("collection %q is already hybrid", src.Collection())
	}
	dst := src.WithCollection(target)
	dstLayout, err := dst.Layout(ctx)
	if err != nil {
		return rep, err
	}
	if dstLayout == LayoutDense {
		return rep, fmt.Errorf("collection %q already exists and is not hybrid; pick another target", target)
	}
	marker := s.markerPath(target)
	if dstLayout == LayoutHybrid && apply {
		if marker == "" {
			return rep, errors.New("no state directory for migration markers")
		}
		n, err := dst.Count(ctx, nil)
		if err != nil {
			return rep, err
		}
		if _, statErr := os.Stat(marker); n > 0 && statErr != nil {
			return rep, fmt.Errorf("collection %q holds a finished migration; copying into it again would revert "+
				"writes made since, so pick another --target", target)
		}
	}

	if !apply {
		uris, err := s.listURIs(ctx, src, &rep)
		if err != nil {
			return rep, err
		}
		for _, uri := range uris {
			sr, err := s.readSource(ctx, src, uri)
			if err != nil {
				return rep, err
			}
			rep.OldChunks += sr.chunks
			s.tally(&rep, uri, sr, func(text string) { rep.NewChunks += len(s.split(text)) })
		}
		return rep, nil
	}

	if marker == "" {
		return rep, errors.New("no state directory for migration markers")
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return rep, err
	}
	if err := os.WriteFile(marker, []byte(src.Collection()+"\n"), 0o600); err != nil {
		return rep, err
	}
	dim, err := s.emb.Dimension(ctx)
	if err != nil {
		return rep, err
	}
	if err := dst.EnsureCollection(ctx, dim); err != nil {
		return rep, err
	}

	s.setShadow(dst)
	switched := false
	defer func() {
		if !switched {
			s.setShadow(nil)
		}
	}()

	uris, err := s.listURIs(ctx, src, &rep)
	if err != nil {
		return rep, err
	}
	live := make(map[string]bool, len(uris))
	for _, uri := range uris {
		live[uri] = true
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if err := s.copySource(ctx, src, dst, dim, uri, &rep); err != nil {
			return rep, fmt.Errorf("%s: %w", uri, err)
		}
	}
	if err := s.removeStale(ctx, src, dst, live, &rep); err != nil {
		return rep, err
	}
	if rep.Oversize > 0 {
		return rep, fmt.Errorf("%d source(s) are too large to migrate (see the report); forget or re-ingest them, then run again", rep.Oversize)
	}

	// Re-copy what writes left inconsistent, then switch. The last check is
	// made with every writer drained, so an empty dirty set is final.
	for attempt := 0; ; attempt++ {
		for _, uri := range s.takeDirty() {
			if err := s.copySource(ctx, src, dst, dim, uri, &rep); err != nil {
				return rep, fmt.Errorf("%s: %w", uri, err)
			}
		}
		s.gate.Lock()
		if s.dirtyCount() == 0 {
			break
		}
		s.gate.Unlock()
		if attempt >= maxReconcile {
			return rep, fmt.Errorf("writes kept failing against %s; not switching (run again)", target)
		}
	}
	// Remove the marker before recording the switch: a finished target must
	// never look resumable, or a later run could copy over writes made since.
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.gate.Unlock()
		return rep, fmt.Errorf("could not clear the migration marker, so not switching: %w", err)
	}
	if commit != nil {
		if err := commit(src.Collection(), target); err != nil {
			_ = os.WriteFile(marker, []byte(src.Collection()+"\n"), 0o600)
			s.gate.Unlock()
			return rep, fmt.Errorf("copied, but recording the switch failed, so searches stay on %s: %w", src.Collection(), err)
		}
	}
	s.mu.Lock()
	s.store, s.shadow, s.touched, s.dirty = dst, nil, nil, nil
	s.mu.Unlock()
	s.gate.Unlock()
	switched, rep.Applied = true, true
	return rep, nil
}

// maxReconcile bounds how often failing writes are re-copied before the
// migration gives up rather than switch with them unresolved.
const maxReconcile = 5

func (s *Service) markerPath(target string) string {
	if s.StateDir == "" {
		return ""
	}
	return filepath.Join(s.StateDir, "hybrid-migration-"+target+".inprogress")
}

// setShadow adds or removes the store writes also reach, after in-flight
// writes finish, so none straddles the change.
func (s *Service) setShadow(dst *QdrantStore) {
	s.gate.Lock()
	defer s.gate.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shadow = dst
	if dst != nil {
		s.touched, s.dirty = map[string]bool{}, map[string]bool{}
	} else {
		s.touched, s.dirty = nil, nil
	}
}

// tally decides whether a rebuilt source can be migrated, counting why not.
func (s *Service) tally(rep *MigrateReport, uri string, sr sourceRead, ok func(string)) {
	switch {
	case sr.oversize:
		rep.Oversize++
		rep.problem("%s: skipped, its text exceeds the %d MiB limit", uri, maxSourceBytes>>20)
	case strings.TrimSpace(sr.text) == "":
		rep.Skipped++
	default:
		if sr.malformed {
			rep.Malformed++
			rep.problem("%s: chunk indices missing or repeated; kept whole, in point-id order", uri)
		}
		ok(sr.text)
	}
}

// copySource copies one source from src to dst as src holds it now, under the
// source's write lock.
func (s *Service) copySource(ctx context.Context, src, dst *QdrantStore, dim int, uri string, rep *MigrateReport) error {
	defer s.lockSource(uri)()
	s.mu.RLock()
	done := s.touched[uri]
	s.mu.RUnlock()
	if done {
		rep.Skipped++
		return nil
	}
	sr, err := s.readSource(ctx, src, uri)
	if err != nil {
		return err
	}
	rep.OldChunks += sr.chunks
	if sr.chunks == 0 {
		// Deleted since it was listed: make sure the target agrees.
		_, err := deleteSource(ctx, dst, uri)
		return err
	}
	var werr error
	s.tally(rep, uri, sr, func(text string) {
		points, _, err := s.prepare(ctx, text, sr.meta)
		if err != nil {
			werr = err
			return
		}
		if werr = writePoints(ctx, dst, dim, uri, points); werr == nil {
			rep.NewChunks += len(points)
		}
	})
	return werr
}

// removeStale deletes sources the target holds that the active store no
// longer does, and that no live write put there.
func (s *Service) removeStale(ctx context.Context, src, dst *QdrantStore, live map[string]bool, rep *MigrateReport) error {
	var ignored MigrateReport
	held, err := s.listURIs(ctx, dst, &ignored)
	if err != nil {
		return err
	}
	for _, uri := range held {
		if live[uri] {
			continue
		}
		if err := func() error {
			defer s.lockSource(uri)()
			s.mu.RLock()
			done := s.touched[uri]
			s.mu.RUnlock()
			if done {
				return nil
			}
			if n, err := src.Count(ctx, EqFilter("source_uri", uri)); err != nil || n > 0 {
				return err
			}
			n, err := deleteSource(ctx, dst, uri)
			if n > 0 {
				rep.Removed++
			}
			return err
		}(); err != nil {
			return err
		}
	}
	return nil
}

// listURIs returns every source URI in st, counting chunks without one. Only
// URIs are held in memory; each source's text is read when it is copied.
func (s *Service) listURIs(ctx context.Context, st *QdrantStore, rep *MigrateReport) ([]string, error) {
	seen := map[string]bool{}
	var offset any
	for page := 0; page < maxScrollPages; page++ {
		pts, next, err := st.Scroll(ctx, nil, []string{"source_uri"}, 1000, offset)
		if err != nil {
			return nil, err
		}
		for _, p := range pts {
			if uri := str(p.Payload["source_uri"]); uri != "" {
				seen[uri] = true
			} else {
				rep.Orphans++
			}
		}
		if next == nil {
			uris := make([]string, 0, len(seen))
			for u := range seen {
				uris = append(uris, u)
			}
			sort.Strings(uris)
			rep.Sources = len(uris)
			return uris, nil
		}
		offset = next
	}
	return nil, errors.New("collection too large to migrate in one pass")
}

// sourceRead is one source as read back from a collection.
type sourceRead struct {
	text      string
	meta      Meta
	chunks    int
	malformed bool
	oversize  bool // its text passed maxSourceBytes; text is empty
}

// readSource reads one source's chunks and rebuilds its text. Reading stops
// once the text passes maxSourceBytes, so an enormous source is reported
// rather than held.
func (s *Service) readSource(ctx context.Context, st *QdrantStore, uri string) (sourceRead, error) {
	fields := []string{"text", "source_uri", "source_type", "scope", "tags", "chunk_index"}
	sr := sourceRead{meta: Meta{SourceURI: uri}}
	var chunks []storedChunk
	var offset any
	size := 0
	for page := 0; page < maxScrollPages; page++ {
		pts, next, err := st.Scroll(ctx, EqFilter("source_uri", uri), fields, 1000, offset)
		if err != nil {
			return sr, err
		}
		for _, p := range pts {
			if len(chunks) == 0 {
				sr.meta.SourceType, sr.meta.Scope, sr.meta.Tags = str(p.Payload["source_type"]), str(p.Payload["scope"]), strSlice(p.Payload["tags"])
			}
			c := storedChunk{id: p.ID, index: -1, text: str(p.Payload["text"])}
			if f, ok := p.Payload["chunk_index"].(float64); ok && f >= 0 && f == float64(int(f)) {
				c.index = int(f)
			}
			size += len(c.text)
			chunks = append(chunks, c)
		}
		sr.chunks = len(chunks)
		if size > maxSourceBytes {
			sr.oversize = true
			return sr, nil
		}
		if next == nil {
			break
		}
		offset = next
	}
	sr.text, sr.malformed = rebuild(chunks)
	return sr, nil
}

// storedChunk is one chunk read back from a collection.
type storedChunk struct {
	id    string
	index int // -1 when missing or not a whole number
	text  string
}

// storedContextLine matches the heading-path line section chunks start with.
var storedContextLine = regexp.MustCompile(`^\[section: [^\n]*\]\n\n`)

// atxLine matches a markdown heading line.
var atxLine = regexp.MustCompile(`^#{1,6}[ \t]`)

// minOverlap is the fewest shared words treated as an overlap between
// neighbouring chunks rather than coincidence.
const minOverlap = 8

// rebuild reassembles a source's text from its stored chunks, in order:
//   - the heading-path line section chunks carry is dropped (the chunker adds
//     it back);
//   - a heading line repeated at the top of each piece of a split section is
//     kept once;
//   - the words a chunk shares with the one before it are cut by position in
//     the original text, so line breaks and code survive.
//
// When the stored order cannot be trusted (an index missing or repeated), the
// chunks are joined in point-id order without removing overlaps, and
// malformed is true: better duplicated or reordered text than lost text.
func rebuild(chunks []storedChunk) (text string, malformed bool) {
	seen := map[int]bool{}
	for _, c := range chunks {
		if c.index < 0 || seen[c.index] {
			malformed = true
		}
		seen[c.index] = true
	}
	sort.SliceStable(chunks, func(i, j int) bool {
		if chunks[i].index != chunks[j].index {
			return chunks[i].index < chunks[j].index
		}
		return chunks[i].id < chunks[j].id
	})
	var parts []string
	var prevWords []string
	prevHeading, prevContext := "", ""
	for _, c := range chunks {
		ctxLine := storedContextLine.FindString(c.text)
		t := c.text[len(ctxLine):]
		if !malformed {
			// A heading repeated at the top of the next piece of the same
			// section (same heading, same path above it) is kept once; two
			// sections that merely share a title are not merged.
			first, rest, _ := strings.Cut(t, "\n")
			if atxLine.MatchString(first) {
				if first == prevHeading && ctxLine == prevContext {
					t = strings.TrimLeft(rest, "\n")
				}
				prevHeading, prevContext = first, ctxLine
			}
			if k := overlap(prevWords, strings.Fields(t)); k >= minOverlap {
				t = cutWords(t, k)
			}
		}
		prevWords = strings.Fields(c.text)
		if strings.TrimSpace(t) != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n"), malformed
}

// cutWords drops the first k whitespace-separated words of t, keeping the
// formatting of what follows.
func cutWords(t string, k int) string {
	inWord, n := false, 0
	for i, r := range t {
		switch {
		case unicode.IsSpace(r):
			if inWord {
				inWord = false
				n++
				if n == k {
					return strings.TrimLeft(t[i:], " \t\n")
				}
			}
		default:
			inWord = true
		}
	}
	return ""
}

// overlap is the longest run of words that ends prev and starts cur, at most
// a little past the old chunker's 60.
func overlap(prev, cur []string) int {
	for k := min(len(prev), len(cur), 100); k > 0; k-- {
		match := true
		for i := 0; i < k; i++ {
			if prev[len(prev)-k+i] != cur[i] {
				match = false
				break
			}
		}
		if match {
			return k
		}
	}
	return 0
}
