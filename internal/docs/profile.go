package docs

import (
	"context"
	"slices"
	"strings"
)

// NewProfileGitHub is NewGitHub running gh with env, a gh profile's
// environment (github.ProfileEnv), so it lists repos as that account.
func NewProfileGitHub(env []string) GitHub {
	return NewGatedProfileGitHub(env, nil)
}

// NewGatedProfileGitHub is NewProfileGitHub asking gate before each gh call;
// an error from gate refuses the call (an account's spent hourly budget,
// SP-074). A nil gate admits every call.
func NewGatedProfileGitHub(env []string, gate func() error) GitHub {
	return ghGitHub{run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if gate != nil {
			if err := gate(); err != nil {
				return nil, err
			}
		}
		return execRunEnv(ctx, env, name, args...)
	}}
}

// NewProfileGit is NewGit cloning as the account of a gh profile, whose
// environment is env.
func NewProfileGit(env []string) Git {
	env = slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return name == "GIT_ASKPASS" || name == "SSH_ASKPASS" || strings.HasPrefix(name, "GIT_CONFIG_")
	})
	env = append(env, profileGitEnv...)
	return ghGit{profile: true, run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return runWatchedEnv(ctx, stallTimeout, env, name, args...)
	}}
}

// profileGitEnv keeps the user's git configuration out of a profile's clones,
// with the askpass programs and GIT_CONFIG_* variables NewProfileGit drops.
// Reason: a global insteadOf rule can turn the URL into SSH, and a global or
// system credential helper (the macOS keychain) can answer with another
// account's credentials; either would clone as someone other than the
// profile's account.
var profileGitEnv = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
