package rag

import "context"

// What the space collections hold, for orch status (SP-031), and which
// collections a search reads, for orch rag why. Both name spaces: orchd
// narrows them to what each caller may see before answering.

// CollectionCount is how many chunks one collection holds, and how many of
// them a relabel to the applied revision would still change.
type CollectionCount struct {
	// Space is the space the collection holds: "" for the base collection
	// (unplaced content), Quarantine for quarantine.
	Space string
	// Chunks counts every chunk; Stale those not labelled by the revision
	// counted against (0 when none was given).
	Chunks, Stale int
}

// CountSpaces counts every collection under the active one, the base first.
// rev is the applied policy revision: a chunk not labelled by it is stale
// (INV-08), as Relabel sees it; with rev 0 nothing is counted as stale.
func (s *Service) CountSpaces(ctx context.Context, rev int64) ([]CollectionCount, error) {
	base := s.active()
	all, err := s.allStores(ctx, base, false)
	if err != nil {
		return nil, err
	}
	out := make([]CollectionCount, 0, len(all))
	for _, ss := range all {
		sp, _ := spaceOf(base.Collection(), ss.st.Collection())
		c := CollectionCount{Space: sp}
		if c.Chunks, err = ss.st.Count(ctx, nil); err != nil {
			return nil, err
		}
		if rev > 0 && c.Chunks > 0 {
			if c.Stale, err = ss.st.Count(ctx, staleFilterFor(rev)); err != nil {
				return nil, err
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// SearchCollections names the collections a search for the given spaces
// reads, as Search chooses them: nil reads every collection, an empty list
// none.
func (s *Service) SearchCollections(ctx context.Context, names []string) ([]string, error) {
	targets, err := s.readStores(ctx, names)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.st.Collection())
	}
	return out, nil
}
