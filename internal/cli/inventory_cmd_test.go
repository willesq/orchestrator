package cli

import (
	"strings"
	"testing"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func TestRAGSourcesCmd(t *testing.T) {
	tests := []struct {
		name         string
		client       *fakeClient
		args         []string
		wantContains []string
		wantAbsent   string
	}{
		{
			name: "renders the inventory",
			client: &fakeClient{listSources: &orchdv1.ListSourcesResponse{
				TotalSources: 2, TotalChunks: 4,
				Sources: []*orchdv1.SourceSummary{
					{SourceUri: "a://1", SourceType: "manual", Chunks: 3, Tags: []string{"x"}},
					{SourceUri: "b://2", SourceType: "web", Chunks: 1, Scope: "work"},
				},
			}},
			args:         []string{"rag", "sources"},
			wantContains: []string{"2 source(s), 4 chunk(s)", "CHUNKS", "a://1", "b://2", "scope=work", "tags=x"},
		},
		{
			name:         "empty inventory",
			client:       &fakeClient{listSources: &orchdv1.ListSourcesResponse{}},
			args:         []string{"rag", "sources"},
			wantContains: []string{"no sources indexed"},
			wantAbsent:   "CHUNKS",
		},
		{
			name: "reports truncation when a limit hides sources",
			client: &fakeClient{listSources: &orchdv1.ListSourcesResponse{
				TotalSources: 9, TotalChunks: 30,
				Sources: []*orchdv1.SourceSummary{{SourceUri: "a://1", Chunks: 3}},
			}},
			args:         []string{"rag", "sources", "--limit", "1"},
			wantContains: []string{"showing 1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := run(factory(tt.client, nil), "", tt.args...)
			if err != nil {
				t.Fatalf("%v: %v", tt.args, err)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("output %q missing %q", out, want)
				}
			}
			if tt.wantAbsent != "" && strings.Contains(out, tt.wantAbsent) {
				t.Errorf("output %q should not contain %q", out, tt.wantAbsent)
			}
		})
	}
}

// Deletion is unrecoverable, so the bare command must refuse — and do so as a
// non-zero error, or a wrapping script cannot tell "refused" from "deleted 0".
func TestRAGForgetRequiresConfirmation(t *testing.T) {
	client := &fakeClient{deleteSource: &orchdv1.DeleteSourceResponse{Deleted: 5}}

	out, err := run(factory(client, nil), "", "rag", "forget", "a://1")
	if err == nil {
		t.Fatal("an unconfirmed forget must return an error, not exit zero")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error %q should tell the user how to confirm", err)
	}
	if strings.Contains(out, "deleted 5") {
		t.Errorf("unconfirmed forget must not delete; got %q", out)
	}
}

func TestRAGForgetWithConfirmation(t *testing.T) {
	tests := []struct {
		name         string
		client       *fakeClient
		wantContains string
	}{
		{
			name:         "reports the chunk count",
			client:       &fakeClient{deleteSource: &orchdv1.DeleteSourceResponse{Deleted: 5}},
			wantContains: "deleted 5 chunk(s) from a://1",
		},
		{
			name:         "absent source is reported plainly",
			client:       &fakeClient{deleteSource: &orchdv1.DeleteSourceResponse{Deleted: 0}},
			wantContains: "nothing deleted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := run(factory(tt.client, nil), "", "rag", "forget", "a://1", "--yes")
			if err != nil {
				t.Fatalf("forget --yes: %v", err)
			}
			if !strings.Contains(out, tt.wantContains) {
				t.Errorf("output %q missing %q", out, tt.wantContains)
			}
		})
	}
}

func TestTasksCmd(t *testing.T) {
	tests := []struct {
		name         string
		client       *fakeClient
		wantContains []string
	}{
		{
			name: "lists tasks and warns the worker is a stub",
			client: &fakeClient{listTasks: &orchdv1.ListTasksResponse{Tasks: []*orchdv1.Task{
				{Id: "t1", Status: "pending", Agent: "dev", Body: "fix it", UpdatedAt: "2026-09-30"},
			}}},
			wantContains: []string{"ID", "t1", "pending", "dev", "fix it", "stub"},
		},
		{
			name:         "empty ledger",
			client:       &fakeClient{listTasks: &orchdv1.ListTasksResponse{}},
			wantContains: []string{"no tasks recorded"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := run(factory(tt.client, nil), "", "tasks")
			if err != nil {
				t.Fatalf("tasks: %v", err)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("output %q missing %q", out, want)
				}
			}
		})
	}
}

func TestTaskCmd(t *testing.T) {
	tests := []struct {
		name         string
		client       *fakeClient
		wantContains []string
	}{
		{
			name: "renders one task with the stub warning",
			client: &fakeClient{getTask: &orchdv1.GetTaskResponse{Found: true, Task: &orchdv1.Task{
				Id: "t1", Status: "complete", Agent: "dev", Body: "fix it", Result: "processed: fix it",
			}}},
			wantContains: []string{"id:", "t1", "complete", "fix it", "processed: fix it", "stub"},
		},
		{
			name:         "no result yet",
			client:       &fakeClient{getTask: &orchdv1.GetTaskResponse{Found: true, Task: &orchdv1.Task{Id: "t1", Status: "pending"}}},
			wantContains: []string{"(none yet)"},
		},
		{
			name:         "unknown id",
			client:       &fakeClient{getTask: &orchdv1.GetTaskResponse{Found: false}},
			wantContains: []string{"no task with id t1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := run(factory(tt.client, nil), "", "task", "t1")
			if err != nil {
				t.Fatalf("task: %v", err)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("output %q missing %q", out, want)
				}
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{name: "single line passes through", in: "hello", want: "hello"},
		{name: "only the first line", in: "one\ntwo", want: "one"},
		{name: "trims surrounding space", in: "  padded  \nrest", want: "padded"},
		{name: "empty stays empty", in: "", want: ""},
		{name: "long lines are elided", in: strings.Repeat("x", 120), want: strings.Repeat("x", 90) + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstLine(tt.in); got != tt.want {
				t.Errorf("firstLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
