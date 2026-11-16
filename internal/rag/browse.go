package rag

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Browsing the store: which collections a caller reads, and every chunk
// matching a filter, page by page, unranked. A ranked search stops at
// maxTopK; browsing reaches the end of what the filters match.

// onlyCollections keeps the targets whose collection is in names. An empty
// list keeps every target. It only narrows what the caller's spaces chose:
// a name outside targets is ignored, never added.
func onlyCollections(targets []spaceStore, names []string) []spaceStore {
	if len(names) == 0 {
		return targets
	}
	kept := targets[:0:0]
	for _, t := range targets {
		if slices.Contains(names, t.st.Collection()) {
			kept = append(kept, t)
		}
	}
	return kept
}

// CollectionInfo is one collection a caller reads.
type CollectionInfo struct {
	Name string
	// Space is the space the collection holds; "" for the base collection.
	Space string
	// Chunks counts the chunks a read of the collection sees: its space's
	// own, labelled under the read's revision, chat direct messages left
	// out, as a browse of it would total.
	Chunks int64
	// Hybrid reports a keyword index, so hybrid search can run on it.
	Hybrid bool
}

// ListCollectionsOpts chooses the collections listed.
type ListCollectionsOpts struct {
	// Spaces and LabelsV are as SearchOpts': nil lists every collection.
	Spaces  []string
	LabelsV int64
	// Hide names collections never listed, with every space collection
	// named after them: a migration's backup, which no read reaches.
	Hide []string
}

