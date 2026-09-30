package status

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Render writes the human-readable report. Problem lines are marked with a
// leading "!" so a long report stays greppable.
func (r Report) Render(w io.Writer) {
	fmt.Fprintf(w, "orch %s — %s\n", r.Version, r.At.Format("2006-01-02 15:04:05"))
	for _, sec := range r.Sections {
		fmt.Fprintf(w, "\n%s\n", sec.Title)
		if sec.Note != "" {
			fmt.Fprintf(w, "   (%s)\n", sec.Note)
		}
		width := 0
		for _, l := range sec.Lines {
			if n := len(l.Name); n > width {
				width = n
			}
		}
		for _, l := range sec.Lines {
			fmt.Fprintf(w, "%s %-*s  %s", mark(l.Level), width, l.Name, l.Value)
			if l.Detail != "" {
				fmt.Fprintf(w, "  (%s)", l.Detail)
			}
			fmt.Fprintln(w)
		}
	}
	fmt.Fprintln(w)
	switch r.Problems {
	case 0:
		fmt.Fprintln(w, "all components healthy.")
	case 1:
		fmt.Fprintln(w, "1 problem — see the ! line above.")
	default:
		fmt.Fprintf(w, "%d problems — see the ! lines above.\n", r.Problems)
	}
}

// RenderJSON writes the report as indented JSON for scripts.
func (r Report) RenderJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func mark(level string) string {
	if level == LevelBad {
		return " !"
	}
	return "  "
}

// humanDuration renders a duration compactly: 45s, 12m, 4h12m, 3d4h.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		return strings.TrimSuffix(fmt.Sprintf("%dh%dm", h, int(d.Minutes())-h*60), "0m")
	default:
		days := int(d.Hours()) / 24
		return strings.TrimSuffix(fmt.Sprintf("%dd%dh", days, int(d.Hours())-days*24), "0h")
	}
}

// humanAgo renders how long ago t was relative to now.
func humanAgo(now, t time.Time) string {
	if t.After(now) {
		return "just now"
	}
	return humanDuration(now.Sub(t)) + " ago"
}

// humanBytes renders a byte count in the largest unit that keeps it readable.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, exp := float64(n), -1
	for value >= unit && exp < 3 {
		value /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", value, "KMGT"[exp])
}
