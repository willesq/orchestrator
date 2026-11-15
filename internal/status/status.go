// Package status assembles the full sitrep behind `orch status`: orchd and its
// supervised subsystems, the vector store, the on-disk stores and their
// freshness, the harness wiring, and the preconditions that need no daemon.
//
// Collect never returns an error. A status report is most needed exactly when
// something is broken, so every collector degrades into a reported problem line
// instead of aborting the report. `orch check` remains the pass/fail gate with a
// non-zero exit; this is the human-readable picture.
package status

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/willesq/orchestrator/internal/check"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/docs"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/prs"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/sources"
	"github.com/willesq/orchestrator/internal/supervisor"
	"github.com/willesq/orchestrator/internal/version"
)

// Line severities. Only Bad counts toward the report's problem tally.
const (
	LevelInfo = ""
	LevelOK   = "ok"
	LevelWarn = "warn"
	LevelBad  = "bad"
)

// Line is one reported fact about one component.
type Line struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Detail string `json:"detail,omitempty"`
	Level  string `json:"level,omitempty"`
}

// Section groups related lines. A section with no lines carries a Note saying why.
type Section struct {
	Title string `json:"title"`
	Note  string `json:"note,omitempty"`
	Lines []Line `json:"lines,omitempty"`
}

// Report is the whole sitrep.
type Report struct {
	Version  string    `json:"version"`
	At       time.Time `json:"at"`
	Problems int       `json:"problems"`
	Sections []Section `json:"sections"`
}

// ServiceInfo describes how orchd is managed on this machine. The CLI supplies it
// because launchd/Homebrew knowledge lives there.
type ServiceInfo struct {
	Manager   string `json:"manager"` // "brew services" | "launchd"
	Detail    string `json:"detail,omitempty"`
	Installed bool   `json:"installed"`
}

// HarnessInfo is one coding-agent harness's wiring state.
type HarnessInfo struct {
	Name    string `json:"name"`
	Pending int    `json:"pending"` // edits `orch install --apply` would make
	// Disabled counts the items the user turned off in orch, which install
	// leaves out and drift ignores.
	Disabled int `json:"disabled,omitempty"`
	// SecurityOff names the security instruction modules turned off on
	// this machine; any is a warning, since agents then lack that policy.
	SecurityOff []string `json:"security_off,omitempty"`
	Err         string   `json:"error,omitempty"`
	// ToolHooks reports the tool-call hooks spaces depend on; nil when not
	// inspected.
	ToolHooks *ToolHooks `json:"tool_hooks,omitempty"`
}

// ToolHooks is whether a harness hands its tool calls to orchd (spaces plan
// SP-012). Spaces cover only some harnesses (D-20).
type ToolHooks struct {
	Covered bool     `json:"covered"`
	Wired   []string `json:"wired,omitempty"`
	Missing []string `json:"missing,omitempty"`
	Err     string   `json:"error,omitempty"`
}

// Deps are the seams Collect reads the world through; the CLI wires the real ones
// and tests substitute fakes.
type Deps struct {
	Settings config.Settings
	Now      func() time.Time
	// Orchd fetches orchd's own status, erroring when the daemon is unreachable.
	Orchd func(ctx context.Context) (*orchdv1.StatusResponse, error)
	// QdrantReady probes the vector store's HTTP readiness.
	QdrantReady func(ctx context.Context) bool
	// QdrantKeyRejected reports whether the engine refuses the API key status
	// resolved, as when a different qdrant holds the port: /readyz alone cannot
	// tell. It is false when no key resolves.
	QdrantKeyRejected func(ctx context.Context) bool
	// Service reports how orchd is installed as a service.
	Service func() ServiceInfo
	// SocketPresent reports whether orchd's socket file exists, which separates a
	// daemon that is down from one that is up but no longer answering.
	SocketPresent func() bool
	// Checks are the daemon-independent preconditions (see check.Preconditions).
	Checks []check.Check
	// Store opens orch.db for the registry/queue counts.
	Store func() (*sql.DB, error)
	// Harness reports per-harness install drift.
	Harness func() []HarnessInfo
	// SafetyNet reports the cc-safety-net install and guard rules, shown with
	// the harness wiring; nil leaves it out.
	SafetyNet func() []Line
	// RepositoryInstructions checks the current repository's shared guidance.
	RepositoryInstructions func() []Line
	// InstructionLoad sizes the instructions each harness loads into every
	// session, so their growth stays visible.
	InstructionLoad func() []Line
	// DirSize sums a directory's bytes.
	DirSize func(path string) int64
	// SpaceLogins fetches each space's gh login and Claude Code
	// configuration from orchd (operator only); nil leaves them out.
	SpaceLogins func(ctx context.Context) ([]*orchdv1.SpaceLogin, error)
}

