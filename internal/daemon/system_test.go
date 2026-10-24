package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/willesq/orchestrator/internal/logs"
	"github.com/willesq/orchestrator/internal/procstat"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestTailLogs(t *testing.T) {
	ring := logs.NewRing(10)
	for i := 0; i < 4; i++ {
		ring.Add("orchd", "o")
	}
	ring.Add("qdrant", "q1")
	ring.Add("qdrant", "q2")

	tests := []struct {
		name       string
		srv        *server
		req        *orchdv1.TailLogsRequest
		wantCode   codes.Code
		wantSeqs   []uint64
		wantLast   uint64
		wantOldest uint64
	}{
		{name: "no probe is unavailable", srv: &server{}, req: &orchdv1.TailLogsRequest{}, wantCode: codes.Unavailable},
		{name: "a probe without logs is unavailable", srv: &server{system: &systemProbe{}}, req: &orchdv1.TailLogsRequest{}, wantCode: codes.Unavailable},
		{
			name: "everything after a sequence", srv: &server{system: newSystemProbe(ring)},
			req: &orchdv1.TailLogsRequest{AfterSeq: 3}, wantSeqs: []uint64{4, 5, 6}, wantLast: 6, wantOldest: 1,
		},
		{
			name: "one source, newest limit", srv: &server{system: newSystemProbe(ring)},
			req: &orchdv1.TailLogsRequest{Source: "orchd", Limit: 2}, wantSeqs: []uint64{3, 4}, wantLast: 6, wantOldest: 1,
		},
		{
			name: "an oversized limit is capped, not refused", srv: &server{system: newSystemProbe(ring)},
			req: &orchdv1.TailLogsRequest{Source: "qdrant", Limit: 1 << 30}, wantSeqs: []uint64{5, 6}, wantLast: 6, wantOldest: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := tt.srv.TailLogs(context.Background(), tt.req)
			if tt.wantCode != codes.OK {
				if grpcstatus.Code(err) != tt.wantCode {
					t.Fatalf("err = %v, want %v", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("TailLogs: %v", err)
			}
			var seqs []uint64
			for _, e := range resp.GetEntries() {
				seqs = append(seqs, e.GetSeq())
				if e.GetAtUnixMs() == 0 || e.GetSource() == "" {
					t.Errorf("entry %v missing time or source", e)
				}
			}
			if len(seqs) != len(tt.wantSeqs) {
				t.Fatalf("seqs = %v, want %v", seqs, tt.wantSeqs)
			}
			for i := range seqs {
				if seqs[i] != tt.wantSeqs[i] {
					t.Fatalf("seqs = %v, want %v", seqs, tt.wantSeqs)
				}
			}
			if resp.GetLastSeq() != tt.wantLast || resp.GetOldestSeq() != tt.wantOldest {
				t.Errorf("last, oldest = %d, %d, want %d, %d", resp.GetLastSeq(), resp.GetOldestSeq(), tt.wantLast, tt.wantOldest)
			}
			if got := resp.GetSources(); len(got) != 2 || got[0] != "orchd" || got[1] != "qdrant" {
				t.Errorf("sources = %v", got)
			}
		})
	}
}

func TestGetSystemMetricsUnavailable(t *testing.T) {
	_, err := (&server{}).GetSystemMetrics(context.Background(), &orchdv1.GetSystemMetricsRequest{})
	if grpcstatus.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v, want Unavailable", err)
	}
}

// fakeProcs serves procstat reads from a table, advancing CPU per read.
type fakeProcs struct {
	stats map[int]procstat.Stats
	reads int
}

func (f *fakeProcs) read(pid int) (procstat.Stats, error) {
	st, ok := f.stats[pid]
	if !ok {
		return procstat.Stats{}, errors.New("no such process")
	}
	f.reads++
	return st, nil
}

