package status

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	tests := []struct {
		name      string
		report    Report
		wantHas   []string
		wantNotIn []string
	}{
		{
			name: "problem lines are marked and tallied",
			report: Report{
				Version: "1.2.3", At: testNow, Problems: 2,
				Sections: []Section{{Title: "service", Lines: []Line{
					{Name: "orchd", Value: "not running", Detail: "start it", Level: LevelBad},
					{Name: "socket", Value: "/tmp/s.sock"},
				}}},
			},
			wantHas: []string{
				"orch 1.2.3 — 2026-09-28 12:00:00",
				"service",
				" ! orchd   not running  (start it)",
				"   socket  /tmp/s.sock",
				"2 problems — see the ! lines above.",
			},
		},
		{
			name: "a single problem reads in the singular",
			report: Report{Problems: 1, Sections: []Section{{Title: "x", Lines: []Line{
				{Name: "a", Value: "bad", Level: LevelBad},
			}}}},
			wantHas: []string{"1 problem — see the ! line above."},
		},
		{
			name:    "a clean report says so",
			report:  Report{Sections: []Section{{Title: "x", Lines: []Line{{Name: "a", Value: "ok", Level: LevelOK}}}}},
			wantHas: []string{"all components healthy."},
		},
		{
			name:      "an empty section prints its note",
			report:    Report{Sections: []Section{{Title: "subsystems", Note: "orchd is not running"}}},
			wantHas:   []string{"subsystems", "   (orchd is not running)"},
			wantNotIn: []string{" ! "},
		},
		{
			name: "names are padded to the widest in the section",
			report: Report{Sections: []Section{{Title: "x", Lines: []Line{
				{Name: "a", Value: "1"},
				{Name: "long-name", Value: "2"},
			}}}},
			wantHas: []string{"   a          1\n", "   long-name  2\n"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			tt.report.Render(&buf)
			out := buf.String()
			for _, want := range tt.wantHas {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			for _, unwanted := range tt.wantNotIn {
				if strings.Contains(out, unwanted) {
					t.Errorf("output should not contain %q:\n%s", unwanted, out)
				}
			}
		})
	}
}

func TestRenderJSONShape(t *testing.T) {
	tests := []struct {
		name    string
		wantHas []string
	}{
		{name: "indented JSON with the expected keys", wantHas: []string{
			`"version": "1.2.3"`, `"problems": 1`, `"title": "service"`, `"level": "bad"`,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Report{Version: "1.2.3", At: testNow, Problems: 1, Sections: []Section{{
				Title: "service",
				Lines: []Line{{Name: "orchd", Value: "not running", Level: LevelBad}},
			}}}
			var buf strings.Builder
			if err := r.RenderJSON(&buf); err != nil {
				t.Fatalf("RenderJSON: %v", err)
			}
			for _, want := range tt.wantHas {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("json missing %q:\n%s", want, buf.String())
				}
			}
		})
	}
}

func TestMark(t *testing.T) {
	tests := []struct {
		name  string
		level string
		want  string
	}{
		{name: "bad is flagged", level: LevelBad, want: " !"},
		{name: "warn is not flagged", level: LevelWarn, want: "  "},
		{name: "ok is not flagged", level: LevelOK, want: "  "},
		{name: "info is not flagged", level: LevelInfo, want: "  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mark(tt.level); got != tt.want {
				t.Errorf("mark(%q) = %q, want %q", tt.level, got, tt.want)
			}
		})
	}
}

func TestHumanDuration(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want string
	}{
		{name: "negative clamps to zero", in: -time.Second, want: "0s"},
		{name: "seconds", in: 45 * time.Second, want: "45s"},
		{name: "minutes", in: 12 * time.Minute, want: "12m"},
		{name: "hours and minutes", in: 4*time.Hour + 12*time.Minute, want: "4h12m"},
		{name: "whole hours drop the minutes", in: 3 * time.Hour, want: "3h"},
		{name: "days and hours", in: 3*24*time.Hour + 4*time.Hour, want: "3d4h"},
		{name: "whole days drop the hours", in: 2 * 24 * time.Hour, want: "2d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := humanDuration(tt.in); got != tt.want {
				t.Errorf("humanDuration(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestHumanAgo(t *testing.T) {
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{name: "past", t: testNow.Add(-90 * time.Second), want: "1m ago"},
		{name: "future clamps to just now", t: testNow.Add(time.Minute), want: "just now"},
		{name: "same instant", t: testNow, want: "0s ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := humanAgo(testNow, tt.t); got != tt.want {
				t.Errorf("humanAgo = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		name string
		in   int64
		want string
	}{
		{name: "bytes", in: 512, want: "512 B"},
		{name: "kilobytes", in: 2048, want: "2.0 KB"},
		{name: "megabytes", in: 5 * 1024 * 1024, want: "5.0 MB"},
		{name: "gigabytes", in: 3 * 1024 * 1024 * 1024, want: "3.0 GB"},
		{name: "terabytes cap the unit", in: 2 * 1024 * 1024 * 1024 * 1024, want: "2.0 TB"},
		{name: "zero", in: 0, want: "0 B"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := humanBytes(tt.in); got != tt.want {
				t.Errorf("humanBytes(%d) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "report marshals and unmarshals"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			original := Collect(context.Background(), healthyDeps(t))
			if err := original.RenderJSON(&buf); err != nil {
				t.Fatalf("RenderJSON: %v", err)
			}
			var back Report
			if err := json.Unmarshal([]byte(buf.String()), &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(back.Sections) != len(original.Sections) || back.Version != original.Version {
				t.Errorf("round trip lost data: %+v", back)
			}
		})
	}
}
