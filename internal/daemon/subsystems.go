package daemon

// The supervised subsystems orchd runs, and how each is built from config.
// Keeping them here means `orch restart <name>` has one place to look for what
// a name refers to and what restarting it actually does.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/cschleiden/go-workflows/backend"
	"github.com/cschleiden/go-workflows/worker"

	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/prs"
	"github.com/willesq/orchestrator/internal/qdrant"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/selfupdate"
	"github.com/willesq/orchestrator/internal/sources"
	"github.com/willesq/orchestrator/internal/supervisor"
	"github.com/willesq/orchestrator/internal/workflows"
)

// Supervised subsystem names. These are the stable identifiers `orch restart
// <name>` accepts and `orch status` prints, so treat them as a public contract.
const (
	UnitQdrant     = "qdrant"
	UnitWorker     = "workflow-worker"
	UnitSources    = "sources-scheduler"
	UnitDocs       = "docs-scheduler"
	UnitPRs        = "pr-babysitter"
	UnitSelfUpdate = "self-update"
)

// deps is what the supervised subsystems need in order to be constructed.
type deps struct {
	settings config.Settings
	apiKey   string
	rag      *rag.Service
	backend  backend.Backend
	store    *sql.DB
	// selfUpdate watches the binary orchd was launched from. Nil — as in tests,
	// or when the process cannot identify its own executable — drops the unit.
	selfUpdate *selfupdate.Watcher
	// supervised says whether something would start orchd again if it exited,
	// which is what makes exiting on a new build a restart rather than a stop.
	supervised bool
	// restart asks orchd to shut down so its service manager starts it again.
	// Nil drops the self-update unit for the same reason.
	restart func(reason string)
}

// subsystems lists orchd's supervised units in start order (stopped in reverse).
// Units whose backing feature is disabled or externally managed are simply absent
// — there is nothing for `orch restart` to bounce in that case.
func subsystems(d deps) []supervisor.Unit {
	var out []supervisor.Unit
	if u, ok := qdrantUnit(d.settings, d.apiKey); ok {
		out = append(out, u)
	}
	out = append(out, workerUnit(d.backend), sourcesUnit(d.settings, d.store, d.rag), docsUnit(d.settings, d.store, d.rag))
	if d.settings.PRBabysitter.Enabled {
		out = append(out, prsUnit(d.settings))
	}
	if d.selfUpdate != nil && d.restart != nil {
		out = append(out, selfUpdateUnit(d.selfUpdate, d.restart, d.supervised))
	}
	return out
}

// selfUpdateCheckInterval is how often orchd looks at the binary behind its own
// launch path. Stating one file costs nothing, so the cadence is about how long
// a freshly installed build should have to wait, not about the cost of looking.
// A variable only so a test need not wait out a real minute.
var selfUpdateCheckInterval = time.Minute

// selfUpdateUnit watches the binary the service manager launches orchd from and
// asks orchd to exit once a different build appears there — which is what makes
// `brew upgrade orch` take effect without anyone running `orch restart`.
//
// Exiting is safe at any tick: orchd's shutdown path drains the workflow worker
// and stops the qdrant engine, and agent tasks are durable, so work in flight
// resumes from its last persisted step under the new binary.
//
// When nothing supervises this process, exiting would replace a stale daemon
// with no daemon at all, so the drift is reported and orchd keeps serving.
func selfUpdateUnit(w *selfupdate.Watcher, restart func(reason string), supervised bool) supervisor.Unit {
	return supervisor.Unit{
		Name:     UnitSelfUpdate,
		Detail:   "restarts orchd when a new build is installed",
		Interval: selfUpdateCheckInterval,
		Tick: func(context.Context) {
			b, changed := w.Changed()
			if !changed {
				return
			}
			if !supervised {
				// Adopting the build keeps this to one line per new build rather
				// than one per check, for as long as the process stays up.
				log.Printf("orchd: a new build is installed at %s (%s), but nothing supervises "+
					"this process — run 'orch restart' to pick it up", w.Path(), b)
				w.Adopt(b)
				return
			}
			restart(fmt.Sprintf("a new build is installed at %s (%s)", w.Path(), b))
		},
	}
}

