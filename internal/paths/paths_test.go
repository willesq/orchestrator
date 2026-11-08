package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	tests := []struct {
		name    string
		envKey  string
		envVal  string
		want    string
		wantCfg string
	}{
		{
			name:    "explicit override",
			envKey:  "ORCH_CONFIG_DIR",
			envVal:  "/tmp/x/config",
			want:    "/tmp/x/config",
			wantCfg: "/tmp/x/config/config.yaml",
		},
		{
			name:    "xdg fallback",
			envKey:  "XDG_CONFIG_HOME",
			envVal:  "/tmp/xdgc",
			want:    filepath.Join("/tmp/xdgc", "orch"),
			wantCfg: filepath.Join("/tmp/xdgc", "orch", "config.yaml"),
		},
		{
			name:    "home fallback when nothing set",
			envKey:  "XDG_CONFIG_HOME",
			envVal:  "",
			want:    filepath.Join(home, ".config", "orch"),
			wantCfg: filepath.Join(home, ".config", "orch", "config.yaml"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", "")
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			t.Setenv(tt.envKey, tt.envVal)
			if got := ConfigDir(); got != tt.want {
				t.Errorf("ConfigDir() = %q, want %q", got, tt.want)
			}
			if got := LegacyConfigFile(); got != tt.wantCfg {
				t.Errorf("LegacyConfigFile() = %q, want %q", got, tt.wantCfg)
			}
		})
	}
}

func TestDataDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	tests := []struct {
		name       string
		envKey     string
		envVal     string
		wantData   string
		wantSocket string
		wantDB     string
	}{
		{
			name:       "explicit override",
			envKey:     "ORCH_DATA_DIR",
			envVal:     "/tmp/d",
			wantData:   "/tmp/d",
			wantSocket: filepath.Join("/tmp/d", "orchd.sock"),
			wantDB:     filepath.Join("/tmp/d", "orch.db"),
		},
		{
			name:       "xdg fallback",
			envKey:     "XDG_DATA_HOME",
			envVal:     "/tmp/xdgd",
			wantData:   filepath.Join("/tmp/xdgd", "orch"),
			wantSocket: filepath.Join("/tmp/xdgd", "orch", "orchd.sock"),
			wantDB:     filepath.Join("/tmp/xdgd", "orch", "orch.db"),
		},
		{
			name:       "home fallback when nothing set",
			envKey:     "XDG_DATA_HOME",
			envVal:     "",
			wantData:   filepath.Join(home, ".local", "share", "orch"),
			wantSocket: filepath.Join(home, ".local", "share", "orch", "orchd.sock"),
			wantDB:     filepath.Join(home, ".local", "share", "orch", "orch.db"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", "")
			t.Setenv("ORCH_SOCKET", "")
			t.Setenv(tt.envKey, tt.envVal)
			if got := DataDir(); got != tt.wantData {
				t.Errorf("DataDir() = %q, want %q", got, tt.wantData)
			}
			if got := SocketPath(); got != tt.wantSocket {
				t.Errorf("SocketPath() = %q, want %q", got, tt.wantSocket)
			}
			if got := DBFile(); got != tt.wantDB {
				t.Errorf("DBFile() = %q, want %q", got, tt.wantDB)
			}
		})
	}
}

func TestSubDirs(t *testing.T) {
	tests := []struct {
		name string
		got  func() string
		want string
	}{
		{name: "sessions", got: SessionsDir, want: filepath.Join("/tmp/d", "sessions")},
		{name: "events", got: EventsDir, want: filepath.Join("/tmp/d", "events")},
		{name: "pending ingest", got: PendingIngestDir, want: filepath.Join("/tmp/d", "pending_ingest")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_DATA_DIR", "/tmp/d")
			if got := tt.got(); got != tt.want {
				t.Errorf("%s = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestSocketEnvOverride(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want string
	}{
		{name: "socket override wins", env: "/tmp/custom.sock", want: "/tmp/custom.sock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_SOCKET", tt.env)
			if got := SocketPath(); got != tt.want {
				t.Errorf("SocketPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnsureCreatesDirs(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "creates config and data dirs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCH_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
			t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
			if err := Ensure(); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			for _, d := range []string{ConfigDir(), DataDir()} {
				if _, err := os.Stat(d); err != nil {
					t.Errorf("dir %s not created: %v", d, err)
				}
			}
		})
	}
}
