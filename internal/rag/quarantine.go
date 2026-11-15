package rag

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/willesq/orchestrator/internal/spaces"
)

// ErrNotQuarantined is returned when a purge names a source quarantine does
// not hold. It never says where else the source is, if anywhere.
var ErrNotQuarantined = errors.New("not in quarantine")

// QuarantinedSource is one source held in the quarantine collection.
type QuarantinedSource struct {
	SourceURI  string
	SourceType string
	Chunks     int
	IngestedAt int64 // unix seconds of its latest store; 0 when unknown
}

// quarantineStore is the quarantine collection of the active store. A chunk
// is in quarantine when it sits there (D-10); stale chunks a relabel has not
// moved yet are not counted until it does.
func (s *Service) quarantineStore() *QdrantStore {
	return s.derive(s.active(), spaces.Quarantine)
}

// ListQuarantine lists every source in the quarantine collection, by URI,
// and the total chunks across them. A missing collection holds nothing.
func (s *Service) ListQuarantine(ctx context.Context) ([]QuarantinedSource, int, error) {
	st := s.quarantineStore()
	fields := []string{"source_uri", "source_type", fieldIngestedAt}
	agg := map[string]*QuarantinedSource{}
	total := 0
	var offset any
	for page := 0; ; page++ {
		if page >= maxScrollPages {
			return nil, 0, fmt.Errorf("list quarantine: exceeded %d pages; aborting scan", maxScrollPages)
		}
		pts, next, err := st.Scroll(ctx, nil, fields, 1000, offset)
		if err != nil {
			return nil, 0, err
		}
		for _, p := range pts {
			uri := str(p.Payload["source_uri"])
			if uri == "" {
				continue
			}
			total++
			cur, ok := agg[uri]
			if !ok {
				cur = &QuarantinedSource{SourceURI: uri, SourceType: str(p.Payload["source_type"])}
				agg[uri] = cur
			}
			cur.Chunks++
			if t := unixTime(p.Payload[fieldIngestedAt]); !t.IsZero() && t.Unix() > cur.IngestedAt {
				cur.IngestedAt = t.Unix()
			}
		}
		if next == nil {
			break
		}
		if offset != nil && fmt.Sprint(next) == fmt.Sprint(offset) {
			return nil, 0, fmt.Errorf("list quarantine: scroll cursor did not advance")
		}
		offset = next
	}
	out := make([]QuarantinedSource, 0, len(agg))
	for _, v := range agg {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceURI < out[j].SourceURI })
	return out, total, nil
}

// PurgeResult is what a quarantine purge deleted, or would delete.
type PurgeResult struct {
	Sources, Chunks int
	Purged          bool
}

// PurgeQuarantine deletes the named sources, or with all every source, from
// the quarantine collection and, during a migration, its shadow. Nothing
// outside quarantine is touched: a named source quarantine does not hold
// refuses the whole purge with ErrNotQuarantined, before anything is
// deleted. Without apply it only counts.
func (s *Service) PurgeQuarantine(ctx context.Context, uris []string, all, apply bool) (PurgeResult, error) {
	if all == (len(uris) > 0) {
		return PurgeResult{}, errors.New("name the sources to purge, or purge all; not both and not neither")
	}
	held, _, err := s.ListQuarantine(ctx)
	if err != nil {
		return PurgeResult{}, err
	}
	chunks := make(map[string]int, len(held))
	for _, h := range held {
		chunks[h.SourceURI] = h.Chunks
	}
	targets := uris
	if all {
		targets = make([]string, 0, len(held))
		for _, h := range held {
			targets = append(targets, h.SourceURI)
		}
	}
	seen := map[string]bool{}
	var res PurgeResult
	var picked []string
	for _, uri := range targets {
		if seen[uri] {
			continue
		}
		seen[uri] = true
		n, ok := chunks[uri]
		if !ok {
			return PurgeResult{}, fmt.Errorf("%q: %w", uri, ErrNotQuarantined)
		}
		res.Sources++
		res.Chunks += n
		picked = append(picked, uri)
	}
	if !apply {
		return res, nil
	}
	res.Chunks = 0
	for _, uri := range picked {
		n, err := s.purgeQuarantined(ctx, uri)
		res.Chunks += n
		if err != nil {
			return res, err
		}
	}
	res.Purged = true
	return res, nil
}

// purgeQuarantined deletes uri's chunks from the quarantine collection and
// its shadow, under the source's write lock so a concurrent ingest cannot
// interleave.
func (s *Service) purgeQuarantined(ctx context.Context, uri string) (int, error) {
	defer s.lockSource(uri)()
	active, shadow := s.stores()
	key := sourceKey(spaces.Quarantine, uri)
	n, err := deleteSource(ctx, s.derive(active, spaces.Quarantine), uri)
	if err != nil {
		if shadow != nil {
			s.markDirty(key)
		}
		return 0, err
	}
	if shadow != nil {
		_, err := deleteSource(ctx, s.derive(shadow, spaces.Quarantine), uri)
		s.shadowResult(key, err)
	}
	return n, nil
}
