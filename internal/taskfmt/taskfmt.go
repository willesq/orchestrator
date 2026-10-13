// Package taskfmt renders queue task outcomes for the CLI and the MCP server,
// so both surfaces describe a run the same way.
package taskfmt

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// Outcome renders a task's route and, once it has finished, its metrics as
// aligned lines; "" for an unrouted task with no metrics yet.
func Outcome(t *orchdv1.Task) string {
	var b strings.Builder
	// The route is known from dispatch, so it shows before the task finishes.
	if r := t.GetRoute(); r != nil {
		fmt.Fprintf(&b, "route:   %s\n", Route(r))
	}
	if t.GetDurationMs() == 0 && t.GetCostUsd() == 0 && t.GetTurns() == 0 {
		return b.String()
	}
	parts := []string{fmt.Sprintf("$%.4f", t.GetCostUsd()), Duration(t.GetDurationMs())}
	if n := t.GetTurns(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d turn(s)", n))
	}
	fmt.Fprintf(&b, "outcome: %s\n", strings.Join(parts, " · "))
	if m := t.GetModels(); len(m) > 0 {
		fmt.Fprintf(&b, "models:  %s\n", strings.Join(m, ", "))
	}
	if s := t.GetSessionId(); s != "" {
		fmt.Fprintf(&b, "session: %s\n", s)
	}
	if p := t.GetPrs(); len(p) > 0 {
		fmt.Fprintf(&b, "prs:     %s\n", strings.Join(p, ", "))
	}
	if v := t.GetVerification(); v != "" {
		fmt.Fprintf(&b, "verify:  %s\n", v)
	}
	return b.String()
}

// Route renders a route as "kind → provider/model@effort (rationale)".
func Route(r *orchdv1.Route) string {
	target := "agent default"
	if r.GetModel() != "" {
		target = r.GetProvider() + "/" + r.GetModel()
		if e := r.GetEffort(); e != "" {
			target += "@" + e
		}
	}
	s := target
	if k := r.GetTaskKind(); k != "" {
		s = k + " → " + target
	}
	var notes []string
	if c := r.GetConfidence(); c != "" {
		notes = append(notes, fmt.Sprintf("confidence %s, %d outcome(s)", c, r.GetEvidenceN()))
	}
	if why := r.GetRationale(); why != "" {
		notes = append(notes, why)
	}
	if len(notes) > 0 {
		s += " (" + strings.Join(notes, "; ") + ")"
	}
	return s
}

// Duration renders milliseconds the way a person reads a run's length.
func Duration(ms int64) string {
	return (time.Duration(ms) * time.Millisecond).Round(100 * time.Millisecond).String()
}

// AgentStats aggregates the outcomes of one agent's tasks.
type AgentStats struct {
	Agent                     string
	Total                     int
	Complete, Failed, Blocked int
	Open                      int // pending or in progress
	CostUSD                   float64
	finishedMS, finishedTimed int64
}

// SuccessRate is complete over finished tasks, or 0 with none finished.
func (a AgentStats) SuccessRate() float64 {
	if done := a.Complete + a.Failed + a.Blocked; done > 0 {
		return float64(a.Complete) / float64(done)
	}
	return 0
}

// AvgDurationMS is the mean run length of finished tasks that reported one.
func (a AgentStats) AvgDurationMS() int64 {
	if a.finishedTimed == 0 {
		return 0
	}
	return a.finishedMS / a.finishedTimed
}

// Stats groups tasks by agent, busiest first.
func Stats(tasks []*orchdv1.Task) []AgentStats {
	by := map[string]*AgentStats{}
	for _, t := range tasks {
		a := by[t.GetAgent()]
		if a == nil {
			a = &AgentStats{Agent: t.GetAgent()}
			by[t.GetAgent()] = a
		}
		a.Total++
		a.CostUSD += t.GetCostUsd()
		switch t.GetStatus() {
		case "complete":
			a.Complete++
		case "failed":
			a.Failed++
		case "blocked":
			a.Blocked++
		default:
			a.Open++
			continue
		}
		if d := t.GetDurationMs(); d > 0 {
			a.finishedMS += d
			a.finishedTimed++
		}
	}
	out := make([]AgentStats, 0, len(by))
	for _, a := range by {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Agent < out[j].Agent
	})
	return out
}

// StatsTable renders Stats as an aligned table with a totals row.
func StatsTable(stats []AgentStats) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tTASKS\tDONE\tFAILED\tBLOCKED\tOPEN\tSUCCESS\tAVG TIME\tCOST")
	var total AgentStats
	for _, a := range stats {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\t%.0f%%\t%s\t$%.2f\n", a.Agent, a.Total, a.Complete, a.Failed,
			a.Blocked, a.Open, 100*a.SuccessRate(), Duration(a.AvgDurationMS()), a.CostUSD)
		total.Total += a.Total
		total.Complete += a.Complete
		total.Failed += a.Failed
		total.Blocked += a.Blocked
		total.Open += a.Open
		total.CostUSD += a.CostUSD
		total.finishedMS += a.finishedMS
		total.finishedTimed += a.finishedTimed
	}
	fmt.Fprintf(w, "all\t%d\t%d\t%d\t%d\t%d\t%.0f%%\t%s\t$%.2f\n", total.Total, total.Complete, total.Failed,
		total.Blocked, total.Open, 100*total.SuccessRate(), Duration(total.AvgDurationMS()), total.CostUSD)
	_ = w.Flush()
	return b.String()
}