// Collect assembles the report in a fixed section order.
func Collect(ctx context.Context, d Deps) Report {
	d = d.withDefaults()
	resp, orchdErr := d.Orchd(ctx)

	r := Report{Version: version.Version, At: d.Now()}
	r.Sections = []Section{
		serviceSection(d, resp, orchdErr),
		subsystemSection(d, resp, orchdErr),
		vectorSection(ctx, d, resp),
		dataSection(ctx, d),
		harnessSection(d),
		preconditionSection(d),
	}
	if sec, ok := spacesSection(resp, orchdErr); ok {
		if resp.GetSpaces().GetConfigured() {
			sec.Lines = append(sec.Lines, loginLines(ctx, d.SpaceLogins)...)
		}
		// After subsystems: what orchd enforces sits beside what it runs.
		r.Sections = slices.Insert(r.Sections, 2, sec)
	}
	if d.InstructionLoad != nil {
		sec := Section{Title: "always-loaded instructions", Lines: d.InstructionLoad()}
		if len(sec.Lines) == 0 {
			sec.Note = "no harness instruction files found"
		}
		r.Sections = append(r.Sections, sec)
	}
	if d.RepositoryInstructions != nil {
		lines := d.RepositoryInstructions()
		sec := Section{Title: "repository instructions", Lines: lines}
		if len(lines) == 0 {
			sec.Note = "not applicable outside the Orchestrator repository"
		}
		r.Sections = append(r.Sections, sec)
	}
	for _, s := range r.Sections {
		for _, l := range s.Lines {
			if l.Level == LevelBad {
				r.Problems++
			}
		}
	}
	return r
}

// withDefaults fills in the collectors a caller left nil, so a zero Deps still
// produces a coherent (if mostly unknown) report.
func (d Deps) withDefaults() Deps {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Orchd == nil {
		d.Orchd = func(context.Context) (*orchdv1.StatusResponse, error) {
			return nil, fmt.Errorf("no orchd client configured")
		}
	}
	if d.QdrantReady == nil {
		d.QdrantReady = func(context.Context) bool { return false }
	}
	if d.QdrantKeyRejected == nil {
		d.QdrantKeyRejected = func(context.Context) bool { return false }
	}
	if d.Service == nil {
		d.Service = func() ServiceInfo { return ServiceInfo{Manager: "unknown"} }
	}
	if d.SocketPresent == nil {
		d.SocketPresent = func() bool { return exists(paths.SocketPath()) }
	}
	if d.Harness == nil {
		d.Harness = func() []HarnessInfo { return nil }
	}
	if d.DirSize == nil {
		d.DirSize = DirSize
	}
	return d
}

func serviceSection(d Deps, resp *orchdv1.StatusResponse, err error) Section {
	sec := Section{Title: "service"}
	switch {
	case err == nil:
		sec.Lines = append(sec.Lines, Line{
			Name: "orchd", Value: "running", Level: LevelOK,
			Detail: fmt.Sprintf("%s, up %s", resp.GetVersion(),
				humanDuration(time.Duration(resp.GetUptimeSeconds())*time.Second)),
		})
	case d.SocketPresent():
		// Something bound the socket and left it behind: a wedged daemon that no
		// longer accepts, or one that died without unlinking. Either way `orch
		// daemon start` is a no-op — the supervisor already considers it started —
		// so point at the restart that actually clears it.
		sec.Lines = append(sec.Lines, Line{
			Name: "orchd", Value: "unreachable", Level: LevelBad,
			Detail: "socket present but not answering — run: orch daemon restart",
		})
	default:
		sec.Lines = append(sec.Lines, Line{
			Name: "orchd", Value: "not running",
			Detail: "start with: orch daemon start", Level: LevelBad,
		})
	}

	if err == nil {
		sec.Lines = append(sec.Lines, buildLine(resp.GetVersion()))
	}

	svc := d.Service()
	managed := Line{Name: "service manager", Value: svc.Manager, Detail: svc.Detail, Level: LevelOK}
	if !svc.Installed {
		managed.Level = LevelWarn
		if managed.Detail == "" {
			managed.Detail = "not installed — run: orch install --apply"
		}
	}
	sec.Lines = append(sec.Lines,
		managed,
		Line{Name: "socket", Value: paths.SocketPath()},
	)
	return sec
}

