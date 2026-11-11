package spaces

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// The enforcement switches (spaces plan SP-070, D-11, D-12, D-40). Each
// layer of spaces enforcement has an operator-only switch so behavior can be
// isolated when debugging. Logging never stops: a switch changes what is
// enforced, never what is recorded.

// Layer names one enforcement layer.
type Layer string

// The layers, in the order they are listed.
const (
	// LayerMemory filters memory reads to the caller's readable spaces.
	LayerMemory Layer = "memory"
	// LayerGuard is the pre-tool-use guard's mode.
	LayerGuard Layer = "guard"
	// LayerTerms refuses outbound text holding another space's terms.
	LayerTerms Layer = "terms"
	// LayerSandbox writes generated sandbox deny rules into checkouts.
	LayerSandbox Layer = "sandbox"
	// LayerHooks is the tool hooks (pre-tool-use, cwd-changed) themselves:
	// orch settings and install cannot turn them off; only this switch can
	// (D-40).
	LayerHooks Layer = "hooks"
)

// Switch states. On/off layers use SwitchOn and SwitchOff; the guard uses
// SwitchOff, SwitchLog, SwitchBlockOtherJobs and SwitchStrict.
const (
	SwitchOn  = "on"
	SwitchOff = "off"
	// SwitchLog records the guard's verdict without enforcing it.
	SwitchLog = "log"
	// SwitchBlockOtherJobs refuses calls reaching another isolated space.
	SwitchBlockOtherJobs = "block-other-jobs"
	// SwitchStrict is opt-in and stricter than block-other-jobs (SP-040).
	SwitchStrict = "strict"
)

// Layers lists every layer in display order.
var Layers = []Layer{LayerMemory, LayerGuard, LayerTerms, LayerSandbox, LayerHooks}

// layerStates lists each layer's states, the default first.
var layerStates = map[Layer][]string{
	LayerMemory:  {SwitchOn, SwitchOff},
	LayerGuard:   {SwitchBlockOtherJobs, SwitchOff, SwitchLog, SwitchStrict},
	LayerTerms:   {SwitchOn, SwitchOff},
	LayerSandbox: {SwitchOn, SwitchOff},
	LayerHooks:   {SwitchOn, SwitchOff},
}

// layerTitles name each layer for people.
var layerTitles = map[Layer]string{
	LayerMemory:  "memory filtering",
	LayerGuard:   "tool guard",
	LayerTerms:   "outbound terms",
	LayerSandbox: "generated sandbox rules",
	LayerHooks:   "tool hooks (pre-tool-use, cwd-changed)",
}

// MaxSwitchFor is the longest a timed switch may last. Reason: a timed
// switch is for a debugging session; anything longer is set untimed, so it
// is a deliberate choice the operator sees in every digest.
const MaxSwitchFor = 24 * time.Hour

var (
	// ErrUnknownLayer is returned for a layer name that is not one of Layers.
	ErrUnknownLayer = errors.New("unknown enforcement layer")
	// ErrBadState is returned for a state the layer does not have.
	ErrBadState = errors.New("not a state of this enforcement layer")
	// ErrBadDuration is returned for a timed switch outside (0, MaxSwitchFor].
	ErrBadDuration = errors.New("a timed switch lasts more than 0 and at most 24h")
)

// ParseLayer returns the layer named s.
func ParseLayer(s string) (Layer, error) {
	l := Layer(strings.ToLower(strings.TrimSpace(s)))
	if _, ok := layerStates[l]; !ok {
		return "", fmt.Errorf("%w %q (one of %s)", ErrUnknownLayer, s, layerList())
	}
	return l, nil
}

func layerList() string {
	names := make([]string, len(Layers))
	for i, l := range Layers {
		names[i] = string(l)
	}
	return strings.Join(names, ", ")
}

// States returns the layer's states, the default first.
func (l Layer) States() []string { return append([]string(nil), layerStates[l]...) }

// Default is the layer's state when the operator never switched it: memory
// on, guard block-other-jobs, terms on, sandbox on, hooks on.
func (l Layer) Default() string {
	if s := layerStates[l]; len(s) > 0 {
		return s[0]
	}
	return ""
}

// Title names the layer for people.
func (l Layer) Title() string { return layerTitles[l] }

// CheckState returns an error unless state is one of the layer's states.
func (l Layer) CheckState(state string) error {
	for _, s := range layerStates[l] {
		if s == state {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is one of %s, not %q", ErrBadState, l, strings.Join(layerStates[l], ", "), state)
}

// Relaxed reports whether state stops the layer enforcing: off for every
// layer, and log for the guard, which records its verdict without acting.
// Every session's start digest names a relaxed layer.
func (l Layer) Relaxed(state string) bool {
	return state == SwitchOff || (l == LayerGuard && state == SwitchLog)
}

// Switch is one layer's current state.
type Switch struct {
	Layer Layer
	State string
	// SetAt and SetBy say when and by whom the state was set; SetAt is zero
	// for a layer never switched.
	SetAt time.Time
	SetBy string
	// Until is when a timed switch reverts to Previous; zero when untimed.
	Until    time.Time
	Previous string
}

// Relaxed reports whether the switch stops its layer enforcing.
func (s Switch) Relaxed() bool { return s.Layer.Relaxed(s.State) }

// Enforcement is every layer's switch.
type Enforcement map[Layer]Switch

// DefaultEnforcement is every layer at its default, never switched.
func DefaultEnforcement() Enforcement {
	e := Enforcement{}
	for _, l := range Layers {
		e[l] = Switch{Layer: l, State: l.Default()}
	}
	return e
}

// State returns layer l's state, its default when unknown.
func (e Enforcement) State(l Layer) string {
	if s, ok := e[l]; ok && s.State != "" {
		return s.State
	}
	return l.Default()
}

// At is e as it stands at now: a timed switch whose time is up reads as the
// state it reverts to. It changes nothing stored.
func (e Enforcement) At(now time.Time) Enforcement {
	out := make(Enforcement, len(e))
	for l, s := range e {
		if !s.Until.IsZero() && !now.Before(s.Until) {
			s = Switch{Layer: l, State: s.Previous, SetAt: s.Until, SetBy: "timer"}
		}
		out[l] = s
	}
	return out
}

// Relaxed lists the switches that stop their layer enforcing, in Layers order.
func (e Enforcement) Relaxed() []Switch {
	var out []Switch
	for _, l := range Layers {
		if s, ok := e[l]; ok && s.Relaxed() {
			out = append(out, s)
		}
	}
	return out
}

// SwitchChange is one recorded switch change.
type SwitchChange struct {
	Layer    Layer
	State    string
	Previous string
	At       time.Time
	By       string
	Until    time.Time
	// RevertsTo is the state a timed switch reverts to at Until.
	RevertsTo string
	// Expired reports a timed switch reverting by itself.
	Expired bool
}

// Describe is the change in one line, for orchd's log and event stream.
func (c SwitchChange) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s) switched from %s to %s", c.Layer, c.Layer.Title(), c.Previous, c.State)
	if c.Expired {
		b.WriteString(": its timed switch expired")
	} else {
		fmt.Fprintf(&b, " by %s", c.By)
	}
	if !c.Until.IsZero() {
		fmt.Fprintf(&b, " until %s", c.Until.UTC().Format(time.RFC3339))
	}
	return b.String()
}
