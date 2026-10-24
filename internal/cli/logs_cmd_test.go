package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func logEntry(seq uint64, source, msg string) *orchdv1.LogEntry {
	return &orchdv1.LogEntry{Seq: seq, AtUnixMs: time.Date(2026, 10, 23, 9, 30, 0, 0, time.Local).UnixMilli(), Source: source, Message: msg}
}

func TestLogsCmd(t *testing.T) {
	saved := logsPollInterval
	logsPollInterval = time.Millisecond
	t.Cleanup(func() { logsPollInterval = saved })
	ended := errors.New("connection closed")

	tests := []struct {
		name      string
		args      []string
		tails     []*orchdv1.TailLogsResponse
		dialErr   error
		clientErr error
		wantErr   string
		wantSub   []string
		wantReqs  []*orchdv1.TailLogsRequest
	}{
		{
			name: "prints recent lines once",
			args: []string{"logs", "-n", "2", "--source", "qdrant"},
			tails: []*orchdv1.TailLogsResponse{{
				Entries: []*orchdv1.LogEntry{logEntry(4, "qdrant", "listening"), logEntry(5, "qdrant", "ready")},
				LastSeq: 9, OldestSeq: 1,
			}},
			wantSub:  []string{"2026-10-23 09:30:00 qdrant", "listening", "ready"},
			wantReqs: []*orchdv1.TailLogsRequest{{Source: "qdrant", Limit: 2}},
		},
		{
			name: "follow polls after the last sequence and notes dropped lines",
			args: []string{"logs", "-f"},
			tails: []*orchdv1.TailLogsResponse{
				{Entries: []*orchdv1.LogEntry{logEntry(3, "orchd", "up")}, LastSeq: 3, OldestSeq: 1},
				{LastSeq: 3, OldestSeq: 1},
				{Entries: []*orchdv1.LogEntry{logEntry(20, "pr-babysitter", "swept")}, LastSeq: 20, OldestSeq: 10},
			},
			wantErr: "connection closed",
			wantSub: []string{"up", "... 6 lines dropped", "pr-babysitter", "swept"},
			wantReqs: []*orchdv1.TailLogsRequest{
				{Limit: 100},
				{AfterSeq: 3, Limit: maxFollowLines},
				{AfterSeq: 3, Limit: maxFollowLines},
				{AfterSeq: 20, Limit: maxFollowLines},
			},
		},
		{
			name:    "reports a daemon that cannot be reached",
			args:    []string{"logs"},
			dialErr: errors.New("dial unix: no such file"),
			wantErr: "no such file",
		},
		{
			name:      "reports a failing call",
			args:      []string{"logs"},
			clientErr: errors.New("unavailable: orchd is not keeping logs"),
			wantErr:   "not keeping logs",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &fakeClient{tails: tt.tails, tailErr: ended, err: tt.clientErr}
			out, err := run(factory(fc, tt.dialErr), "", tt.args...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("logs: %v", err)
			}
			for _, s := range tt.wantSub {
				if !strings.Contains(out, s) {
					t.Errorf("output missing %q:\n%s", s, out)
				}
			}
			if tt.wantReqs == nil {
				return
			}
			if len(fc.tailReqs) != len(tt.wantReqs) {
				t.Fatalf("requests = %v, want %v", fc.tailReqs, tt.wantReqs)
			}
			for i, want := range tt.wantReqs {
				got := fc.tailReqs[i]
				if got.GetAfterSeq() != want.GetAfterSeq() || got.GetSource() != want.GetSource() || got.GetLimit() != want.GetLimit() {
					t.Errorf("request %d = %v, want %v", i, got, want)
				}
			}
		})
	}
}
