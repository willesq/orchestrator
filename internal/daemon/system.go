package daemon

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/logs"
	"github.com/willesq/orchestrator/internal/procstat"
	"github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

const (
	defaultTailLimit = 500
	maxTailLimit     = 5000
	// maxTailBytes keeps a reply under gRPC's default 4 MiB receive limit;
	// 5000 lines at the 2000-character cap would not fit.
	maxTailBytes = 3 << 20
	// Walking the qdrant store on every poll would cost more than the numbers
	// are worth; sizes change slowly.
	dataSizeTTL = 30 * time.Second
)

// dataPath is one of orchd's on-disk stores. A path that is a file also counts
// the SQLite -wal and -shm files beside it.
type dataPath struct {
	name, path string
}

// systemProbe backs TailLogs and GetSystemMetrics. Every process lookup goes
// through internal/procstat, which reads the kernel directly: orchd starts no
// command to observe itself or its children.
type systemProbe struct {
	logs *logs.Ring
	// qdrantPID reports the qdrant orchd runs; nil when qdrant runs elsewhere,
	// and then it is looked up by name.
	qdrantPID func() int
	data      []dataPath
	tasks     func(context.Context) (map[string]int, error)

	read func(pid int) (procstat.Stats, error)
	find func(name string) (int, error)
	now  func() time.Time

	mu      sync.Mutex
	prev    map[string]sample
	sizes   []*orchdv1.DataSize
	sizedAt time.Time
}

type sample struct {
	stats procstat.Stats
	at    time.Time
}

func newSystemProbe(ring *logs.Ring) *systemProbe {
	return &systemProbe{
		logs: ring,
		read: procstat.Read,
		find: procstat.FindByName,
		now:  time.Now,
		prev: map[string]sample{},
	}
}

func (s *server) TailLogs(_ context.Context, req *orchdv1.TailLogsRequest) (*orchdv1.TailLogsResponse, error) {
	if s.system == nil || s.system.logs == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "orchd is not keeping logs")
	}
	limit := int(req.GetLimit())
	switch {
	case limit <= 0:
		limit = defaultTailLimit
	case limit > maxTailLimit:
		limit = maxTailLimit
	}
	ring := s.system.logs
	entries, last, oldest := ring.Since(req.GetAfterSeq(), req.GetSource(), limit)
	// Keep the newest lines that fit, as Since does for the line limit.
	size, keep := 0, len(entries)
	for keep > 0 && size+len(entries[keep-1].Message)+len(entries[keep-1].Source)+32 <= maxTailBytes {
		size += len(entries[keep-1].Message) + len(entries[keep-1].Source) + 32
		keep--
	}
	entries = entries[keep:]
	resp := &orchdv1.TailLogsResponse{LastSeq: last, OldestSeq: oldest, Sources: ring.Sources()}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, &orchdv1.LogEntry{
			Seq: e.Seq, AtUnixMs: e.Time.UnixMilli(), Source: e.Source, Message: e.Message,
		})
	}
	return resp, nil
}

func (s *server) GetSystemMetrics(ctx context.Context, _ *orchdv1.GetSystemMetricsRequest) (*orchdv1.SystemMetrics, error) {
	if s.system == nil {
		return nil, grpcstatus.Error(codes.Unavailable, "orchd is not reporting metrics")
	}
	return s.system.metrics(ctx), nil
}

func (p *systemProbe) metrics(ctx context.Context) *orchdv1.SystemMetrics {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	now := p.now()
	out := &orchdv1.SystemMetrics{
		AtUnixMs:   now.UnixMilli(),
		Goroutines: clampInt32(runtime.NumGoroutine()),
		HeapBytes:  mem.HeapAlloc,
		Processes: []*orchdv1.ProcessMetrics{
			p.process("orchd", os.Getpid(), now),
			p.process("qdrant", p.qdrant(), now),
		},
		Data: p.dataSizes(now),
	}
	if p.tasks != nil {
		// Counts are best effort: a busy database leaves them out of one poll
		// rather than failing the whole report.
		if counts, err := p.tasks(ctx); err == nil {
			out.Tasks = map[string]int32{}
			for k, v := range counts {
				out.Tasks[k] = clampInt32(v)
			}
		}
	}
	return out
}

func (p *systemProbe) qdrant() int {
	if p.qdrantPID != nil {
		return p.qdrantPID()
	}
	pid, err := p.find("qdrant")
	if err != nil {
		return 0
	}
	return pid
}

// process reports one process, with CPU measured since the previous call for
// the same name, or over its lifetime on the first.
func (p *systemProbe) process(name string, pid int, now time.Time) *orchdv1.ProcessMetrics {
	out := &orchdv1.ProcessMetrics{Name: name}
	if pid <= 0 {
		p.forget(name)
		return out
	}
	st, err := p.read(pid)
	if err != nil {
		p.forget(name)
		return out
	}
	out.Pid = clampInt32(pid)
	out.Running = true
	out.RssBytes = st.RSSBytes
	if !st.Started.IsZero() {
		out.UptimeSeconds = int64(now.Sub(st.Started).Seconds())
	}

	p.mu.Lock()
	prev, ok := p.prev[name]
	p.prev[name] = sample{stats: st, at: now}
	p.mu.Unlock()
	if ok && prev.stats.PID == pid {
		out.CpuPercent = procstat.CPUPercent(prev.stats, st, now.Sub(prev.at))
	} else if !st.Started.IsZero() {
		out.CpuPercent = procstat.CPUPercent(procstat.Stats{PID: pid}, st, now.Sub(st.Started))
	}
	return out
}

func (p *systemProbe) forget(name string) {
	p.mu.Lock()
	delete(p.prev, name)
	p.mu.Unlock()
}

func (p *systemProbe) dataSizes(now time.Time) []*orchdv1.DataSize {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sizes != nil && now.Sub(p.sizedAt) < dataSizeTTL {
		return p.sizes
	}
	sizes := make([]*orchdv1.DataSize, 0, len(p.data))
	for _, d := range p.data {
		sizes = append(sizes, &orchdv1.DataSize{Name: d.name, Path: d.path, Bytes: pathSize(d.path)})
	}
	p.sizes, p.sizedAt = sizes, now
	return sizes
}

// pathSize is the bytes under path: a directory's files, or a file plus its
// SQLite journal files. Missing or unreadable entries count as zero. The root
// comes from config and may be a symlink (a store moved to another disk), so it
// is followed; links inside the store are not.
func pathSize(path string) uint64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if !info.IsDir() {
		total := uint64(info.Size())
		for _, suffix := range []string{"-wal", "-shm"} {
			if fi, err := os.Lstat(path + suffix); err == nil && fi.Mode().IsRegular() {
				total += uint64(fi.Size())
			}
		}
		return total
	}
	var total uint64
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// Regular files only: a symlink is not followed out of the store.
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += uint64(fi.Size())
			}
		}
		return nil
	})
	return total
}
