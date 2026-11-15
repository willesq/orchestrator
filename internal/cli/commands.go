package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"gopkg.in/yaml.v3"

	"github.com/willesq/orchestrator/internal/agentrun"
	"github.com/willesq/orchestrator/internal/check"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/mcpserver"
	"github.com/willesq/orchestrator/internal/memctx"
	"github.com/willesq/orchestrator/internal/model"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/qdrant"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/taskfmt"
	"github.com/willesq/orchestrator/internal/timeline"
	"github.com/willesq/orchestrator/internal/version"
)

func newCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Verify runtime preconditions (onnxruntime, model, orchd, gh); non-zero exit on failure",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			results, ok := check.RunAll(check.Default())
			check.Print(c.OutOrStdout(), results)
			if !ok {
				return fmt.Errorf("one or more preconditions failed")
			}
			return nil
		},
	}
}

func newModelCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "model", Short: "Manage the embedding model and the coding-agent model registry"}
	cmd.AddCommand(registryModelCmds()...)
	cmd.AddCommand(&cobra.Command{
		Use:   "fetch",
		Short: "Download the embedding model into the model dir if missing",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			v, err := config.Load()
			if err != nil {
				return err
			}
			s, err := config.Unmarshal(v)
			if err != nil {
				return err
			}
			dir := s.Embedder.ModelDir
			if dir == "" {
				dir = filepath.Join(paths.DataDir(), "models", "minilm")
			}
			m := model.New(model.Config{Dir: dir})
			if m.Present() {
				fmt.Fprintf(c.OutOrStdout(), "model already present in %s\n", dir)
				return nil
			}
			fmt.Fprintf(c.OutOrStdout(), "downloading model into %s ...\n", dir)
			if err := m.Ensure(c.Context()); err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), "done")
			return nil
		},
	})
	return cmd
}

func newInstallCmd() *cobra.Command {
	var (
		apply  bool
		target string
		home   string
		binDir string
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Wire the orchestrator into a coding-agent harness (dry-run by default)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if home == "" {
				h, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				home = h
			}
			bins, err := installBins(binDir)
			if err != nil {
				return err
			}
			var adapters []install.Adapter
			if target == "all" {
				adapters = install.AllAdapters()
			} else {
				a, err := install.AdapterFor(target)
				if err != nil {
					return err
				}
				adapters = []install.Adapter{a}
			}
			backupDir := filepath.Join(paths.DataDir(), "backups")
			for _, a := range adapters {
				plan := install.BuildPlan(a, bins, home)
				if apply {
					if err := plan.Apply(c.OutOrStdout(), backupDir); err != nil {
						return err
					}
				} else if err := plan.Print(c.OutOrStdout()); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "write the changes (default: dry-run, writes nothing)")
	cmd.Flags().StringVar(&target, "target", "claude-code", "harness: claude-code, codex, gemini, or all")
	cmd.Flags().StringVar(&home, "home", "", "override HOME (for testing against a temp dir)")
	cmd.Flags().StringVar(&binDir, "bin-dir", "", "dir of the built binaries (default: dir of this executable)")
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the orch version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "orch %s (commit %s)\n", version.Version, version.Commit)
			return nil
		},
	}
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read configuration, or reset it",
		Long: "Settings live in orch.db and are changed in Orch.app (Settings). These commands read\n" +
			"them, and reset reaches them when orchd cannot run.",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "get <key>",
			Short: "Get a config value by dotted key",
			Args:  cobra.ExactArgs(1),
			RunE: func(c *cobra.Command, args []string) error {
				v, err := config.Load()
				if err != nil {
					return err
				}
				val := config.Get(v, args[0])
				if val == nil {
					return fmt.Errorf("no such key: %s", args[0])
				}
				fmt.Fprintln(c.OutOrStdout(), val)
				return nil
			},
		},
		newConfigResetCmd(),
		&cobra.Command{
			Use:   "list",
			Short: "List all configuration",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				v, err := config.Load()
				if err != nil {
					return err
				}
				out, err := yaml.Marshal(config.List(v))
				if err != nil {
					return err
				}
				fmt.Fprint(c.OutOrStdout(), string(out))
				return nil
			},
		},
	)
	return cmd
}

func newPingCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "ping",
		Short: "Ping the orchd daemon",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			resp, err := doPing(cf, 3*time.Second)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "orchd %s, uptime %ds\n", resp.GetVersion(), resp.GetUptimeSeconds())
			return nil
		},
	}
}

func newDaemonCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "daemon", Short: "Manage the orchd daemon"}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "status",
			Short: "Show whether orchd is reachable",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				resp, err := doPing(cf, daemonProbeTimeout)
				if err != nil {
					fmt.Fprintln(c.OutOrStdout(), daemonDownLine())
					return nil
				}
				fmt.Fprintf(c.OutOrStdout(), "orchd: running (%s, uptime %ds)\n",
					resp.GetVersion(), resp.GetUptimeSeconds())
				return nil
			},
		},
		&cobra.Command{
			Use:   "start",
			Short: "Start orchd (brew services when Homebrew-managed, else launchd)",
			Args:  cobra.NoArgs,
			RunE:  func(c *cobra.Command, _ []string) error { return daemonControl(c.OutOrStdout(), cf, "start") },
		},
		&cobra.Command{
			Use:   "stop",
			Short: "Stop orchd (brew services when Homebrew-managed, else launchd)",
			Args:  cobra.NoArgs,
			RunE:  func(c *cobra.Command, _ []string) error { return daemonControl(c.OutOrStdout(), cf, "stop") },
		},
		&cobra.Command{
			Use:   "restart",
			Short: "Restart orchd (brew services when Homebrew-managed, else launchd)",
			Args:  cobra.NoArgs,
			RunE:  func(c *cobra.Command, _ []string) error { return daemonControl(c.OutOrStdout(), cf, "restart") },
		},
	)
	return cmd
}

// daemonDownLine describes an unreachable orchd. A leftover socket means the
// daemon is up but no longer accepting — a state `orch daemon start` cannot fix,
// because the supervisor already considers the job started.
func daemonDownLine() string {
	if _, err := os.Stat(paths.SocketPath()); err == nil {
		return "orchd: unreachable (socket present but not answering — run 'orch daemon restart')"
	}
	return "orchd: not running (start with 'orch daemon start')"
}

// runCommand runs an external command; overridable in tests.
var runCommand = func(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// How long a lifecycle command waits for orchd to reach the state it asked for,
// how often it probes while waiting, and how long one probe may take. Vars so
// tests can collapse the wait.
var (
	daemonWait         = 20 * time.Second
	daemonPoll         = 250 * time.Millisecond
	daemonProbeTimeout = 2 * time.Second
)

// daemonIsHomebrew reports whether this orch is a Homebrew install (binary under
// a Cellar prefix), in which case `brew services` supervises orchd. A var so
// tests can force either path.
var daemonIsHomebrew = func() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return strings.Contains(exe, "/Cellar/")
}

// daemonControl performs a lifecycle op (start|stop|restart) and only reports
// success once orchd has actually reached the requested state.
func daemonControl(w io.Writer, cf clientFactory, op string) error {
	if op == "start" {
		if resp, err := doPing(cf, daemonProbeTimeout); err == nil {
			fmt.Fprintf(w, "orchd already running (%s, uptime %ds)\n",
				resp.GetVersion(), resp.GetUptimeSeconds())
			return nil
		}
	}
	if err := superviseDaemon(w, op); err != nil {
		return err
	}
	return awaitDaemon(w, cf, op)
}

// superviseDaemon hands the lifecycle op to whatever supervises orchd: `brew
// services` for a Homebrew install, launchctl otherwise. This keeps the lifecycle
// commands consistent with how the daemon was started. It reports only that the
// supervisor accepted the op — awaitDaemon decides whether it took effect.
func superviseDaemon(w io.Writer, op string) error {
	if daemonIsHomebrew() {
		if err := runCommand("brew", "services", op, "orch"); err != nil {
			return fmt.Errorf("brew services %s orch: %w", op, err)
		}
		verbs := map[string]string{"start": "started", "stop": "stopped", "restart": "restarted"}
		fmt.Fprintf(w, "orchd %s (brew services %s orch)\n", verbs[op], op)
		return nil
	}
	return launchctlControl(w, op)
}

