package events

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestStreamNamesCannotLeaveTheDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "events")
	for _, bad := range []string{"../escape", "a/b", "", ".", "..", "x.jsonl/..", strings.Repeat("a", 129), "sid\x00"} {
		if err := Append(dir, bad, Event{Kind: "info"}); !errors.Is(err, ErrInvalidStream) {
			t.Errorf("Append(%q) = %v", bad, err)
		}
		if _, err := Read(dir, bad); !errors.Is(err, ErrInvalidStream) {
			t.Errorf("Read(%q) = %v", bad, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "escape.jsonl")); !os.IsNotExist(err) {
		t.Fatal("an event was written outside the events directory")
	}
	for _, good := range []string{"queue", "pr-babysitter", "1d75205a-bab4-4e6a-bf77-8921b8d70d18", "a_b"} {
		if err := Append(dir, good, Event{Kind: "info"}); err != nil {
			t.Errorf("Append(%q) = %v", good, err)
		}
	}
}
