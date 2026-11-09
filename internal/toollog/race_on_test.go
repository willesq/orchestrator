//go:build race

package toollog

// raceEnabled skips timing bounds the race detector's slowdown would break.
const raceEnabled = true
