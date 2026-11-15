package daemon

// The supervised subsystems orchd runs, and how each is built from config.
// Keeping them here means `orch restart <name>` has one place to look for what
// a name refers to and what restarting it actually does.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cschleiden/go-workflows/backend"
	"github.com/cschleiden/go-workflows/worker"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/evidence"
	"github.com/willesq/orchestrator/internal/github"
	"github.com/willesq/orchestrator/internal/handoff"
	"github.com/willesq/orchestrator/internal/models"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/prs"
	"github.com/willesq/orchestrator/internal/prunecap"
	"github.com/willesq/orchestrator/internal/qdrant"
	"github.com/willesq/orchestrator/internal/queue"
	"github.com/willesq/orchestrator/internal/rag"
	"github.com/willesq/orchestrator/internal/reviews"
	"github.com/willesq/orchestrator/internal/selfupdate"
	"github.com/willesq/orchestrator/internal/settings"
	"github.com/willesq/orchestrator/internal/sources"
	"github.com/willesq/orchestrator/internal/spaces"
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
	// summaries feeds the session-summarizer unit; nil drops the unit.
	summaries *summaryQueue
	backend   backend.Backend
	store     *sql.DB
	// github supplies the scopes the PR watcher covers; nil fails its sweeps.
	github *github.Service
	// selfUpdate watches the binary orchd was launched from. Nil — as in tests,
	// or when the process cannot identify its own executable — drops the unit.
	selfUpdate *selfupdate.Watcher
	// supervised says whether something would start orchd again if it exited,
	// which is what makes exiting on a new build a restart rather than a stop.
	supervised bool
	// restart asks orchd to shut down so its service manager starts it again.
	// Nil drops the self-update unit for the same reason.
	restart func(reason string)
	// system receives qdrant's output and PID; nil discards them.
	system *systemProbe
	// docs is the docs scheduler shared with the SyncDocs RPC; nil builds one.
	docs *docs.Scheduler
	// settingsSvc is the settings reconciler shared with the settings RPCs;
	// nil drops the settings watcher.
	settingsSvc *settings.Service
	// taskCtl reconciles tasks when the worker starts, and live receives the
	// worker's runner; either may be nil.
	taskCtl *queue.Control
	live    *liveRunner
	// taskHub receives the runner's liveness changes; nil drops them.
	taskHub *taskHub
	// reviews is the PR review watcher shared with its RPCs; nil drops its unit.
	reviews *reviews.Watcher
	// ghProfiles picks the gh login of each space's GitHub calls; nil runs
	// every call as the default login.
	ghProfiles *ghProfiles
	// spaces re-checks GitHub org bindings before content is written; nil
	// checks none.
	spaces *spaces.Engine
	// placer takes docs that are no longer public out of the public space
	// on each docs sync and sources refresh (SP-030); nil checks none.
	placer *writePlacer
}

