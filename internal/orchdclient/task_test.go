package orchdclient

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/willesq/orchestrator/internal/agentrun"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// pingRecorder records the task metadata each Ping arrives with.
type pingRecorder struct {
	orchdv1.UnimplementedOrchdServer
	got chan []string
}

func (p *pingRecorder) Ping(ctx context.Context, _ *orchdv1.PingRequest) (*orchdv1.PingResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	p.got <- md.Get(TaskMetadataKey)
	return &orchdv1.PingResponse{}, nil
}

func TestDialReportsTheTask(t *testing.T) {
	tests := []struct {
		name   string
		taskID string
		want   []string
	}{
		{name: "inside a queued task", taskID: "task-1", want: []string{"task-1"}},
		{name: "outside a task", taskID: "", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Short path for macOS's Unix socket limit.
			dir, err := os.MkdirTemp(os.TempDir(), "orch-cl-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			sock := filepath.Join(dir, "d.sock")
			lis, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			rec := &pingRecorder{got: make(chan []string, 1)}
			gs := grpc.NewServer()
			orchdv1.RegisterOrchdServer(gs, rec)
			go func() { _ = gs.Serve(lis) }()
			t.Cleanup(gs.Stop)

			t.Setenv("ORCH_SOCKET", sock)
			t.Setenv(agentrun.TaskIDEnv, tt.taskID)
			client, cleanup, err := Dial()
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if _, err := client.Ping(context.Background(), &orchdv1.PingRequest{}); err != nil {
				t.Fatal(err)
			}
			got := <-rec.got
			if len(got) != len(tt.want) || (len(got) > 0 && got[0] != tt.want[0]) {
				t.Errorf("task metadata = %q, want %q", got, tt.want)
			}
		})
	}
}
