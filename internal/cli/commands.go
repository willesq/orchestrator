package cli

import (
	"context"
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

	"github.com/willesq/orchestrator/internal/check"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/install"
	"github.com/willesq/orchestrator/internal/mcpserver"
	"github.com/willesq/orchestrator/internal/model"
	"github.com/willesq/orchestrator/internal/paths"
	"github.com/willesq/orchestrator/internal/qdrant"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
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
	cmd := &cobra.Command{Use: "model", Short: "Manage the embedding model files"}
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
			if binDir == "" {
				exe, err := os.Executable()
				if err != nil {
					return err
				}
				binDir = filepath.Dir(exe)
			}
			bins := install.Bins{
				Orch:     filepath.Join(binDir, "orch"),
				OrchHook: filepath.Join(binDir, "orch-hook"),
				Orchd:    filepath.Join(binDir, "orchd"),
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
	cmd.Flags().StringVar(&target, "target", "claude-code", "harness: claude-code")
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
	cmd := &cobra.Command{Use: "config", Short: "Get, set, and list configuration"}
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
		&cobra.Command{
			Use:   "set <key> <value>",
			Short: "Set a config value and persist it",
			Args:  cobra.ExactArgs(2),
			RunE: func(c *cobra.Command, args []string) error {
				v, err := config.Load()
				if err != nil {
					return err
				}
				if err := config.Set(v, args[0], args[1]); err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "set %s = %s\n", args[0], args[1])
				return nil
			},
		},
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
				resp, err := doPing(cf, 2*time.Second)
				if err != nil {
					fmt.Fprintln(c.OutOrStdout(), "orchd: not running")
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
			RunE:  func(c *cobra.Command, _ []string) error { return daemonControl(c.OutOrStdout(), "start") },
		},
		&cobra.Command{
			Use:   "stop",
			Short: "Stop orchd (brew services when Homebrew-managed, else launchd)",
			Args:  cobra.NoArgs,
			RunE:  func(c *cobra.Command, _ []string) error { return daemonControl(c.OutOrStdout(), "stop") },
		},
		&cobra.Command{
			Use:   "restart",
			Short: "Restart orchd (brew services when Homebrew-managed, else launchd)",
			Args:  cobra.NoArgs,
			RunE:  func(c *cobra.Command, _ []string) error { return daemonControl(c.OutOrStdout(), "restart") },
		},
	)
	return cmd
}

// runCommand runs an external command; overridable in tests.
var runCommand = func(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

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

// daemonControl performs a lifecycle op (start|stop|restart) against whatever
// supervises orchd: `brew services` for a Homebrew install, launchctl otherwise.
// This keeps the lifecycle commands consistent with how the daemon was started.
func daemonControl(w io.Writer, op string) error {
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
	textCmd := &cobra.Command{
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
	}
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
	var agent string
	cmd := &cobra.Command{
		Use:   "dispatch <body>",
		Short: "Dispatch a durable agent task",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
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
			resp, err := client.Dispatch(ctx, &orchdv1.DispatchRequest{Agent: agent, Body: body})
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "dispatched %s\n", resp.GetInstanceId())
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "dev", "target agent")
	return cmd
}

func newSearchCmd(cf clientFactory) *cobra.Command {
	var (
		topK       int
		scope      string
		sourceType string
		tags       []string
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
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resp, err := client.Search(ctx, &orchdv1.SearchRequest{
				Query:      query,
				TopK:       int32(topK),
				Scope:      scope,
				SourceType: sourceType,
				Tags:       tags,
			})
			if err != nil {
				return err
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
				fmt.Fprintf(c.OutOrStdout(), "%d. [%.3f] %s (%s)\n   %s\n",
					i+1, h.GetScore(), h.GetSourceUri(), h.GetSourceType(), text)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&topK, "top-k", 5, "number of results")
	cmd.Flags().StringVar(&scope, "scope", "", "filter by scope")
	cmd.Flags().StringVar(&sourceType, "type", "", "filter by source type")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "filter: any of these tags")
	return cmd
}
