package daemon

import (
	"log"
	"sync"
	"time"
)

// ghBudget caps the GitHub API calls each isolated space's own gh account
// makes an hour (SP-074). Every call orchd makes as the account counts; once
// the cap is reached that account's docs sync, PR watch and PR outcome calls
// wait for the next hour, while reviews and lookups, which the user is
// waiting on, still run up to twice the cap (hardCapFactor). Other accounts,
// the default login included, are not counted.
type ghBudget struct {
	// limit is the calls an hour; 0 is no cap.
	limit int
	now   func() time.Time

	mu   sync.Mutex
	used map[string]*budgetWindow
}

// budgetWindow is one account's count in the hour that started at start.
type budgetWindow struct {
	start  time.Time
	calls  int
	logged bool
}

func newGHBudget(limit int, now func() time.Time) *ghBudget {
	return &ghBudget{limit: max(limit, 0), now: now, used: map[string]*budgetWindow{}}
}

// window returns dir's current hour, starting a new one when the last ended.
// The caller holds b.mu.
func (b *ghBudget) window(dir string) *budgetWindow {
	now := b.now()
	w := b.used[dir]
	if w == nil || now.Sub(w.start) >= time.Hour {
		w = &budgetWindow{start: now}
		b.used[dir] = w
	}
	return w
}

// hardCapFactor bounds the calls that are not deferred (reviews, lookups):
// past this many times the budget, they are refused too, so nothing can
// spend an account's whole GitHub rate limit.
const hardCapFactor = 2

// spend counts one call as account dir.
func (b *ghBudget) spend(dir string) {
	if b == nil || b.limit == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.window(dir).calls++
}

// take counts one call as account dir unless the account has reached cap
// times its budget this hour, checking and counting under one lock so
// concurrent calls cannot pass the cap together. It reports whether the
// call may run.
func (b *ghBudget) take(dir string, cap int) bool {
	if b == nil || b.limit == 0 {
		return true
	}
	b.mu.Lock()
	w := b.window(dir)
	ok := w.calls < cap*b.limit
	if ok {
		w.calls++
	}
	b.mu.Unlock()
	if !ok {
		b.spent(dir) // logs the first refusal of the hour
	}
	return ok
}

// spent reports whether account dir has used its hour's budget, logging the
// first time each hour. Reason: tasks can read orchd's log, so the line names
// neither the space nor the directory.
func (b *ghBudget) spent(dir string) bool {
	if b == nil || b.limit == 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.window(dir)
	if w.calls < b.limit {
		return false
	}
	if !w.logged {
		w.logged = true
		log.Printf("github: an isolated space's gh account used its %d GitHub API calls for this hour; its docs sync and PR watching wait until %s",
			b.limit, w.start.Add(time.Hour).Format(time.Kitchen))
	}
	return true
}