// ListCollections lists the collections a read for opts.Spaces reaches that
// exist, the base first and then by name, with what each holds. A
// migration's shadow (its target) and the collections opts.Hide names are
// never listed: they are not what a read of the store answers from.
func (s *Service) ListCollections(ctx context.Context, opts ListCollectionsOpts) ([]CollectionInfo, error) {
	targets, err := s.listStores(ctx, opts.Spaces)
	if err != nil {
		return nil, err
	}
	targets = withLabels(targets, opts.LabelsV)
	hide := slices.Clone(opts.Hide)
	if _, shadow := s.stores(); shadow != nil {
		hide = append(hide, shadow.Collection())
	}
	base := s.active().Collection()
	out := make([]CollectionInfo, 0, len(targets))
	for _, t := range targets {
		name := t.st.Collection()
		if hidden(name, base, hide) {
			continue
		}
		l, err := t.st.Layout(ctx)
		if err != nil {
			return nil, err
		}
		if l == LayoutUnknown {
			// Reason: a readable space whose collection was never made
			// holds nothing; listing it would offer an empty choice.
			continue
		}
		n, err := t.st.Count(ctx, buildFilter(t.must(nil), nil, nil, TimeRange{}))
		if err != nil {
			return nil, err
		}
		sp, _ := spaceOf(base, name)
		out = append(out, CollectionInfo{Name: name, Space: sp, Chunks: int64(n), Hybrid: l == LayoutHybrid})
	}
	slices.SortStableFunc(out, func(a, b CollectionInfo) int {
		switch {
		case a.Name == base:
			return -1
		case b.Name == base:
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// hidden reports whether name is one of hide, or a space collection named
// after one. The active base itself is never hidden: config naming it as
// its own backup is refused elsewhere, and hiding it would hide the store.
func hidden(name, base string, hide []string) bool {
	if name == base {
		return false
	}
	for _, h := range hide {
		if h == "" {
			continue
		}
		if name == h || strings.HasPrefix(name, h+spaceSep) {
			return true
		}
	}
	return false
}

// BrowseOpts filters a browse. The filters are a search's.
type BrowseOpts struct {
	Scope      string
	SourceType string
	SourceURI  string
	Tags       []string
	AllTags    []string
	Range      TimeRange
	// Spaces, LabelsV and Collections choose the collections read, as
	// SearchOpts'.
	Spaces      []string
	LabelsV     int64
	Collections []string
	// Limit is the page size, at most maxBrowse; 0 or less is
	// defaultBrowse.
	Limit int
	// Cursor continues from a previous page's next cursor; "" starts at
	// the beginning.
	Cursor string
}

// Browse page bounds.
const (
	defaultBrowse = 50
	maxBrowse     = 200
)

// ErrBadCursor refuses a browse cursor that is malformed or names a
// collection the browse does not read.
var ErrBadCursor = errors.New("the browse cursor is not valid for this listing")

// browseCursor is where a browse continues: a collection, and the id of the
// first point there still to return (Qdrant's scroll offset). It is sent to
// the client as base64url JSON. Reason: a name, not an index, so a cursor
// stays meaningful when a space's collection is created between pages.
type browseCursor struct {
	Collection string `json:"c"`
	Offset     string `json:"o,omitempty"`
}

func (c browseCursor) encode() string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (browseCursor, error) {
	var c browseCursor
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(raw, &c) != nil || c.Collection == "" {
		return browseCursor{}, ErrBadCursor
	}
	return c, nil
}

// Browse returns one page of the chunks matching opts, unranked, in storage
// order: the targets in order (the base first), and within each in point-id
// order. The next cursor continues the listing and is "" after the last
// page; a page may end in one collection and the next start in another.
// total counts every matching chunk, on the first page only (opts.Cursor
// ""), and is 0 on later pages.
//
// The reads are a search's: the caller's spaces choose the collections,
// each read with its space and labels revision as the second layer, and
// chat direct messages are always left out (SP-060, D-19). Hits carry no
// score and no vector.
//
// While a store move runs a source may be in two collections at once, and
// a browse lists both copies.
func (s *Service) Browse(ctx context.Context, opts BrowseOpts) (hits []Hit, next string, total int64, err error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultBrowse
	}
	limit = min(limit, maxBrowse)
	if len(opts.Tags) > 0 || len(opts.AllTags) > 0 || opts.SourceType != "" {
		s.ensureSearchIndexes()
	}
	targets, err := s.listStores(ctx, opts.Spaces)
	if err != nil {
		return nil, "", 0, err
	}
	targets = withLabels(onlyCollections(targets, opts.Collections), opts.LabelsV)
	must := map[string]string{"scope": opts.Scope, "source_type": opts.SourceType, "source_uri": opts.SourceURI}
	filterOf := func(t spaceStore) map[string]any {
		return buildFilter(t.must(must), opts.Tags, opts.AllTags, opts.Range)
	}
	start, offset := 0, ""
	if opts.Cursor != "" {
		c, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, "", 0, err
		}
		start = slices.IndexFunc(targets, func(t spaceStore) bool { return t.st.Collection() == c.Collection })
		if start < 0 {
			return nil, "", 0, ErrBadCursor
		}
		offset = c.Offset
	} else {
		for _, t := range targets {
			n, err := t.st.Count(ctx, filterOf(t))
			if err != nil {
				return nil, "", 0, err
			}
			total += int64(n)
		}
	}
	hits = make([]Hit, 0, limit)
	for i := start; i < len(targets); i++ {
		t := targets[i]
		var off any
		if i == start && offset != "" {
			off = offset
		}
		for page := 0; ; page++ {
			if page >= maxScrollPages {
				return nil, "", 0, fmt.Errorf("browse: exceeded %d pages; aborting scan", maxScrollPages)
			}
			// Reason: one point more than the page needs shows whether
			// anything follows, so the last page's cursor is "" rather than
			// one leading to an empty page. Once the page is full, that one
			// point is all a later collection is asked for.
			pts, nextOff, err := t.st.Scroll(ctx, filterOf(t), nil, limit-len(hits)+1, off)
			if err != nil {
				return nil, "", 0, err
			}
			take := min(len(pts), limit-len(hits))
			for _, p := range pts[:take] {
				hits = append(hits, toHit(SearchHit{ID: p.ID, Payload: p.Payload}, t.st.Collection(), 0, 0, nil))
			}
			if len(pts) > take {
				return hits, browseCursor{Collection: t.st.Collection(), Offset: pts[take].ID}.encode(), total, nil
			}
			if nextOff == nil {
				break
			}
			if off != nil && fmt.Sprint(nextOff) == fmt.Sprint(off) {
				return nil, "", 0, fmt.Errorf("browse: scroll cursor did not advance")
			}
			off = nextOff
		}
	}
	return hits, "", total, nil
}