// qdrantUnit owns the native (Docker-free) engine's lifecycle. In docker or
// external mode the engine belongs to someone else, so there is no unit: the
// second return value is false and `orch status` reports the configured URL.
func qdrantUnit(s config.Settings, apiKey string) (supervisor.Unit, bool) {
	if s.RAG.Qdrant.Mode != "binary" {
		return supervisor.Unit{}, false
	}
	qp := qdrant.New(qdrant.Config{
		Version:  s.RAG.Qdrant.Version,
		BinDir:   filepath.Join(paths.DataDir(), "bin"),
		Storage:  s.QdrantStorageDir(),
		HTTPPort: s.RAG.Qdrant.HTTPPort,
		GRPCPort: s.RAG.Qdrant.GRPCPort,
		APIKey:   apiKey,
	})
	return supervisor.Unit{
		Name:   UnitQdrant,
		Detail: fmt.Sprintf("native engine on :%d", s.RAG.Qdrant.HTTPPort),
		Run: func(ctx context.Context, beat func()) error {
			if err := qp.Start(ctx); err != nil {
				return fmt.Errorf("start qdrant: %w", err)
			}
			beat()
			return nil
		},
		Stop: qp.Stop,
	}, true
}

// workerUnit drains durable agent-task workflows. A restart builds a fresh
// worker over the same backend, so an in-flight task resumes from its last
// persisted step rather than being lost.
func workerUnit(b backend.Backend) supervisor.Unit {
	// Held across Run/Stop so the unit can join the worker it started: without
	// that, a restart would leave the previous worker draining the same backend.
	var current *worker.Worker
	return supervisor.Unit{
		Name:   UnitWorker,
		Detail: "durable agent-task workflows",
		Run: func(ctx context.Context, beat func()) error {
			w := worker.New(b, nil)
			if err := w.RegisterWorkflow(workflows.AgentTask); err != nil {
				return err
			}
			if err := w.RegisterActivity(workflows.ProcessActivity); err != nil {
				return err
			}
			if err := w.Start(ctx); err != nil {
				return err
			}
			current = w
			beat()
			return nil
		},
		Stop: func() error {
			if current == nil {
				return nil
			}
			w := current
			current = nil
			// The context is already canceled; this drains in-flight tasks.
			return w.WaitForCompletion()
		},
	}
}

// sourcesUnit refreshes registered web/repo/owner sources on a cadence.
func sourcesUnit(s config.Settings, store *sql.DB, ing sources.Ingester) supervisor.Unit {
	sched := sources.NewScheduler(sources.NewRegistry(store), ing,
		sources.RefreshOpts{KnownRoots: expandRoots(s.Sources.KnownRoots)})
	return supervisor.Unit{
		Name:     UnitSources,
		Detail:   "web/repo/owner source freshness",
		Tick:     sched.Scan,
		Interval: interval(s.Sources.ScanInterval, 5*time.Minute),
	}
}

// docsUnit ingests GitHub code-repo markdown on a cadence (gh-CLI-driven, with
// repo/file skipping; separate from the generic sources scheduler).
func docsUnit(s config.Settings, store *sql.DB, ing docs.Ingester) supervisor.Unit {
	sched := docs.NewScheduler(docs.NewRegistry(store), ing, docs.SyncOpts{
		RepoCache:       s.RepoCacheDir(),
		IncludeArchived: s.Docs.IncludeArchived,
		IncludeForks:    s.Docs.IncludeForks,
		GH:              docs.NewGitHub(),
		Git:             docs.NewGit(),
	})
	return supervisor.Unit{
		Name:     UnitDocs,
		Detail:   "GitHub markdown ingestion",
		Tick:     sched.Scan,
		Interval: interval(s.Docs.ScanInterval, time.Minute),
	}
}

// prsUnit polls open PRs via gh and caches state + events on transitions. gh
// failures (e.g. not authenticated) are non-fatal: the cache stays as-is.
func prsUnit(s config.Settings) supervisor.Unit {
	bb := prs.New(paths.PRStateFile(), paths.EventsDir())
	return supervisor.Unit{
		Name:     UnitPRs,
		Detail:   "open-PR polling via gh",
		Tick:     func(ctx context.Context) { _ = bb.Sweep(ctx) },
		Interval: interval(s.PRBabysitter.PollInterval, 10*time.Minute),
	}
}

// interval parses a configured cadence, falling back when it is unset or unparseable.
func interval(spec string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(spec); err == nil && d > 0 {
		return d
	}
	return fallback
}