// awaitDaemon blocks until orchd has actually reached the state op asked for, and
// reports what really happened rather than what the supervisor accepted.
//
// Supervisors return success too eagerly to be trusted on their own: `brew
// services start` exits zero for a job it already considers started, even when
// the daemon behind it is wedged and refusing connections — which turns `orch
// daemon start` into a no-op that claims success while `orch status` keeps
// reporting the daemon down.
func awaitDaemon(w io.Writer, cf clientFactory, op string) error {
	wantUp := op != "stop"
	deadline := time.Now().Add(daemonWait)
	for {
		resp, err := doPing(cf, daemonProbeTimeout)
		if up := err == nil; up == wantUp {
			if wantUp {
				fmt.Fprintf(w, "orchd is answering on %s (%s)\n", paths.SocketPath(), resp.GetVersion())
			}
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(daemonPoll)
	}
	if wantUp {
		return fmt.Errorf("orchd did not answer on %s within %s.\n"+
			"The service manager accepted the %s, so orchd is likely wedged, crash-looping, "+
			"or still running an older binary.\n"+
			"Try 'orch daemon restart'; if that does not help, check orchd's log",
			paths.SocketPath(), daemonWait, op)
	}
	return fmt.Errorf("orchd is still answering on %s %s after the stop — "+
		"the service manager accepted it but the daemon has not exited",
		paths.SocketPath(), daemonWait)
}

// launchctlControl drives the orchd launchd plist written by `orch install` (used
// for source builds not managed by Homebrew).
func launchctlControl(w io.Writer, op string) error {
	switch op {
	case "start":
		return launchctlAction(w, "load", "started")
	case "stop":
		return launchctlAction(w, "unload", "stopped")
	case "restart":
		_ = launchctlAction(io.Discard, "unload", "stopped")
		return launchctlAction(w, "load", "started")
	default:
		return fmt.Errorf("unknown daemon op %q", op)
	}
}

func launchctlAction(w io.Writer, action, verb string) error {
	plist := orchdPlistPath()
	if _, err := os.Stat(plist); err != nil {
		return fmt.Errorf("orchd plist not found at %s — run 'orch install --apply' first", plist)
	}
	if err := runCommand("launchctl", action, "-w", plist); err != nil {
		return fmt.Errorf("launchctl %s: %w", action, err)
	}
	fmt.Fprintf(w, "orchd %s (launchctl %s)\n", verb, action)
	return nil
}

func orchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", "com.orchestrator.orchd.plist")
}

func newQdrantCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "qdrant", Short: "Inspect and migrate the vector store engine"}
	cmd.AddCommand(newQdrantCutoverCmd())
	cmd.AddCommand(newQdrantMigrateCmd())
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show whether the qdrant engine is ready",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			v, err := config.Load()
			if err != nil {
				return err
			}
			s, err := config.Unmarshal(v)
			if err != nil {
				return err
			}
			if s.RAG.Qdrant.Mode != "binary" {
				fmt.Fprintf(c.OutOrStdout(), "qdrant: mode=%s url=%s (not orchd-managed)\n",
					s.RAG.Qdrant.Mode, s.RAG.Qdrant.URL)
				return nil
			}
			qp := qdrant.New(qdrant.Config{HTTPPort: s.RAG.Qdrant.HTTPPort})
			if qp.Ready(c.Context()) {
				fmt.Fprintf(c.OutOrStdout(), "qdrant: ready on :%d\n", s.RAG.Qdrant.HTTPPort)
			} else {
				fmt.Fprintln(c.OutOrStdout(), "qdrant: not running (start with 'orch daemon start')")
			}
			return nil
		},
	})
	return cmd
}

func doPing(cf clientFactory, timeout time.Duration) (*orchdv1.PingResponse, error) {
	client, cleanup, err := cf()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return client.Ping(ctx, &orchdv1.PingRequest{})
}

func newIngestCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "ingest", Short: "Ingest content into the RAG store"}
	var (
		sourceURI  string
		sourceType string
		scope      string
		tags       []string
	)
	// orchd refuses this from a queued task too; refusing first gives the
	// session a plain reason before it reads stdin.
	textCmd := operatorOnlyMsg("queue tasks cannot write to the RAG store", &cobra.Command{
		Use:   "text",
		Short: "Ingest text from stdin",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			data, err := io.ReadAll(c.InOrStdin())
			if err != nil {
				return err
			}
			if len(data) == 0 {
				return fmt.Errorf("no input on stdin")
			}
			if sourceURI == "" {
				return fmt.Errorf("--source-uri is required")
			}
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			resp, err := client.Ingest(ctx, &orchdv1.IngestRequest{
				Text:       string(data),
				SourceUri:  sourceURI,
				SourceType: sourceType,
				Scope:      scope,
				Tags:       tags,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "ingested %d chunk(s) from %s\n", resp.GetChunks(), sourceURI)
			return nil
		},
	})
	textCmd.Flags().StringVar(&sourceURI, "source-uri", "", "source URI (required)")
	textCmd.Flags().StringVar(&sourceType, "type", "manual", "source type")
	textCmd.Flags().StringVar(&scope, "scope", "", "scope (personal|work|project:<name>)")
	textCmd.Flags().StringSliceVar(&tags, "tag", nil, "tag (repeatable)")
	cmd.AddCommand(textCmd)
	return cmd
}

func newMCPCmd(cf clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP server (stdio) exposing orchd to a coding agent",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "rag",
			Short: "Serve the RAG MCP server on stdio",
			Args:  cobra.NoArgs,
			RunE:  func(_ *cobra.Command, _ []string) error { return serveMCP(cf, mcpserver.NewRAG) },
		},
		&cobra.Command{
			Use:   "queue",
			Short: "Serve the queue MCP server on stdio",
			Args:  cobra.NoArgs,
			RunE:  func(_ *cobra.Command, _ []string) error { return serveMCP(cf, mcpserver.NewQueue) },
		},
	)
	return cmd
}

// serveMCP dials orchd, builds the requested MCP server, and serves it on stdio.
func serveMCP(cf clientFactory, build func(orchdv1.OrchdClient) *server.MCPServer) error {
	client, cleanup, err := cf()
	if err != nil {
		return err
	}
	defer cleanup()
	return mcpserver.Serve(build(client))
}

