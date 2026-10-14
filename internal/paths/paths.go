// Package paths resolves the on-disk locations the orchestrator uses: the config
// directory, the data directory (SQLite stores, the orchd socket), with env overrides.
//
// Layout (macOS/XDG):
//
//	config:  $XDG_CONFIG_HOME/orch  (default ~/.config/orch)
//	data:    $XDG_DATA_HOME/orch    (default ~/.local/share/orch)
//	socket:  <data>/orchd.sock
package paths

import (
	"os"
	"path/filepath"
)

// ConfigDir returns the directory holding config.yaml.
func ConfigDir() string {
	if d := os.Getenv("ORCH_CONFIG_DIR"); d != "" {
		return d
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "orch")
}

// DataDir returns the directory holding runtime state (SQLite dbs, socket).
func DataDir() string {
	if d := os.Getenv("ORCH_DATA_DIR"); d != "" {
		return d
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "orch")
}

// ConfigFile is the absolute path to config.yaml.
func ConfigFile() string { return filepath.Join(ConfigDir(), "config.yaml") }

// DBFile is the absolute path to the orch.db SQLite store.
func DBFile() string { return filepath.Join(DataDir(), "orch.db") }

// SessionsDir holds per-session records + checkpoints.
func SessionsDir() string { return filepath.Join(DataDir(), "sessions") }

// EventsDir holds per-session + shared event logs (JSONL).
func EventsDir() string { return filepath.Join(DataDir(), "events") }

// PendingIngestDir holds detected-but-undecided URLs/paths per session.
func PendingIngestDir() string { return filepath.Join(DataDir(), "pending_ingest") }

// PRStateFile is the PR-babysitter's cached state.
func PRStateFile() string { return filepath.Join(DataDir(), "pr_babysitter_state.json") }

// RoutingEvaluatorStateFile records the routing evaluator's last run and report.
func RoutingEvaluatorStateFile() string {
	return filepath.Join(DataDir(), "routing_evaluator_state.json")
}

// SocketPath is the absolute path to the orchd Unix-domain socket.
func SocketPath() string {
	if s := os.Getenv("ORCH_SOCKET"); s != "" {
		return s
	}
	return filepath.Join(DataDir(), "orchd.sock")
}

// Ensure creates the config and data directories if they do not exist.
func Ensure() error {
	for _, d := range []string{ConfigDir(), DataDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