// subsystems lists orchd's supervised units in start order (stopped in reverse).
// Units whose backing feature is disabled or externally managed are simply absent
// — there is nothing for `orch restart` to bounce in that case.
func subsystems(d deps) []supervisor.Unit {
	var out []supervisor.Unit
	if u, ok := qdrantUnit(d.settings, d.apiKey, d.system); ok {
		out = append(out, u)
	}
	if d.settings.Queue.Worker.Runs() {
		var reg *models.Registry
		if d.store != nil {
			reg = models.NewRegistry(d.store)
		}
		out = append(out, workerUnit(d.backend, d.settings.Queue.Worker, secretEnv(d.settings), reg, d.taskCtl, d.live, d.taskHub))
	}
	sched := d.docs
	if sched == nil {
		sched = newDocsScheduler(d.settings, d.store, d.rag, nil, d.ghProfiles, d.placer)
	}
	out = append(out, sourcesUnit(d.settings, d.store, d.rag, d.spaces, d.placer), docsUnit(d.settings, sched))
	if d.settingsSvc != nil {
		out = append(out, settingsUnit(d.settingsSvc))
	}
	if d.settings.PRBabysitter.Enabled {
		out = append(out, prsUnit(d.settings, d.store, d.github, d.ghProfiles))
	}
	if d.reviews != nil {
		out = append(out, reviewsUnit(d.reviews))
	}
	if u, ok := routingEvaluatorUnit(d.settings, d.store); ok {
		out = append(out, u)
	}
	if d.rag != nil {
		out = append(out, handoffUnit(d.rag, d.summaries, handoff.Dir(), sources.Canonical(d.store, d.settings.Sources.KnownRoots)))
	}
	if d.summaries != nil {
		sum := d.settings.Sessions.Summarize
		out = append(out, summariesUnit(d.summaries, newSummarizer(sum, secretEnv(d.settings)), d.rag,
			interval(sum.Timeout, 2*time.Minute)))
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
				log.Printf("self-update: a new build is installed at %s (%s), but nothing supervises "+
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
func qdrantUnit(s config.Settings, apiKey string, sys *systemProbe) (supervisor.Unit, bool) {
	if s.RAG.Qdrant.Mode != "binary" {
		return supervisor.Unit{}, false
	}
	var output io.Writer
	if sys != nil && sys.logs != nil {
		output = sys.logs.SourceWriter("qdrant")
	}
	qp := qdrant.New(qdrant.Config{
		Output:   output,
		Version:  s.RAG.Qdrant.Version,
		BinDir:   filepath.Join(paths.DataDir(), "bin"),
		Storage:  s.QdrantStorageDir(),
		HTTPPort: s.RAG.Qdrant.HTTPPort,
		GRPCPort: s.RAG.Qdrant.GRPCPort,
		APIKey:   apiKey,
	})
	if sys != nil {
		sys.qdrantPID = qp.PID
	}
	return supervisor.Unit{
		Name:   UnitQdrant,
		Detail: fmt.Sprintf("native engine on :%d", s.RAG.Qdrant.HTTPPort),
		Run: func(ctx context.Context, beat func()) error {
			if err := qp.Start(ctx); err != nil {
				err = fmt.Errorf("start qdrant: %w", err)
				if errors.Is(err, qdrant.ErrKeyRejected) {
					// Only memory needs this engine. Exiting would take the socket,
					// hooks and task queue down with it and, under launchd's
					// KeepAlive, crash-loop; so orchd stays up with the unit failed,
					// where orch status shows why.
					log.Printf("qdrant: %v; memory requests will fail until this is fixed, the rest of orchd keeps running", err)
					return supervisor.Degraded(err)
				}
				return err
			}
			beat()
			return nil
		},
		Stop: qp.Stop,
	}, true
}

// workerUnit drains durable agent-task workflows, running each as a headless
// agent session. A restart builds a fresh worker over the same backend: a session
// cut short by the restart is run again by its workflow and resumed rather than
// started over (see workflows.AgentTask), and once the worker is up, ctl
// settles any task a stopped worker left without an active run.
func workerUnit(b backend.Backend, cfg config.QueueWorkerSettings, dropEnv []string, reg *models.Registry,
	ctl *queue.Control, live *liveRunner, hub *taskHub) supervisor.Unit {
	// Held across Run/Stop so the unit can join the worker it started: without
	// that, a restart would leave the previous worker draining the same backend.
	var current *worker.Worker
	return supervisor.Unit{
		Name:   UnitWorker,
		Detail: fmt.Sprintf("durable agent-task workflows (%d at a time)", maxConcurrent(cfg)),
		Run: func(ctx context.Context, beat func()) error {
			// The unit context is the runner's stop signal: canceling it on stop or
			// restart ends in-flight sessions instead of waiting them out.
			tr := newTaskRunner(cfg, ctx, dropEnv)
			tr.models = reg
			tr.r.OnLive = hub.publish
			workflows.SetRunner(tr)
			opts := worker.DefaultOptions
			opts.MaxParallelActivityTasks = maxConcurrent(cfg)
			w := worker.New(b, &opts)
			if err := w.RegisterWorkflow(workflows.AgentTask); err != nil {
				return err
			}
			if err := w.RegisterActivity(workflows.StartActivity); err != nil {
				return err
			}
			if err := w.RegisterActivity(workflows.RunActivity); err != nil {
				return err
			}
			// Reason: before Start, so a task too stale to run is stopped
			// before this worker can pick it up.
			reconcileTasks(ctx, ctl)
			if err := w.Start(ctx); err != nil {
				return err
			}
			current = w
			if live != nil {
				live.set(tr.r)
			}
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
func sourcesUnit(s config.Settings, store *sql.DB, ing sources.Ingester, sp *spaces.Engine, placer *writePlacer) supervisor.Unit {
	sched := sources.NewScheduler(sources.NewRegistry(store), ing,
		sources.RefreshOpts{KnownRoots: expandRoots(s.Sources.KnownRoots), ExcludeDirs: s.Ingest.ExcludeDirs,
			Guard: sourcesGuard(store), CheckOwner: docsOwnerCheck(sp), LeavesPublic: leavesPublic(placer)})
	return supervisor.Unit{
		Name:     UnitSources,
		Detail:   "web/repo/owner source freshness",
		Tick:     sched.Scan,
		Interval: interval(s.Sources.ScanInterval, 5*time.Minute),
	}
}

// newDocsScheduler builds the one docs scheduler orchd runs: the docs unit
// ticks it and SyncDocs asks it to run now, so a sync never runs twice at once.
// sp, when set, re-checks each owner's space binding before it syncs; ghp,
// when set, syncs each owner as its space's gh login. placer, when set,
// takes docs of repos no longer public out of the public space (SP-030).
func newDocsScheduler(s config.Settings, store *sql.DB, ing docs.Ingester, sp *spaces.Engine, ghp *ghProfiles, placer *writePlacer) *docs.Scheduler {
	var clients func(context.Context, string) (docs.GitHub, docs.Git, error)
	if ghp != nil {
		clients = ghp.docsClients
	}
	return docs.NewScheduler(docs.NewRegistry(store), ing, docs.SyncOpts{
		RepoCache:       s.RepoCacheDir(),
		IncludeArchived: s.Docs.IncludeArchived,
		IncludeForks:    s.Docs.IncludeForks,
		GH:              docs.NewGitHub(),
		Git:             docs.NewGit(),
		ExcludeDirs:     s.Ingest.ExcludeDirs,
		Guard:           docsGuard(store),
		CheckOwner:      docsOwnerCheck(sp),
		Clients:         clients,
		LeavesPublic:    leavesPublic(placer),
	}, interval(s.Docs.ScanInterval, time.Minute))
}

// leavesPublic is placer's check for the docs sync and the sources refresh,
// or nil without a placer.
func leavesPublic(placer *writePlacer) func(context.Context, string, rag.Meta) bool {
	if placer == nil {
		return nil
	}
	return placer.leavesPublic
}

// docsTick is how often the docs unit checks for a requested sync. Checking
// is a flag read, so a short cadence costs nothing and a request starts within
// seconds without interrupting anything.
const docsTick = 2 * time.Second

// docsUnit ingests GitHub code-repo markdown on a cadence (gh-CLI-driven, with
// repo/file skipping; separate from the generic sources scheduler).
func docsUnit(s config.Settings, sched *docs.Scheduler) supervisor.Unit {
	return supervisor.Unit{
		Name:     UnitDocs,
		Detail:   "GitHub markdown ingestion",
		Tick:     sched.Tick,
		Interval: docsTick,
	}
}

// prsUnit polls open PRs and issues in the user's enabled GitHub scopes via gh
// and caches state + events on transitions. gh failures (e.g. not
// authenticated) are non-fatal: the cache stays as-is.
func prsUnit(s config.Settings, store *sql.DB, gh *github.Service, ghp *ghProfiles) supervisor.Unit {
	bb := prs.New(paths.PRStateFile(), paths.EventsDir(), watchFrom(gh, ghp))
	var attr *evidence.Attributor
	if store != nil {
		attr = newAttributor(store, gh, ghp)
	}
	poll := interval(s.PRBabysitter.PollInterval, 10*time.Minute)
	var last, backoffUntil time.Time
	return supervisor.Unit{
		Name:   UnitPRs,
		Detail: "PRs and issues in your enabled GitHub scopes via gh; labels routed tasks by the fate of their PRs",
		// Ticks often but sweeps only every poll, or as soon as a hook asks
		// after a gh pr mutation.
		Tick: func(ctx context.Context) {
			if time.Now().Before(backoffUntil) || !prSweepDue(handoff.Dir(), last, poll) {
				return
			}
			last = time.Now()
			if err := bb.Sweep(ctx); rateLimited(err) {
				backoffUntil = time.Now().Add(rateLimitBackoff)
				log.Printf("pr-babysitter: GitHub rate limit; PR sweeps paused for %s: %v", rateLimitBackoff, err)
			}
			if attr != nil {
				if err := attr.Sweep(ctx); err != nil {
					log.Printf("pr-babysitter: pr outcomes: %v", err)
				}
			}
		},
		Interval: min(prRequestCheck, poll),
	}
}

// newAttributor labels routed tasks by their PRs, reading each PR as the gh
// login of its owner's space.
func newAttributor(store *sql.DB, gh *github.Service, ghp *ghProfiles) *evidence.Attributor {
	attr := evidence.NewAttributor(store)
	if ghp == nil {
		return attr
	}
	attr.GH = evidence.GH(ghp.RoutedBackground())
	if gh != nil {
		// A PR counts as the task's when the account of its owner's space
		// opened it (SP-074).
		attr.LoginFor = func(ctx context.Context, owner string) (string, error) {
			_, login, err := ghp.account(ctx, owner, func(ctx context.Context) (string, error) {
				id, err := gh.Identity(ctx, false)
				return id.Login, err
			})
			return login, err
		}
	}
	return attr
}

// prSweepDue reports whether the PR babysitter should sweep now: on its first
// tick, once poll has passed since the last sweep, or when a hook, a scope
// change or the app's Refresh asked and minRequestGap has passed. A request that comes sooner is
// left in place, so it runs when the gap is up rather than being lost.
func prSweepDue(dir string, last time.Time, poll time.Duration) bool {
	since := time.Since(last)
	if last.IsZero() || since >= poll {
		handoff.TakePRSweep(dir)
		return true
	}
	if since < minRequestGap {
		return false
	}
	return handoff.TakePRSweep(dir)
}

// minRequestGap spaces requested sweeps, so repeated requests (gh pr
// mutations, scope toggles) cannot spend the user's search rate limit.
const minRequestGap = time.Minute

// rateLimitBackoff pauses sweeps after GitHub reports a rate limit.
const rateLimitBackoff = 10 * time.Minute

// rateLimited reports whether err is GitHub refusing for rate limiting.
func rateLimited(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "rate limit")
}

// prRequestCheck is how often the PR babysitter looks for a refresh request.
const prRequestCheck = 15 * time.Second

// interval parses a configured cadence, falling back when it is unset or unparseable.
func interval(spec string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(spec); err == nil && d > 0 {
		return d
	}
	return fallback
}

func maxConcurrent(cfg config.QueueWorkerSettings) int {
	if cfg.MaxConcurrent > 0 {
		return cfg.MaxConcurrent
	}
	return 1
}

// taskRunner adapts agentrun to the workflows package's Runner.
type taskRunner struct {
	r *agentrun.Runner
	// models re-checks a task's routed model when it runs; nil skips the check.
	models *models.Registry
}

// secretEnv names the variables carrying credentials orchd holds, which queued
// sessions must not inherit.
func secretEnv(s config.Settings) []string {
	env := []string{"QDRANT_API_KEY"}
	if k := s.RAG.Qdrant.APIKeyEnv; k != "" && k != env[0] {
		env = append(env, k)
	}
	return env
}

func newTaskRunner(cfg config.QueueWorkerSettings, stop context.Context, dropEnv []string) taskRunner {
	home, _ := os.UserHomeDir()
	mode := cfg.PermissionMode
	if mode == "" {
		mode = "auto"
	}
	return taskRunner{r: &agentrun.Runner{
		Bin:          agentrun.FindClaude(cfg.ClaudeBin, home),
		DropEnv:      dropEnv,
		AllowedTools: cfg.AllowedTools["claude"],
		Verify: agentrun.VerifyOptions{
			Enabled: cfg.Verify.Enabled,
			Repos:   cfg.Verify.Repos,
			Timeout: interval(cfg.Verify.Timeout, 15*time.Minute),
			Retries: max(cfg.Verify.Retries, 0),
		},
		PermissionMode: mode,
		Timeout:        interval(cfg.Timeout, time.Hour),
		Home:           home,
		ScratchDir:     filepath.Join(paths.DataDir(), "tasks"),
		Stop:           stop,
		Codex:          codexOptions(cfg.Codex, home),
	}}
}

// orchBin is the orch CLI installed beside this daemon, or "".
func orchBin() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	bin := filepath.Join(filepath.Dir(exe), "orch")
	if info, err := os.Stat(bin); err != nil || info.IsDir() {
		return ""
	}
	return bin
}

// codexOptions configures Codex runs; with Codex off, Bin stays empty and the
// worker treats Codex routes as not runnable.
func codexOptions(c config.QueueCodexSettings, home string) agentrun.CodexOptions {
	o := agentrun.CodexOptions{
		Sandbox:  c.Sandbox,
		Network:  c.Network,
		StateDir: filepath.Join(paths.DataDir(), "codex"),
		MCPAllow: c.MCPAllow,
		OrchBin:  orchBin(),

		AllowProjectConfig: c.AllowProjectConfig,
	}
	if o.Sandbox == "" {
		o.Sandbox = "workspace-write"
	}
	if c.Enabled {
		o.Bin = agentrun.FindCodex(c.Bin, home)
	}
	return o
}

func (t taskRunner) Run(ctx context.Context, in workflows.AgentTaskInput) (workflows.Outcome, error) {
	o, err := t.r.Run(ctx, t.task(ctx, in))
	return workflows.Outcome{
		Status: o.Status, Result: o.Result,
		CostUSD: o.CostUSD, DurationMS: o.DurationMS, Turns: o.Turns,
		SessionID: o.SessionID, Models: o.Models, PRs: o.PRs, Verification: o.Verification,
	}, err
}

// task converts in for the runner, re-checking its routed model. A task can wait
// pending long after dispatch; if its model has since been disabled or hidden,
// the user's later choice wins and the agent's own model runs.
func (t taskRunner) task(ctx context.Context, in workflows.AgentTaskInput) agentrun.Task {
	task := agentTask(in)
	// Codex switched off after the task was routed to it: run on Claude Code
	// with the agent's own model rather than fail.
	if task.Provider == agentrun.ProviderCodex && (t.r == nil || t.r.Codex.Bin == "") {
		log.Printf("workflow-worker: task %s: routed to codex, which the worker no longer runs; running on Claude Code", in.ID)
		task.Provider, task.Model, task.Effort = "", "", ""
		return task
	}
	if task.Model == "" || t.models == nil {
		return task
	}
	m, err := t.models.Get(ctx, in.Route.Provider, task.Model)
	if err != nil || !m.Routable() {
		log.Printf("workflow-worker: task %s: routed model %s/%s is no longer routable; running the agent's own model",
			in.ID, in.Route.Provider, task.Model)
		task.Model, task.Effort = "", ""
		return task
	}
	if !m.SupportsEffort(task.Effort) {
		task.Effort = ""
	}
	return task
}

// agentTask converts a durable task input for the runner. Only providers the
// worker runs carry their model through; any other route runs unrouted.
func agentTask(in workflows.AgentTaskInput) agentrun.Task {
	t := agentrun.Task{ID: in.ID, Agent: in.Agent, Body: in.Body, Cwd: in.Cwd, Heads: in.Heads, Space: in.Space}
	switch in.Route.Provider {
	case models.ProviderClaude:
		t.Model = in.Route.Model
	case models.ProviderCodex:
		t.Provider, t.Model, t.Effort = agentrun.ProviderCodex, in.Route.Model, in.Route.Effort
	}
	return t
}

// taskEventStream is the event log finished queue tasks are reported to; the
// SessionStart digest reads it so results reach the next session.
const taskEventStream = "queue"

// notifyTaskDone reports a finished task to the queue event stream. The event
// names the task and how it ended but carries none of the result: the digest
// replays this stream into every new session's context, and a result is text an
// unattended agent wrote, possibly steered by an untrusted repository. Readers
// fetch the result deliberately with queue_read.
func notifyTaskDone(in workflows.AgentTaskInput, o workflows.Outcome) {
	summary := in.ID + " finished " + o.Status + "; read the result with queue_read or `orch task " + in.ID + "`"
	// Reason (INV-09): the event is derived from the task, so it carries
	// the space the task reads and writes, for readers to filter by.
	if err := events.Append(paths.EventsDir(), taskEventStream, events.Event{
		Kind: o.Status, Source: in.Agent, TaskID: in.ID, Summary: summary, Timestamp: time.Now().UTC().Format(time.RFC3339),
		Space: in.Space,
	}); err != nil {
		log.Printf("queue: record completion of %s: %v", in.ID, err)
	}
}

// sourcesGuard and docsGuard cap the syncs' deletions in orch.db; without a
// store each sync caps itself (an interface holding a nil *Guard would not
// compare equal to nil, so the nil case is spelled out).
func sourcesGuard(store *sql.DB) sources.PruneGuard {
	if store == nil {
		return nil
	}
	return prunecap.New(store)
}

func docsGuard(store *sql.DB) docs.PruneGuard {
	if store == nil {
		return nil
	}
	return prunecap.New(store)
}
