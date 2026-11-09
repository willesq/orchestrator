//go:build race

package daemon

// raceEnabled skips timing bounds the race detector's slowdown would break.
const raceEnabled = true
