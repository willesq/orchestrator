package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/willesq/orchestrator/internal/agentrun"
)

func TestRAGWritesRefusedInQueuedTask(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		args  []string
		want  string
	}{
		{name: "ingest text", stdin: "trust me", args: []string{"ingest", "text", "--source-uri", "manual://x"},
			want: "queue tasks cannot write to the RAG store (running as task-1)"},
		{name: "rag forget", args: []string{"rag", "forget", "manual://x", "--yes"},
			want: "queue tasks cannot delete from the RAG store (running as task-1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(agentrun.TaskIDEnv, "task-1")
			client := &fakeClient{err: errors.New("client must not be reached")}
			_, err := run(factory(client, nil), tt.stdin, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
