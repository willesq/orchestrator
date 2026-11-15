package hook

import (
	"context"
	"fmt"
	"time"

	"github.com/willesq/orchestrator/internal/events"
	"github.com/willesq/orchestrator/internal/paths"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
	"github.com/willesq/orchestrator/internal/spaces"
)

// The sandbox setting in start digests (D-68, RC-216). The setting does not
// mark a digest as enforcement off (D-61), but a session's next start
// digest names it once after each change to it, and after each orchd start
// while it is off. Reason: a process running as the user can write a
// consistent 'off' into orch.db while orchd is stopped, which the next
// start takes; this way no session misses it.

// sandboxView is the sandbox setting as the start hook sees it.
type sandboxView struct {
	Mode   string
	SetAt  int64
	Detail string
	// Started is orchd's start in Unix seconds; 0 when unknown.
	Started int64
	// FromDB marks a setting read from orch.db because orchd did not answer.
	FromDB bool
}

// sandboxSeen is the setting a session's digest last named.
type sandboxSeen struct {
	Mode    string `json:"mode"`
	SetAt   int64  `json:"set_at"`
	Started int64  `json:"started,omitempty"`
}

// startSlack absorbs the rounding of orchd's start, which the hook works out
// from its uptime in whole seconds.
const startSlack = 5

// sandboxFromOrchd is the setting orchd reports, with orchd's start.
func sandboxFromOrchd(client orchdv1.OrchdClient, rep *orchdv1.EnforcementState) *sandboxView {
	sb := rep.GetSandboxSetting()
	if sb == nil {
		return nil
	}
	v := &sandboxView{Mode: sb.GetMode(), SetAt: sb.GetSetAt(), Detail: sb.GetDetail()}
	ctx, cancel := context.WithTimeout(context.Background(), enforcementTimeout)
	defer cancel()
	if p, err := client.Ping(ctx, &orchdv1.PingRequest{}); err == nil && p != nil {
		v.Started = time.Now().Unix() - p.GetUptimeSeconds()
	}
	return v
}

// sandboxFromDB is the setting as orch.db holds it, for when orchd does not
// answer; nil when it cannot be read (the switches' notice already says
// orch cannot tell).
func sandboxFromDB() *sandboxView {
	ctx, cancel := context.WithTimeout(context.Background(), enforcementTimeout)
	defer cancel()
	s, _, err := spaces.ReadSandboxSetting(ctx, paths.DBFile())
	if err != nil {
		return nil
	}
	return &sandboxView{Mode: string(s.Mode), SetAt: unixOrZero(s.SetAt), FromDB: true}
}

// sandboxNotice is the digest line naming the setting when this session
// has not been shown it since its last change, or, while it is off, since
// orchd's last start; it records that the session has now been shown it.
// A setting never changed (the default, auto) is not named.
func sandboxNotice(sessionID string, v *sandboxView) []string {
	if v == nil || (v.SetAt == 0 && v.Mode != string(spaces.SandboxOff)) {
		return nil
	}
	st := loadState(sessionID)
	seen := st.Sandbox
	off := v.Mode == string(spaces.SandboxOff)
	restarted := false
	if seen != nil && off && v.Started > 0 {
		d := v.Started - seen.Started
		restarted = d > startSlack || d < -startSlack
	}
	if seen != nil && seen.Mode == v.Mode && seen.SetAt == v.SetAt && !restarted {
		return nil
	}
	st.Sandbox = &sandboxSeen{Mode: v.Mode, SetAt: v.SetAt, Started: v.Started}
	_ = saveState(sessionID, st)
	return []string{sandboxLine(v, off)}
}

// sandboxLine names the setting, flattened and capped like an event line.
// It is not an enforcement notice: the layers' switches have their own.
func sandboxLine(v *sandboxView, off bool) string {
	s := "the sandbox setting is " + v.Mode
	if v.SetAt > 0 {
		s += " (set " + time.Unix(v.SetAt, 0).UTC().Format("2006-01-02 15:04 UTC") + ")"
	}
	if v.Detail != "" {
		s += ": " + capRunes(v.Detail, 160)
	}
	if off && v.Started > 0 {
		s += fmt.Sprintf("; orchd started %s with it off", time.Unix(v.Started, 0).UTC().Format("2006-01-02 15:04 UTC"))
	}
	if v.FromDB {
		s += " (orchd did not answer; read from orch.db)"
	}
	return eventLine(events.Event{Kind: "info", Source: "orchd", Summary: s + "; see 'orch spaces sandbox'"})
}