func TestSystemMetrics(t *testing.T) {
	start := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	self := os.Getpid()
	dir := t.TempDir()
	dbFile := filepath.Join(dir, "orch.db")
	writeSized(t, dbFile, 100)
	writeSized(t, dbFile+"-wal", 20)
	store := filepath.Join(dir, "qdrant")
	writeSized(t, filepath.Join(store, "a", "seg"), 300)
	writeSized(t, filepath.Join(store, "b"), 5)
	if err := os.Symlink(dbFile, filepath.Join(store, "link")); err != nil {
		t.Fatal(err)
	}

	procs := &fakeProcs{stats: map[int]procstat.Stats{
		self: {PID: self, RSSBytes: 64 << 20, CPUSeconds: 30, Started: start},
		4242: {PID: 4242, RSSBytes: 512 << 20, CPUSeconds: 60, Started: start.Add(-time.Minute)},
	}}
	now := start.Add(time.Minute)
	qpid := 4242
	p := &systemProbe{
		qdrantPID: func() int { return qpid },
		data: []dataPath{
			{name: "database", path: dbFile},
			{name: "qdrant", path: store},
			{name: "models", path: filepath.Join(dir, "missing")},
		},
		tasks: func(context.Context) (map[string]int, error) { return map[string]int{"pending": 2, "done": 7}, nil },
		read:  procs.read,
		find:  func(string) (int, error) { t.Fatal("find called with a provisioner PID"); return 0, nil },
		now:   func() time.Time { return now },
		prev:  map[string]sample{},
	}
	srv := &server{system: p}

	m, err := srv.GetSystemMetrics(context.Background(), &orchdv1.GetSystemMetricsRequest{})
	if err != nil {
		t.Fatalf("GetSystemMetrics: %v", err)
	}
	if m.GetGoroutines() <= 0 || m.GetHeapBytes() == 0 || m.GetAtUnixMs() != now.UnixMilli() {
		t.Errorf("runtime fields = %d goroutines, %d heap, at %d", m.GetGoroutines(), m.GetHeapBytes(), m.GetAtUnixMs())
	}
	byName := processes(m)
	// First sample: CPU over the process lifetime. orchd: 30s in 60s = 50%.
	if o := byName["orchd"]; !o.GetRunning() || o.GetPid() != int32(self) || o.GetRssBytes() != 64<<20 ||
		o.GetUptimeSeconds() != 60 || o.GetCpuPercent() != 50 {
		t.Errorf("orchd = %v", o)
	}
	// qdrant: 60s in 120s = 50%.
	if q := byName["qdrant"]; !q.GetRunning() || q.GetPid() != 4242 || q.GetUptimeSeconds() != 120 || q.GetCpuPercent() != 50 {
		t.Errorf("qdrant = %v", q)
	}
	sizes := map[string]uint64{}
	for _, d := range m.GetData() {
		sizes[d.GetName()] = d.GetBytes()
	}
	if sizes["database"] != 120 || sizes["qdrant"] != 305 || sizes["models"] != 0 {
		t.Errorf("sizes = %v, want database 120 (with -wal), qdrant 305 (symlink not followed), models 0", sizes)
	}
	if m.GetTasks()["pending"] != 2 || m.GetTasks()["done"] != 7 {
		t.Errorf("tasks = %v", m.GetTasks())
	}

	// Second sample 10s later: CPU since the first. qdrant used 2.5s more = 25%.
	now = now.Add(10 * time.Second)
	q := procs.stats[4242]
	q.CPUSeconds += 2.5
	procs.stats[4242] = q
	writeSized(t, filepath.Join(store, "c"), 1000)
	p.tasks = func(context.Context) (map[string]int, error) { return nil, errors.New("database is locked") }
	m, err = srv.GetSystemMetrics(context.Background(), &orchdv1.GetSystemMetricsRequest{})
	if err != nil {
		t.Fatalf("GetSystemMetrics: %v", err)
	}
	if got := processes(m)["qdrant"].GetCpuPercent(); got != 25 {
		t.Errorf("qdrant cpu = %v, want 25", got)
	}
	if got := processes(m)["orchd"].GetCpuPercent(); got != 0 {
		t.Errorf("orchd cpu = %v, want 0 (no CPU used since the last sample)", got)
	}
	if m.GetTasks() != nil {
		t.Errorf("tasks = %v, want none when the count fails", m.GetTasks())
	}
	for _, d := range m.GetData() {
		if d.GetName() == "qdrant" && d.GetBytes() != 305 {
			t.Errorf("qdrant size = %d, want the cached 305 within the TTL", d.GetBytes())
		}
	}

	// Past the TTL the sizes are measured again.
	now = now.Add(dataSizeTTL)
	m, _ = srv.GetSystemMetrics(context.Background(), &orchdv1.GetSystemMetricsRequest{})
	for _, d := range m.GetData() {
		if d.GetName() == "qdrant" && d.GetBytes() != 1305 {
			t.Errorf("qdrant size = %d, want 1305 after the TTL", d.GetBytes())
		}
	}

	// qdrant stopped: reported not running, and its sample forgotten.
	qpid = 0
	m, _ = srv.GetSystemMetrics(context.Background(), &orchdv1.GetSystemMetricsRequest{})
	if q := processes(m)["qdrant"]; q.GetRunning() || q.GetPid() != 0 {
		t.Errorf("stopped qdrant = %v", q)
	}
	if _, ok := p.prev["qdrant"]; ok {
		t.Error("stopped qdrant's sample kept")
	}
}

