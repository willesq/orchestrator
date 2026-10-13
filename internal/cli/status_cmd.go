package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/check"
	"github.com/willesq/orchestrator/internal/config"
	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/install"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/status"
)

// newStatusCmd builds `orch status`: the full sitrep across every component.
// It deliberately never exits non-zero — `orch check` owns the pass/fail gate —
// and it stays useful with orchd down, which is when it is usually run.
func newStatusCmd(cf clientFactory) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Full status report for every component of the orchestrator",
		Long: `Report the live state of every component: the orchd service, each
supervised subsystem (qdrant engine, workflow worker, sources and docs schedulers,
PR babysitter), the vector store, the on-disk stores and their freshness, harness
	wiring, repository instructions, and the daemon-independent preconditions.

Exits zero even when components are down; use 'orch check' for a pass/fail gate.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := settings()
			if err != nil {
				return err
			}
			report := status.Collect(c.Context(), statusDeps(cf, s))
			if asJSON {
				return report.RenderJSON(c.OutOrStdout())
			}
			report.Render(c.OutOrStdout())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	return cmd
}

// settings loads the resolved configuration.
func settings() (config.Settings, error) {
	v, err := config.Load()
	if err != nil {
		return config.Settings{}, err
	}
	return config.Unmarshal(v)
}

// statusDeps wires the real collectors the status report reads the world through.
func statusDeps(cf clientFactory, s config.Settings) status.Deps {
	return status.Deps{
		Settings: s,
		Orchd: func(ctx context.Context) (*orchdv1.StatusResponse, error) {
			client, cleanup, err := cf()
			if err != nil {
				return nil, err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			return client.Status(ctx, &orchdv1.StatusRequest{})
		},
		QdrantReady:            func(ctx context.Context) bool { return qdrantReady(ctx, s.QdrantURL()) },
		Service:                serviceInfo,
		Checks:                 check.Preconditions(),
		Store:                  db.OpenReader,
		Harness:                harnessInfo,
		RepositoryInstructions: repositoryInstructions,
		InstructionLoad:        instructionLoad,
	}
}

// qdrantReady probes the engine's readiness endpoint at whatever URL is in play,
// which (unlike the provisioner's localhost probe) also covers external engines.
// Redirects are not followed: a readiness check should answer for the host we
// configured, not for wherever it points us.
func qdrantReady(ctx context.Context, baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.JoinPath("readyz").String(), nil)
	if err != nil {
		return false
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// serviceInfo describes how orchd is managed: Homebrew's services when installed
// that way, else the launchd agent plist `orch install` writes.
func serviceInfo() status.ServiceInfo {
	if daemonIsHomebrew() {
		return status.ServiceInfo{Manager: "brew services", Detail: "orch formula", Installed: true}
	}
	plist := orchdPlistPath()
	if _, err := os.Stat(plist); err != nil {
		return status.ServiceInfo{Manager: "launchd", Detail: "plist absent: " + plist}
	}
	return status.ServiceInfo{Manager: "launchd", Detail: plist, Installed: true}
}

// harnessInfo reports, per coding-agent harness, how many install edits are still
// pending — including shared instructions, native entry files, hooks and MCP.
func harnessInfo() []status.HarnessInfo {
	home, err := os.UserHomeDir()
	if err != nil {
		return []status.HarnessInfo{{Name: "all", Err: err.Error()}}
	}
	bins, err := installBins("")
	if err != nil {
		return []status.HarnessInfo{{Name: "all", Err: err.Error()}}
	}
	out := make([]status.HarnessInfo, 0, len(install.AllAdapters()))
	for _, a := range install.AllAdapters() {
		info := status.HarnessInfo{Name: a.Name()}
		n, err := install.BuildPlan(a, bins, home).Pending()
		if err != nil {
			info.Err = err.Error()
		} else {
			info.Pending = n
		}
		out = append(out, info)
	}
	return out
}

// installBins resolves the binary paths install plans point at: the given dir, or
// the directory of the running executable.
func installBins(binDir string) (install.Bins, error) {
	if binDir == "" {
		exe, err := os.Executable()
		if err != nil {
			return install.Bins{}, fmt.Errorf("resolve executable: %w", err)
		}
		binDir = filepath.Dir(exe)
	}
	return install.Bins{
		Orch:     filepath.Join(binDir, "orch"),
		OrchHook: filepath.Join(binDir, "orch-hook"),
		Orchd:    filepath.Join(binDir, "orchd"),
	}, nil
}
