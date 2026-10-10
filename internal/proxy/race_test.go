//go:build race

package proxy

// raceEnabled: the race detector makes sync.Pool drop items at random.
const raceEnabled = true