// buildLine compares the build orchd is *running* against the one installed on
// this machine, and reports drift with the command that closes it.
//
// The installed build is this CLI's own version: `orch` and `orchd` are built
// and shipped together, so the binary printing this report is the build orchd
// should be running. orchd normally notices an upgrade within a minute and
// restarts itself, so drift that survives long enough to be seen here means
// that did not happen — a foreground daemon with no service manager behind it,
// or a restart the supervisor never completed.
func buildLine(running string) Line {
	if running == version.Version {
		return Line{Name: "build", Value: running, Detail: "orchd matches the installed orch", Level: LevelOK}
	}
	return Line{
		Name: "build", Value: "drifted", Level: LevelWarn,
		Detail: fmt.Sprintf("orchd is running %s, %s is installed — run: orch restart",
			running, version.Version),
	}
}

// harnessSection reports whether each coding-agent harness's hooks and MCP
// entries still point at the current binaries.
func harnessSection(d Deps) Section {
	sec := Section{Title: "harness wiring"}
	infos := d.Harness()
	var safety []Line
	if d.SafetyNet != nil {
		safety = d.SafetyNet()
	}
	if len(infos) == 0 && len(safety) == 0 {
		sec.Note = "not inspected"
		return sec
	}
	for _, h := range infos {
		l := Line{Name: h.Name, Value: "wired", Level: LevelOK}
		switch {
		case h.Err != "":
			l.Value, l.Level, l.Detail = "unknown", LevelWarn, h.Err
		case h.Pending > 0:
			l.Value, l.Level = "drifted", LevelWarn
			l.Detail = fmt.Sprintf("%d edit(s) pending — run: orch install --target %s --apply", h.Pending, h.Name)
		}
		addDetail := func(s string) {
			if l.Detail == "" {
				l.Detail = s
			} else {
				l.Detail += "; " + s
			}
		}
		if h.Err == "" && h.Disabled > 0 {
			addDetail(fmt.Sprintf("%d disabled by you (orch settings list)", h.Disabled))
		}
		if h.Err == "" && len(h.SecurityOff) > 0 {
			l.Level = LevelWarn
			addDetail("security instructions off: " + strings.Join(h.SecurityOff, ", "))
		}
		sec.Lines = append(sec.Lines, l)
		if h.ToolHooks != nil {
			sec.Lines = append(sec.Lines, toolHooksLine(h.Name, *h.ToolHooks))
		}
	}
	sec.Lines = append(sec.Lines, safety...)
	return sec
}

// toolHooksLine reports a harness's tool-call hooks: the command log and the
// tool guard see only the calls of harnesses whose hooks are wired.
func toolHooksLine(harness string, t ToolHooks) Line {
	l := Line{Name: harness + " tool hooks"}
	switch {
	case t.Err != "":
		l.Value, l.Level, l.Detail = "unknown", LevelWarn, t.Err
	case !t.Covered:
		l.Value, l.Level = "not covered by spaces", LevelInfo
		l.Detail = "spaces cover Claude Code and Codex only; this harness's tool calls are not logged or guarded"
	case len(t.Missing) > 0:
		l.Value, l.Level = "missing "+strings.Join(t.Missing, ", "), LevelWarn
		l.Detail = "tool calls are not logged — run: orch install --target " + harness + " --apply"
	default:
		l.Value, l.Level, l.Detail = "wired", LevelOK, strings.Join(t.Wired, ", ")
	}
	return l
}

