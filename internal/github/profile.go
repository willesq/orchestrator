package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// profileOverrides are environment variables gh lets override a profile's
// login or host. Reason: with GH_TOKEN or GITHUB_TOKEN set, gh ignores
// GH_CONFIG_DIR's login, so a space's calls would run as whoever the token
// belongs to.
var profileOverrides = []string{
	"GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN",
	"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST",
}

// ProfileOverrides returns the variables that would override a gh
// profile's login or host, for callers that build a profile's environment
// themselves ('orch run').
func ProfileOverrides() []string { return slices.Clone(profileOverrides) }

// ProfileEnv is the environment for running gh as the account logged in to
// the gh profile directory dir: this process's environment, without anything
// that would override the profile's login, and with GH_CONFIG_DIR set to dir.
// orch never reads inside dir; gh does.
func ProfileEnv(dir string) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(profileOverrides, name)
	})
	return append(env, "GH_CONFIG_DIR="+dir)
}

// ProfileGH runs gh as the account logged in to profile directory dir.
func ProfileGH(dir string) Runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		return GHEnv(ctx, ProfileEnv(dir), args...)
	}
}

// ErrNotLoggedIn is returned by AuthLogin when gh has no working login to
// github.com.
var ErrNotLoggedIn = errors.New("gh is not logged in to github.com")

// AuthLogin returns the github.com account gh is logged in to, as 'gh auth
// status' reports it. It is how orch checks a profile: it never asks gh for
// the token and never reads gh's files.
func AuthLogin(ctx context.Context, gh Runner) (string, error) {
	out, err := gh(ctx, "auth", "status", "--active", "--hostname", "github.com", "--json", "hosts")
	if err != nil {
		return "", err
	}
	var st struct {
		Hosts map[string][]struct {
			State  string `json:"state"`
			Active bool   `json:"active"`
			Login  string `json:"login"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return "", fmt.Errorf("parse gh auth status: %w", err)
	}
	for _, a := range st.Hosts["github.com"] {
		if !a.Active {
			continue
		}
		if a.State != "success" || !ValidLogin(a.Login) {
			return "", fmt.Errorf("%w: its login does not work (%s)", ErrNotLoggedIn, Printable(a.State, 32))
		}
		return a.Login, nil
	}
	return "", ErrNotLoggedIn
}

// Orgs lists the orgs gh's account belongs to.
func Orgs(ctx context.Context, gh Runner) ([]string, error) {
	out, err := gh(ctx, "api", "--paginate", "user/orgs", "--jq", ".[].login")
	if err != nil {
		return nil, err
	}
	var orgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if org := strings.TrimSpace(line); ValidLogin(org) {
			orgs = append(orgs, org)
		}
	}
	return orgs, nil
}
