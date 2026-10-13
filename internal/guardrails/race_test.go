//go:build race

package guardrails

// The race detector instruments every memory access (5-20x slowdown), so the
// latency budget is scaled when running under -race.
const raceEnabled = true