func subsystemSection(d Deps, resp *orchdv1.StatusResponse, err error) Section {
	sec := Section{Title: "subsystems"}
	if err != nil {
		sec.Note = "unavailable — orchd is not answering"
		return sec
	}
	if len(resp.GetSubsystems()) == 0 {
		sec.Note = "orchd reports no supervised subsystems"
		return sec
	}
	now := d.Now()
	for _, s := range resp.GetSubsystems() {
		l := Line{Name: s.GetName(), Value: s.GetState(), Level: LevelOK}
		if s.GetState() != supervisor.StateRunning {
			l.Level = LevelBad
		}
		var parts []string
		if s.GetDetail() != "" {
			parts = append(parts, s.GetDetail())
		}
		if b := s.GetLastBeat(); b > 0 {
			parts = append(parts, "last tick "+humanAgo(now, time.Unix(b, 0)))
		}
		if n := s.GetRestarts(); n > 0 {
			parts = append(parts, fmt.Sprintf("%d restart(s)", n))
		}
		if e := s.GetError(); e != "" {
			parts = append(parts, "error: "+e)
		}
		l.Detail = strings.Join(parts, ", ")
		sec.Lines = append(sec.Lines, l)
	}
	return sec
}

// spacesSection reports the enforced spaces policy and any drift from it: a
// draft saved but not applied, or orch.db's applied revision edited outside
// orchd. Neither changes what is enforced. ok is false while spaces were
// never used, so the report is unchanged for anyone without them.
func spacesSection(resp *orchdv1.StatusResponse, err error) (sec Section, ok bool) {
	sec = Section{Title: "spaces"}
	sp := resp.GetSpaces()
	if err != nil || sp == nil ||
		(sp.GetAppliedRevision() == 0 && sp.GetDraftRevision() == 0 && len(sp.GetDrift()) == 0 && sp.GetError() == "" &&
			len(sp.GetRelaxed()) == 0 && sp.GetEnforcementError() == "" && sp.GetSandbox().GetSetAt() == 0 &&
			sp.GetSandbox().GetProblem() == "") {
		return sec, false
	}
	policy := Line{Name: "policy", Value: "none applied", Level: LevelOK, Detail: "one shared space, as before spaces"}
	if n := sp.GetAppliedRevision(); n > 0 {
		policy.Value = fmt.Sprintf("revision %d applied", n)
		policy.Detail = ""
		if !sp.GetConfigured() {
			policy.Detail = "no spaces defined: one shared space"
		}
	}
	sec.Lines = append(sec.Lines, policy)
	for _, drift := range sp.GetDrift() {
		l := Line{Name: "drift", Level: LevelWarn}
		switch drift {
		case "draft":
			l.Value = fmt.Sprintf("draft revision %d not applied", sp.GetDraftRevision())
			l.Detail = "review with 'orch spaces diff', then 'orch spaces apply'"
		case "changed_outside_orchd":
			l.Value, l.Level = "applied revision changed outside orchd", LevelBad
			l.Detail = "orchd keeps enforcing the revision it loaded; apply a revision to replace it"
		default:
			l.Value = drift
		}
		sec.Lines = append(sec.Lines, l)
	}
	if len(sp.GetDrift()) == 0 && sp.GetCounts() != nil {
		// Reason: SP-031 lists drift with the counts even when there is none.
		sec.Lines = append(sec.Lines, Line{Name: "drift", Value: "none", Level: LevelOK})
	}
	for _, sw := range sp.GetRelaxed() {
		sec.Lines = append(sec.Lines, relaxedLine(sw))
	}
	if sb := sp.GetSandbox(); sb != nil && (sp.GetConfigured() || sb.GetSetAt() > 0 || sb.GetProblem() != "") {
		sec.Lines = append(sec.Lines, sandboxLine(sb))
	}
	if l, ok := quarantineLine(sp); ok {
		sec.Lines = append(sec.Lines, l)
	}
	sec.Lines = append(sec.Lines, countLines(sp)...)
	if e := sp.GetEnforcementError(); e != "" {
		sec.Lines = append(sec.Lines, Line{Name: "enforcement", Value: "switches in orch.db not taken", Level: LevelBad, Detail: printable(e)})
	}
	if e := sp.GetError(); e != "" {
		sec.Lines = append(sec.Lines, Line{Name: "error", Value: e, Level: LevelBad})
	}
	return sec, true
}