func TestSystemProbeFindsQdrantByName(t *testing.T) {
	tests := []struct {
		name        string
		find        func(string) (int, error)
		stats       map[int]procstat.Stats
		wantRunning bool
	}{
		{
			name:        "found and readable",
			find:        func(n string) (int, error) { return 77, nil },
			stats:       map[int]procstat.Stats{77: {PID: 77}},
			wantRunning: true,
		},
		{name: "lookup fails", find: func(string) (int, error) { return 0, procstat.ErrUnsupported }},
		{name: "found but gone before the read", find: func(string) (int, error) { return 78, nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			procs := &fakeProcs{stats: tt.stats}
			if procs.stats == nil {
				procs.stats = map[int]procstat.Stats{}
			}
			procs.stats[os.Getpid()] = procstat.Stats{PID: os.Getpid()}
			p := newSystemProbe(nil)
			p.find, p.read = tt.find, procs.read
			q := processes(p.metrics(context.Background()))["qdrant"]
			if q.GetRunning() != tt.wantRunning {
				t.Errorf("qdrant = %v, want running %v", q, tt.wantRunning)
			}
		})
	}
}

func TestSystemProbeReadsItself(t *testing.T) {
	p := newSystemProbe(nil)
	p.find = func(string) (int, error) { return 0, procstat.ErrUnsupported }
	o := processes(p.metrics(context.Background()))["orchd"]
	if !o.GetRunning() || o.GetRssBytes() == 0 {
		t.Errorf("orchd = %v, want this process's real stats", o)
	}
}

func processes(m *orchdv1.SystemMetrics) map[string]*orchdv1.ProcessMetrics {
	out := map[string]*orchdv1.ProcessMetrics{}
	for _, p := range m.GetProcesses() {
		out[p.GetName()] = p
	}
	return out
}

func writeSized(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTailLogsFitsTheMessageLimit(t *testing.T) {
	ring := logs.NewRing(maxTailLimit)
	long := strings.Repeat("x", 1999)
	for i := 0; i < maxTailLimit; i++ {
		ring.Add("qdrant", long)
	}
	resp, err := (&server{system: newSystemProbe(ring)}).TailLogs(context.Background(),
		&orchdv1.TailLogsRequest{Limit: maxTailLimit})
	if err != nil {
		t.Fatal(err)
	}
	if size := proto.Size(resp); size > 4<<20 {
		t.Errorf("reply is %d bytes, over gRPC's 4 MiB default", size)
	}
	entries := resp.GetEntries()
	if len(entries) == 0 || len(entries) == maxTailLimit {
		t.Fatalf("got %d entries, want some but not all", len(entries))
	}
	if last := entries[len(entries)-1].GetSeq(); last != resp.GetLastSeq() {
		t.Errorf("newest entry %d, want the newest line %d kept", last, resp.GetLastSeq())
	}
}

func TestPathSizeFollowsASymlinkedRoot(t *testing.T) {
	dir := t.TempDir()
	writeSized(t, filepath.Join(dir, "real", "seg"), 42)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "real"), link); err != nil {
		t.Fatal(err)
	}
	if got := pathSize(link); got != 42 {
		t.Errorf("pathSize(symlinked root) = %d, want 42", got)
	}
}
