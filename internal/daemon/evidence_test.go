package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/willesq/orchestrator/internal/db"
	"github.com/willesq/orchestrator/internal/evidence"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

type fakeRater struct{ err error }

func (f fakeRater) Rate(context.Context, string, string) error { return f.err }

func TestRateTask(t *testing.T) {
	tests := []struct {
		name     string
		rating   string
		rater    taskRater
		wantCode codes.Code
	}{
		{name: "records", rating: "good", rater: fakeRater{}},
		{name: "bad label", rating: "great", rater: fakeRater{}, wantCode: codes.InvalidArgument},
		{name: "unknown task", rating: "bad", rater: fakeRater{err: fmt.Errorf("%w: x", evidence.ErrNoTask)}, wantCode: codes.NotFound},
		{name: "unfinished task", rating: "bad", rater: fakeRater{err: errors.New("task x is pending")}, wantCode: codes.FailedPrecondition},
		{name: "unavailable", rating: "good", wantCode: codes.Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{rater: tt.rater}
			_, err := s.RateTask(context.Background(), &orchdv1.RateTaskRequest{Id: "x", Rating: tt.rating})
			if grpcstatus.Code(err) != tt.wantCode {
				t.Fatalf("err = %v, want %v", err, tt.wantCode)
			}
		})
	}
}

func TestDBRater(t *testing.T) {
	t.Setenv("ORCH_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	conn, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Exec(`INSERT INTO tasks (id, agent, body, result, status, updated_at) VALUES ('t', 'dev', 'b', '', 'complete', datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	if err := (dbRater{db: conn}).Rate(context.Background(), "t", "rework"); err != nil {
		t.Fatal(err)
	}
	var r string
	_ = conn.QueryRow(`SELECT rating FROM tasks WHERE id = 't'`).Scan(&r)
	if r != "rework" {
		t.Fatalf("rating = %q", r)
	}
}