// relaxedLine reports an enforcement layer switched off (or the guard only
// logging), and since when (SP-070).
//
// Reason for printable: the fields come from orch.db, which a process
// running as the user can edit, and land in the operator's terminal.
func relaxedLine(sw *orchdv1.EnforcementSwitch) Line {
	l := Line{Name: "enforcement", Level: LevelWarn,
		Value: printable(fmt.Sprintf("%s is %s", sw.GetLayer(), sw.GetState()))}
	var parts []string
	if t := sw.GetTitle(); t != "" {
		parts = append(parts, t)
	}
	if p := sw.GetProblem(); p != "" {
		l.Value = printable(sw.GetLayer() + " does not enforce")
		parts = append(parts, p)
	}
	if at := sw.GetSetAt(); at > 0 {
		parts = append(parts, "since "+time.Unix(at, 0).Local().Format("2006-01-02 15:04"))
	}
	if u := sw.GetUntil(); u > 0 {
		parts = append(parts, fmt.Sprintf("reverts to %s at %s", sw.GetRevertsTo(), time.Unix(u, 0).Local().Format("2006-01-02 15:04")))
	}
	if by := sw.GetSetBy(); by != "" {
		parts = append(parts, "set by "+by)
	}
	parts = append(parts, "switch back with 'orch spaces enforce "+sw.GetLayer()+" "+sw.GetDefaultState()+"'")
	l.Detail = printable(strings.Join(parts, "; "))
	return l
}

// sandboxLine reports the sandbox setting (D-61) once: what it is, what it
// does now, and who set it. It is a choice, not a switch, so only off and a
// problem with orch.db's copy are more than informational.
func sandboxLine(sb *orchdv1.SandboxSetting) Line {
	l := Line{Name: "sandbox", Level: LevelOK, Value: printable(sb.GetMode())}
	if sb.GetMode() == sb.GetDefaultMode() {
		l.Value += " (default)"
	}
	if sb.GetMode() == "off" {
		l.Level = LevelWarn
	}
	var parts []string
	if d := sb.GetDetail(); d != "" {
		parts = append(parts, d)
	}
	if at := sb.GetSetAt(); at > 0 {
		parts = append(parts, "set "+time.Unix(at, 0).Local().Format("2006-01-02 15:04"))
	}
	if by := sb.GetSetBy(); by != "" {
		parts = append(parts, "by "+by)
	}
	if p := sb.GetProblem(); p != "" {
		l.Level = LevelBad
		parts = append(parts, p)
	}
	parts = append(parts, "change with 'orch spaces sandbox auto|on|off'")
	l.Detail = printable(strings.Join(parts, "; "))
	return l
}

// printable replaces control and format characters with spaces.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
}

func vectorSection(ctx context.Context, d Deps, resp *orchdv1.StatusResponse) Section {
	q := d.Settings.RAG.Qdrant
	sec := Section{Title: "vector store"}

	owner := "externally managed"
	if q.Mode == "binary" {
		owner = "orchd-managed"
	}
	engine := Line{
		Name: "engine", Value: "ready", Level: LevelOK,
		Detail: fmt.Sprintf("mode=%s, url=%s, %s",
			orNone(q.Mode), orNone(redactURL(d.Settings.QdrantURL())), owner),
	}
	switch {
	case !d.QdrantReady(ctx):
		engine.Value, engine.Level = "not reachable", LevelBad
	case d.QdrantKeyRejected(ctx):
		// Status resolves the key itself, from its own environment and files,
		// so it cannot be sure this is the key orchd sends.
		engine.Value, engine.Level = "rejects the resolved API key", LevelBad
		engine.Detail += ", another engine may hold the port, or orchd may use a different key"
	}
	sec.Lines = append(sec.Lines,
		engine,
		Line{Name: "collection", Value: orNone(q.Collection)},
	)
	if q.Mode == "binary" {
		dir := d.Settings.QdrantStorageDir()
		sec.Lines = append(sec.Lines, Line{
			Name: "storage", Value: humanBytes(d.DirSize(dir)), Detail: dir,
		})
	}
	if m := resp.GetMigration(); m != nil {
		detail := fmt.Sprintf("%d/%d sources, %d chunks", m.GetSourcesDone(), m.GetSourcesTotal(), m.GetChunks())
		if at := m.GetStartedAt(); at > 0 {
			detail += ", started " + humanAgo(d.Now(), time.Unix(at, 0))
		}
		sec.Lines = append(sec.Lines, Line{Name: "migration", Value: "copying to " + m.GetTarget(), Level: LevelOK, Detail: detail})
	}
	return sec
}

