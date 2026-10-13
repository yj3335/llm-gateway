//go:build race

package guardrails

// The race detector instruments every memory access (5-20x slowdown), so the
// latency budget test only logs under -race; CI enforces it in a non-race run.
const raceEnabled = true
