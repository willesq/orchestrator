package spacesettings

import "github.com/willesq/orchestrator/internal/spaces"

// Choice is the sandbox layer's state for a sync or a launch: its SP-070
// switch, for debugging, and the operator's lasting setting (D-61).
type Choice struct {
	// SwitchedOff is the SP-070 switch set to off: orch writes nothing,
	// not even a space's gh profile (SP-046.3).
	SwitchedOff bool
	// Setting is auto, on or off; "" reads as auto.
	Setting spaces.SandboxMode
}

// ChoiceOn and ChoiceAuto are the switch on with the setting on or auto.
var (
	ChoiceOn   = Choice{Setting: spaces.SandboxOn}
	ChoiceAuto = Choice{Setting: spaces.SandboxAuto}
)

// Sandboxes reports whether a session with rules r gets orch's sandbox
// rules: never while the switch is off or the setting is off, always with
// on, and with auto only when they keep it out of another isolated
// space's root (D-61).
func (c Choice) Sandboxes(r Rules) bool {
	if c.SwitchedOff {
		return false
	}
	switch c.Setting {
	case spaces.SandboxOn:
		return true
	case spaces.SandboxOff:
		return false
	}
	return r.OtherJobs
}

// CheckoutFor is the fragment a checkout with rules r gets: its full rules
// when the sandbox applies to it, else only its gh profile, and nothing
// while the switch is off.
func (c Choice) CheckoutFor(r Rules, checkout string) Fragment {
	switch {
	case c.SwitchedOff:
		return Fragment{}
	case c.Sandboxes(r):
		return CheckoutFragment(r, checkout)
	}
	return LoginFragment(r)
}

// AnySandboxed reports whether some session of res's spaces gets orch's
// sandbox rules under c, for orch status: a checkout of no space is not
// counted.
func (c Choice) AnySandboxed(res *spaces.Resolved, l Layout) bool {
	if !res.Configured() {
		return false
	}
	for _, n := range res.Names() {
		if c.Sandboxes(For(res, n, l)) {
			return true
		}
	}
	return false
}