func dataSection(ctx context.Context, d Deps) Section {
	sec := Section{Title: "data"}
	dbPath := paths.DBFile()
	sec.Lines = append(sec.Lines,
		Line{Name: "data dir", Value: paths.DataDir()},
		fileLine("orch.db", dbPath),
		fileLine("workflows.db", filepath.Join(paths.DataDir(), "workflows.db")),
	)

	switch {
	case !exists(dbPath):
		// Reporting must not create (and migrate) the store as a side effect.
		sec.Lines = append(sec.Lines, Line{
			Name: "registries", Value: "not created yet",
			Detail: "created on first ingest or orchd start",
		})
	case d.Store == nil:
		sec.Lines = append(sec.Lines, Line{
			Name: "registries", Value: "not inspected", Level: LevelWarn,
		})
	default:
		conn, err := d.Store()
		if err != nil {
			sec.Lines = append(sec.Lines, Line{
				Name: "registries", Value: "unreadable", Detail: err.Error(), Level: LevelWarn,
			})
			break
		}
		defer func() { _ = conn.Close() }()
		sec.Lines = append(sec.Lines,
			tasksLine(ctx, conn),
			sourcesLine(ctx, conn, d.Now().Unix()),
			docsLine(ctx, conn, d.Now()),
		)
	}

	sec.Lines = append(sec.Lines, prsLine(d.Now()))
	return sec
}

// tasksLine counts dispatched agent tasks by status from orch.db's result ledger.
func tasksLine(ctx context.Context, conn *sql.DB) Line {
	rows, err := conn.QueryContext(ctx, `SELECT status, COUNT(*) FROM tasks GROUP BY status`)
	if err != nil {
		return Line{Name: "tasks", Value: "unreadable", Detail: err.Error(), Level: LevelWarn}
	}
	defer func() { _ = rows.Close() }()
	total := 0
	var parts []string
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return Line{Name: "tasks", Value: "unreadable", Detail: err.Error(), Level: LevelWarn}
		}
		total += n
		parts = append(parts, fmt.Sprintf("%d %s", n, st))
	}
	// Without this a mid-iteration driver error would report a short count as
	// if it were the whole picture.
	if err := rows.Err(); err != nil {
		return Line{Name: "tasks", Value: "unreadable", Detail: err.Error(), Level: LevelWarn}
	}
	return Line{Name: "tasks", Value: fmt.Sprintf("%d recorded", total), Detail: strings.Join(parts, ", ")}
}

// sourcesLine summarises the freshness registry the sources scheduler drives.
func sourcesLine(ctx context.Context, conn *sql.DB, now int64) Line {
	list, err := sources.NewRegistry(conn).List(ctx)
	if err != nil {
		return Line{Name: "sources", Value: "unreadable", Detail: err.Error(), Level: LevelWarn}
	}
	due, unhealthy, attention := 0, 0, 0
	for _, s := range list {
		if now >= s.LastFetched+s.RefreshSecs {
			due++
		}
		switch {
		case s.Status == "stale" || s.Status == "missing" || s.Status == "error":
			unhealthy++
		case strings.HasPrefix(s.Status, "attention"):
			// A prune refused past its cap waits for the operator.
			attention++
		}
	}
	l := Line{
		Name:   "sources",
		Value:  fmt.Sprintf("%d registered", len(list)),
		Detail: fmt.Sprintf("%d due, %d stale/missing", due, unhealthy),
	}
	if attention > 0 {
		l.Detail += fmt.Sprintf(", %d need attention (orch source list)", attention)
	}
	if unhealthy > 0 || attention > 0 {
		l.Level = LevelWarn
	}
	return l
}

