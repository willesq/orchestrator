package status

import (
	"context"
	"fmt"
	"strings"

	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

// loginLines reports, one line per space, which gh login and Claude Code
// configuration the space uses and whether each is logged in (SP-074.3,
// D-59). orchd answers only the operator, so inside an agent session there
// are no lines. Values come from orchd and are printed through printable.
func loginLines(ctx context.Context, fetch func(context.Context) ([]*orchdv1.SpaceLogin, error)) []Line {
	if fetch == nil {
		return nil
	}
	logins, err := fetch(ctx)
	if err != nil {
		return nil
	}
	var out []Line
	for _, l := range logins {
		gh, ghLevel := loginText("gh", "default login", "own profile", l.GetGh())
		cl, clLevel := loginText("claude", "default config", "own config", l.GetClaude())
		line := Line{Name: printable("login " + l.GetSpace()), Value: printable(gh + "; " + cl), Level: worse(ghLevel, clLevel)}
		var details []string
		for _, s := range []*orchdv1.LoginState{l.GetGh(), l.GetClaude()} {
			if s.GetDir() != "" {
				details = append(details, s.GetDir())
			}
			if d := s.GetDetail(); d != "" {
				details = append(details, d)
			}
		}
		line.Detail = printable(strings.Join(details, "; "))
		out = append(out, line)
	}
	return out
}

// loginText describes one login: its source and state.
func loginText(tool, def, own string, s *orchdv1.LoginState) (string, string) {
	src := def
	if s.GetOwn() {
		src = own
	}
	var who []string
	if a := s.GetAccount(); a != "" {
		who = append(who, a)
	}
	if st := s.GetStorage(); st != "" {
		who = append(who, "token in "+st)
	}
	text := fmt.Sprintf("%s: %s, %s", tool, src, s.GetState())
	if len(who) > 0 {
		text += " (" + strings.Join(who, ", ") + ")"
	}
	switch s.GetState() {
	case "logged in":
		return text, LevelOK
	case "unknown":
		return text, LevelInfo
	}
	return text, LevelWarn
}

// worse returns the more serious of two levels.
func worse(a, b string) string {
	rank := map[string]int{LevelInfo: 0, LevelOK: 1, LevelWarn: 2, LevelBad: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
