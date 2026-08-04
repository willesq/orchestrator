package events

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendAndRead(t *testing.T) {
	tests := []struct {
		name   string
		events []Event
		want   int
	}{
		{name: "no events reads empty", events: nil, want: 0},
		{name: "single event", events: []Event{{Kind: "complete", Source: "dev", Summary: "done"}}, want: 1},
		{
			name: "multiple events preserved",
			events: []Event{
				{Kind: "complete", Source: "dev", Summary: "a"},
				{Kind: "blocked", Source: "ops", Summary: "b"},
			},
			want: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, e := range tt.events {
				if err := Append(dir, "s1", e); err != nil {
					t.Fatalf("Append: %v", err)
				}
			}
			got, err := Read(dir, "s1")
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if len(got) != tt.want {
				t.Errorf("read %d events, want %d", len(got), tt.want)
			}
		})
	}
}

func TestReadSkipsMalformed(t *testing.T) {
	tests := []struct {
		name string
		want int
	}{
		{name: "malformed line skipped, valid kept", want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "s1.jsonl")
			if err := os.WriteFile(path, []byte("{bad json\n{\"kind\":\"info\",\"source\":\"x\",\"summary\":\"ok\"}\n"), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := Read(dir, "s1")
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if len(got) != tt.want {
				t.Errorf("read %d events, want %d", len(got), tt.want)
			}
		})
	}
}