// docsLine summarises the GitHub markdown owners the docs scheduler syncs.
func docsLine(ctx context.Context, conn *sql.DB, now time.Time) Line {
	owners, err := docs.NewRegistry(conn).ListOwners(ctx)
	if err != nil {
		return Line{Name: "docs owners", Value: "unreadable", Detail: err.Error(), Level: LevelWarn}
	}
	var last int64
	for _, o := range owners {
		if o.LastSynced > last {
			last = o.LastSynced
		}
	}
	detail := "never synced"
	if last > 0 {
		detail = "last sync " + humanAgo(now, time.Unix(last, 0))
	}
	line := Line{Name: "docs owners", Value: fmt.Sprintf("%d registered", len(owners)), Detail: detail}
	// A failed sync needs attention: say how many repos are failing and why.
	failing, reason := 0, ""
	for _, o := range owners {
		repos, err := docs.NewRegistry(conn).ListRepos(ctx, o.Owner)
		if err != nil {
			continue
		}
		for _, r := range repos {
			if r.Status == "error" {
				failing++
				if reason == "" {
					reason = o.Owner + "/" + r.Repo + ": " + r.Error
				}
			}
		}
		if o.Error != "" && o.Status != "ok" {
			// The owner's own reason (stopped early, listing failed) explains
			// the most.
			reason = o.Owner + ": " + o.Error
		}
	}
	if failing > 0 || reason != "" {
		line.Level = LevelWarn
		line.Value = fmt.Sprintf("%d registered, needs attention", len(owners))
		if failing > 0 {
			detail += fmt.Sprintf("; %d repo(s) failing", failing)
		}
		line.Detail = detail + "; " + reason + " — retry with: orch docs sync"
	}
	return line
}

// prsLine reports the PR babysitter's cached state, which survives orchd restarts.
func prsLine(now time.Time) Line {
	path := paths.PRStateFile()
	st := prs.LoadState(path)
	var prCount int
	for _, item := range st.PRs {
		if item.IsPR() {
			prCount++
		}
	}
	l := Line{Name: "pr cache", Value: fmt.Sprintf("%d PRs, %d issues", prCount, len(st.PRs)-prCount)}
	var parts []string
	if n := len(prs.Filter(st, true, false)); n > 0 {
		parts = append(parts, fmt.Sprintf("%d blocked", n))
	}
	if n := len(prs.Filter(st, false, true)); n > 0 {
		parts = append(parts, fmt.Sprintf("%d ready", n))
	}
	if fi, err := os.Stat(path); err == nil {
		parts = append(parts, "updated "+humanAgo(now, fi.ModTime()))
	} else {
		parts = append(parts, "no cache file yet")
	}
	l.Detail = strings.Join(parts, ", ")
	return l
}

func preconditionSection(d Deps) Section {
	sec := Section{Title: "preconditions"}
	if len(d.Checks) == 0 {
		sec.Note = "no precondition checks configured"
		return sec
	}
	results, _ := check.RunAll(d.Checks)
	for _, res := range results {
		l := Line{Name: res.Name, Value: "ok", Detail: res.Detail, Level: LevelOK}
		if !res.OK {
			l.Value, l.Level = "failing", LevelBad
		}
		if l.Detail == l.Value {
			l.Detail = "" // a check whose only detail is "ok" adds nothing
		}
		sec.Lines = append(sec.Lines, l)
	}
	return sec
}

// fileLine reports a file's size, or that it is absent (not a problem: the stores
// are created lazily on first use).
func fileLine(name, path string) Line {
	fi, err := os.Stat(path)
	if err != nil {
		return Line{Name: name, Value: "absent", Detail: path}
	}
	return Line{Name: name, Value: humanBytes(fi.Size()), Detail: path}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// DirSize sums the bytes of every regular file under dir (0 when absent).
func DirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil //nolint:nilerr // an unreadable entry just does not count
		}
		if fi, err := entry.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

func orNone(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

// redactURL strips any embedded credentials before a URL is printed. A
// configured engine URL may carry userinfo (http://user:pass@host), and reports
// get pasted into issues, so the password must never reach stdout or --json.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	return u.Redacted()
}
