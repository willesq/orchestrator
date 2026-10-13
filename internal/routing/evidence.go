package routing

import (
	"context"
	"fmt"
)

// Tally counts observed outcomes for one model on one task kind.
type Tally struct{ Good, Rework, Bad int }

// N is how many outcomes the tally holds.
func (t Tally) N() int { return t.Good + t.Rework + t.Bad }

// Score is the observed success rate in [0, 1]; rework counts half.
func (t Tally) Score() float64 {
	if t.N() == 0 {
		return 0
	}
	return (float64(t.Good) + 0.5*float64(t.Rework)) / float64(t.N())
}

func (t Tally) String() string {
	return fmt.Sprintf("%d good, %d rework, %d bad", t.Good, t.Rework, t.Bad)
}

// Evidence maps "kind provider/model" to its tally.
type Evidence map[string]Tally

func evidenceKey(kind, provider, model string) string { return kind + " " + provider + "/" + model }

// For returns the tally for one kind and model.
func (e Evidence) For(kind, provider, model string) Tally {
	return e[evidenceKey(kind, provider, model)]
}

// Evidence tallies the ledger's routed tasks. Each task contributes its best
// label: the user's rating, else the outcome of its PRs, else a failure. A task
// that merely completed carries no verdict and is not counted; turns and
// duration are left out entirely so speed never stands in for quality.
func (s *Store) Evidence(ctx context.Context) (Evidence, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT task_kind, provider, model,
		       CASE
		         WHEN COALESCE(rating,'') <> '' THEN rating
		         WHEN COALESCE(pr_outcome,'') <> '' THEN pr_outcome
		         WHEN status = 'failed' THEN 'bad'
		         ELSE ''
		       END AS label,
		       COUNT(*)
		FROM tasks
		WHERE COALESCE(task_kind,'') <> '' AND COALESCE(model,'') <> ''
		GROUP BY task_kind, provider, model, label`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ev := Evidence{}
	for rows.Next() {
		var kind, provider, model, label string
		var n int
		if err := rows.Scan(&kind, &provider, &model, &label, &n); err != nil {
			return nil, err
		}
		k := evidenceKey(kind, provider, model)
		t := ev[k]
		switch label {
		case "good":
			t.Good += n
		case "rework":
			t.Rework += n
		case "bad":
			t.Bad += n
		default:
			continue
		}
		ev[k] = t
	}
	return ev, rows.Err()
}
