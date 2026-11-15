package spaces

import (
	"context"
	"errors"
	"os"
	"slices"
)

// SessionReads is what a session's hook may show it from orch's shared
// event streams (SP-026): every event, or only those of the listed spaces.
type SessionReads struct {
	// All is set when no policy is applied, so every event shows (INV-10).
	All bool
	// Spaces are the spaces the session reads under the applied policy;
	// empty when it reads none of them.
	Spaces []string
}

// Shows reports whether an event placed in space shows to the session. An
// event with no space shows only when no policy is applied.
func (r SessionReads) Shows(space string) bool {
	return r.All || space != "" && slices.Contains(r.Spaces, space)
}

// SessionReads decides, from the guard files orchd writes, which spaces
// session id reads: its pin's space's readable set (Resolved.ReadableFrom),
// public spaces only for a session with no readable pin. It never fails:
// a policy that may be applied but cannot be read shows nothing, as the
// guard refuses then (INV-13); with no policy ever applied every event
// shows.
func (f GuardFiles) SessionReads(ctx context.Context, id string) SessionReads {
	snap, err := ReadGuardSnapshot(f.Dir)
	if err != nil {
		applied, herr := HasApplied(ctx, f.DB)
		if herr == nil && !applied && (errors.Is(err, os.ErrNotExist) || errors.Is(err, errSnapshotVersion)) {
			return SessionReads{All: true}
		}
		return SessionReads{}
	}
	if snap.Broken {
		return SessionReads{}
	}
	if snap.Policy == nil {
		return SessionReads{All: true}
	}
	// Reason: as the guard does, the git-tree rule is not re-checked; orchd
	// validated the policy at apply.
	res, err := Validate(*snap.Policy, Env{Home: snap.Home, InGitTree: func(string) bool { return false }})
	switch {
	case err != nil:
		return SessionReads{}
	case !res.Configured():
		return SessionReads{All: true}
	}
	reader := ""
	if p, perr := ReadGuardPin(f.Dir, id); perr == nil {
		reader = p.Space
	}
	return SessionReads{Spaces: res.ReadableFrom(reader)}
}
