package daemon

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/willesq/orchestrator/internal/settings/spacesettings"
	"github.com/willesq/orchestrator/internal/spaces"
)

// Per-checkout harness settings (spaces plan SP-046). orchd keeps
// .claude/settings.local.json in every checkout under a space's roots; see
// docs/internals/install.md "Per-space harness settings".

// checkoutSettingsEvery is how often orchd syncs every checkout. A new
// clone gets its file at the next sync, so within this plus one sync's time
// (SP-046.1.a4 allows 60 s).
const checkoutSettingsEvery = 30 * time.Second

// checkoutSettings runs the syncs.
type checkoutSettings struct {
	syncer *spacesettings.Syncer
	// kick asks for a sync now: after an apply or a sandbox switch.
	kick chan struct{}
	// problems are the last sync's, logged only when they change.
	problems []string
}

func newCheckoutSettings() *checkoutSettings {
	l := spacesettings.DefaultLayout()
	return &checkoutSettings{
		syncer: &spacesettings.Syncer{
			Layout: l,
			// Reason: in the folder every space's rules deny, since the
			// record names every space's checkouts and the backups hold
			// users' own settings.
			StateFile: filepath.Join(l.OrchDir(), "checkouts.json"),
			BackupDir: filepath.Join(l.OrchDir(), "backups"),
		},
		kick: make(chan struct{}, 1),
	}
}

// nudge asks for a sync soon, without waiting for it.
func (c *checkoutSettings) nudge() {
	if c == nil {
		return
	}
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// runCheckoutSettings syncs at start, every tick and whenever nudged.
func (s *server) runCheckoutSettings(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.syncCheckoutSettings(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.checkouts.kick:
		}
	}
}

// syncCheckoutSettings brings every checkout's file up to date with the
// applied policy, the sandbox switch and the sandbox setting (D-61), and
// logs what changed.
func (s *server) syncCheckoutSettings(ctx context.Context) spacesettings.Report {
	c := s.checkouts
	if c == nil || s.spaces == nil {
		return spacesettings.Report{}
	}
	res, err := s.spaces.Current()
	if err != nil {
		// Reason: a policy that does not verify enforces nothing new, and
		// taking the denies out would weaken every checkout; the files stay
		// as the last good policy wrote them.
		if errors.Is(err, spaces.ErrBroken) {
			c.logProblems([]string{"the applied spaces policy does not verify; checkout settings are left as they are"})
		}
		return spacesettings.Report{}
	}
	if c.syncer.Place == nil {
		c.syncer.Place = s.placeCheckout
	}
	rep := c.syncer.Sync(ctx, res, s.enforcement.choice())
	for _, p := range rep.Written {
		log.Printf("orchd: spaces: wrote harness settings in %s", p)
	}
	for _, p := range rep.Cleared {
		log.Printf("orchd: spaces: took orch's harness settings out of %s", p)
	}
	for _, p := range rep.Ignored {
		log.Printf("orchd: spaces: added %s to %s's git info/exclude", spacesettings.LocalPath, p)
	}
	problems := rep.Problems
	if rep.Truncated {
		problems = append(problems, "too many folders under the space roots; some checkouts were not looked at")
	}
	c.logProblems(problems)
	return rep
}

// logProblems logs problems when they differ from the last sync's.
func (c *checkoutSettings) logProblems(problems []string) {
	if slices.Equal(problems, c.problems) {
		return
	}
	c.problems = problems
	if len(problems) > 0 {
		log.Printf("orchd: spaces: checkout settings: %s", strings.Join(problems, "; "))
	}
}

// placeCheckout is the space whose rules a checkout gets: the space content
// from it is placed in, but only when its folder root decided that (alone,
// with its remote agreeing, or being stricter). Reason: the remote comes
// from the checkout's .git/config, which the session in it can rewrite; for
// placing content a stricter remote is the safe choice, but for deny rules
// a stricter space unlocks that space's folders and gh profile. A checkout
// its remote alone would place gets the rules of a checkout with no space.
func (s *server) placeCheckout(ctx context.Context, checkout string) (string, bool, error) {
	res, err := s.spaces.Current()
	if err != nil {
		return "", false, err
	}
	o, _, err := s.placementOrigin(ctx, checkout)
	if err != nil {
		return "", false, err
	}
	p, err := s.spaces.PlaceUnder(ctx, res, o)
	if err != nil {
		return "", false, err
	}
	// Reason: content from a checkout under a public root that no
	// public_paths pattern lists is quarantined (SP-030, SP-076, D-65), but
	// the folder's rules are still its root's, as before: the root decided.
	// A repo GitHub did not confirm public kept the rules of a checkout with
	// no space before, and still does.
	if p.Rule == spaces.PlaceNoPublicProof && len(p.Roots) > 0 && (p.Remote == nil || p.Remote.Repo.Visibility == spaces.VisibilityPublic) {
		return p.Roots[len(p.Roots)-1].Space, true, nil
	}
	byRoot := p.Rule == spaces.PlaceRoot || p.Rule == spaces.PlaceRootStricter || p.Rule == spaces.PlaceAgree || p.Rule == spaces.PlacePublicPath
	return p.Space, p.Outcome == spaces.Placed && byRoot, nil
}