func newDispatchCmd(cf clientFactory) *cobra.Command {
	var agent, cwd, kind, modelRef string
	cmd := &cobra.Command{
		Use:   "dispatch <body>",
		Short: "Dispatch a durable agent task (runs headless when queue.worker.enabled)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			// A queued session may reach this from its shell; refusing keeps a task
			// from queueing more. orchd's cap on active tasks backs this up.
			if id := os.Getenv(agentrun.TaskIDEnv); id != "" {
				return fmt.Errorf("queue tasks cannot dispatch further tasks (running as %s)", id)
			}
			body := args[0]
			for _, a := range args[1:] {
				body += " " + a
			}
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if cwd == "" {
				if cwd, err = os.Getwd(); err != nil {
					return err
				}
			}
			if cwd, err = filepath.Abs(cwd); err != nil {
				return err
			}
			resp, err := client.Dispatch(ctx, &orchdv1.DispatchRequest{
				Agent: agent, Body: body, Cwd: cwd, TaskKind: kind, Model: modelRef,
			})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			fmt.Fprintf(out, "dispatched %s (runs in %s)\n", resp.GetInstanceId(), cwd)
			if r := resp.GetRoute(); r != nil {
				fmt.Fprintf(out, "route: %s\n", taskfmt.Route(r))
			}
			if !resp.GetWorkerEnabled() {
				fmt.Fprintln(out, "note: the queue worker is disabled, so this task stays pending until you run\n"+
					"      `orch config set queue.worker.enabled true` and `orch restart`.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "dev", "target agent")
	cmd.Flags().StringVar(&cwd, "cwd", "", "directory the agent session runs in (default: current directory)")
	cmd.Flags().StringVar(&kind, "kind", "", "task kind to route by (code, review, docs, research, ops, classify, test, adversarial)")
	cmd.Flags().StringVar(&modelRef, "model", "", "run on this enabled model (<provider>/<slug>) instead of routing")
	return cmd
}

func newSearchCmd(cf clientFactory) *cobra.Command {
	var (
		topK       int
		scope      string
		sourceType string
		tags       []string
		allTags    []string
		uri        string
		mode       string
		ranking    string
		perSource  int
		since      string
		until      string
		dms        bool
	)
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the RAG store",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			query := args[0]
			for _, a := range args[1:] {
				query += " " + a
			}
			from, to, err := parseBounds(since, until)
			if err != nil {
				return err
			}
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			req := &orchdv1.SearchRequest{
				Query:      query,
				TopK:       int32(topK),
				Scope:      scope,
				SourceType: sourceType,
				Tags:       tags,
				AllTags:    allTags,
				SourceUri:  uri,
				Mode:       mode,
				Ranking:    ranking,
				PerSource:  int32(perSource),
				Since:      from,
				Until:      to,
			}
			var resp *orchdv1.SearchResponse
			if dms {
				resp, err = client.SearchDirectMessages(ctx, &orchdv1.SearchDirectMessagesRequest{Search: req})
			} else {
				resp, err = client.Search(ctx, req)
			}
			if err != nil {
				return err
			}
			if f := resp.GetFallback(); f != "" {
				fmt.Fprintf(c.ErrOrStderr(), "note: %s\n", f)
			}
			hits := resp.GetHits()
			if len(hits) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no results")
				return nil
			}
			for i, h := range hits {
				text := h.GetText()
				if len(text) > 200 {
					text = text[:200] + "…"
				}
				match := ""
				switch {
				case h.GetExact():
					match = " exact"
				case h.GetKeywordScore() > 0:
					match = " keyword"
				}
				when := timeline.Describe(h.GetStartedAt(), h.GetEndedAt(), h.GetSourceType())
				if when != "" {
					when = " · " + when
				}
				// Stored fields are the writer's choice: the URI and type are
				// quoted and the text kept on one line, so neither can pass for
				// another result when an agent reads this output.
				fmt.Fprintf(c.OutOrStdout(), "%d. [%.3f%s] %s (%s)%s\n   %s\n",
					i+1, h.GetScore(), match, memctx.QuoteField(h.GetSourceUri()), memctx.QuoteField(h.GetSourceType()), when, memctx.OneLine(text))
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&topK, "top-k", 5, "number of results")
	cmd.Flags().StringVar(&scope, "scope", "", "filter by scope")
	cmd.Flags().StringVar(&sourceType, "type", "", "filter by source type")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "filter: any of these tags")
	cmd.Flags().StringSliceVar(&allTags, "require-tag", nil, "filter: every one of these tags (e.g. owner:willesq,repo:orchestrator)")
	cmd.Flags().StringVar(&uri, "uri", "", "filter: only this source (exact source URI)")
	cmd.Flags().StringVar(&mode, "mode", "", "hybrid (keyword + semantic) or dense; default rag.search.mode")
	cmd.Flags().StringVar(&ranking, "ranking", "", "weighted (source type and recency) or plain; default rag.search.ranking")
	cmd.Flags().IntVar(&perSource, "per-source", 0, "most results one source may return (default rag.search.per_source; -1 for no cap)")
	cmd.Flags().BoolVar(&dms, "direct-messages", false, "also search chat direct messages, which no other search returns (the operator only: Orch.app while a spaces policy is applied)")
	addBoundFlags(cmd, &since, &until)
	return cmd
}

// newConfigResetCmd puts settings back to their defaults without orchd: the way
// out when a setting stops orchd from running at all, where the app cannot
// reach it.
func newConfigResetCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "reset [key...]",
		Short: "Reset settings to their defaults (--all for every one); restart orchd to apply",
		RunE: func(c *cobra.Command, args []string) error {
			if all == (len(args) > 0) {
				return errors.New("name the settings to reset, or pass --all")
			}
			conn, err := db.Open()
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			ctx := c.Context()
			keys := args
			if all {
				keys, err = config.ResetAllStore(ctx, conn)
			} else {
				err = config.ResetStore(ctx, conn, args, true, "cli:reset")
			}
			if err != nil {
				return err
			}
			if len(keys) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "every setting already had its default")
				return nil
			}
			fmt.Fprintf(c.OutOrStdout(), "reset %s; run `orch restart` for orchd to use the defaults\n", strings.Join(keys, ", "))
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "reset every setting")
	return operatorOnlyMsg("queue tasks cannot change orch's configuration; report it in the task result instead", cmd)
}
