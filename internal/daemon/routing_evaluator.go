package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/models"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/routing"
	"github.com/willesq/orchestrator/internal/supervisor"
)

// UnitRoutingEvaluator re-runs model discovery and routing evaluation weekly.
const UnitRoutingEvaluator = "routing-evaluator"

// routingEventStream is where the evaluator reports; the SessionStart digest
// reads it.
const routingEventStream = "routing"

// routingEvaluatorCheck is how often the unit looks whether a run is due. The
// cadence itself (default weekly) is kept in the evaluator's state file, so a
// restart of orchd neither skips nor repeats a run.
var routingEvaluatorCheck = time.Hour

// discoverer runs model discovery and reports which refs are new. The Claude
// CLI is resolved the way the worker resolves it, since orchd's launchd PATH
// would otherwise miss common install locations.
func discoverer(store *sql.DB, claudeBin string) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		reg := models.NewRegistry(store)
		before, err := reg.Models(ctx, models.Filter{})
		if err != nil {
			return nil, err
		}
		known := map[string]bool{}
		for _, m := range before {
			known[m.Ref()] = true
		}
		refreshErr := models.Refresh(ctx, reg, home, models.Adapters(agentrun.FindClaude(claudeBin, home)))
		after, err := reg.Models(ctx, models.Filter{})
		if err != nil {
			return nil, err
		}
		var fresh []string
		for _, m := range after {
			if !known[m.Ref()] && m.State == models.StateDiscovered {
				fresh = append(fresh, m.Ref())
			}
		}
		return fresh, refreshErr
	}
}

// routingEvaluatorUnit builds the unit, or reports why it cannot run.
func routingEvaluatorUnit(s config.Settings, store *sql.DB) (supervisor.Unit, bool) {
	if !s.Routing.Evaluate.Enabled || store == nil {
		return supervisor.Unit{}, false
	}
	policy := routing.PolicyFrom(s.Routing)
	if err := policy.Validate(); err != nil {
		log.Printf("orchd: routing evaluator off: %v", err)
		return supervisor.Unit{}, false
	}
	every := interval(s.Routing.Evaluate.Interval, 7*24*time.Hour)
	ev := &routing.Evaluator{
		Router:    &routing.Router{Store: routing.NewStore(store), Models: models.NewRegistry(store), Policy: policy},
		Refresh:   discoverer(store, s.Queue.Worker.ClaudeBin),
		Emit:      emitRouting,
		AutoApply: s.Routing.AutoApply,
		Every:     every,
		StatePath: paths.RoutingEvaluatorStateFile(),
		Now:       time.Now,
	}
	mode := "proposes"
	if s.Routing.AutoApply {
		mode = "applies"
	}
	return supervisor.Unit{
		Name:   UnitRoutingEvaluator,
		Detail: fmt.Sprintf("model discovery + routing evaluation every %s; %s changes", every, mode),
		Tick: func(ctx context.Context) {
			if err := ev.Tick(ctx); err != nil {
				log.Printf("orchd: routing evaluator: %v", err)
			}
		},
		Interval: min(routingEvaluatorCheck, every),
	}, true
}

func emitRouting(summary string) error {
	return events.Append(paths.EventsDir(), routingEventStream, events.Event{
		Kind: "info", Source: UnitRoutingEvaluator, Summary: summary, Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}
