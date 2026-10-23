package prs

import (
	"slices"
	"sort"
)

// Why a PR is in the cache: which discovery search found it.
const (
	ReasonAuthored        = "authored"
	ReasonReviewRequested = "review_requested"
)

// Why a PR needs the user now, in priority order.
const (
	AttentionCIFailed         = "ci_failed"
	AttentionChangesRequested = "changes_requested"
	AttentionConflicts        = "conflicts"
	AttentionReviewRequested  = "review_requested"
	AttentionReadyToMerge     = "ready_to_merge"
)

var attentionRank = map[string]int{
	AttentionCIFailed:         0,
	AttentionChangesRequested: 1,
	AttentionConflicts:        2,
	AttentionReviewRequested:  3,
	AttentionReadyToMerge:     4,
}

// Attention says why pr needs the user, or "" when it does not. Problems on
// the user's own PRs come first, then reviews other people are waiting on,
// then the user's PRs that only need merging. Drafts never need attention:
// they are still being worked on, so flagging them is noise.
func Attention(pr PR) string {
	if pr.IsDraft {
		return ""
	}
	authored := pr.Authored()
	switch {
	case authored && pr.CIState == "failure":
		return AttentionCIFailed
	case authored && pr.ReviewDecision == "CHANGES_REQUESTED":
		return AttentionChangesRequested
	case authored && pr.Mergeable == "CONFLICTING":
		return AttentionConflicts
	case slices.Contains(pr.Reasons, ReasonReviewRequested):
		return AttentionReviewRequested
	case authored && isReady(pr):
		return AttentionReadyToMerge
	}
	return ""
}

// Authored reports whether the user opened pr. Cache entries written before
// reasons were recorded came only from the authored search.
func (pr PR) Authored() bool {
	return len(pr.Reasons) == 0 || slices.Contains(pr.Reasons, ReasonAuthored)
}

// Ordered returns every cached PR: those needing attention first, most urgent
// first, then the rest; ties break by repo and title so the order is stable
// between sweeps.
func Ordered(s State) []PR {
	out := make([]PR, 0, len(s.PRs))
	for _, pr := range s.PRs {
		out = append(out, pr)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri < rj
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// NeedsMe returns the PRs needing attention, in Ordered's order.
func NeedsMe(s State) []PR {
	var out []PR
	for _, pr := range Ordered(s) {
		if Attention(pr) != "" {
			out = append(out, pr)
		}
	}
	return out
}

func rank(pr PR) int {
	if r, ok := attentionRank[Attention(pr)]; ok {
		return r
	}
	return len(attentionRank)
}
