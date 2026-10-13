package router

import (
	"errors"
	"sync"
	"time"
)

// State is a circuit breaker state.
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF-OPEN"
	default:
		return "CLOSED"
	}
}

// ErrCircuitOpen is returned when a breaker rejects a call.
var ErrCircuitOpen = errors.New("circuit breaker open")

// BreakerConfig tunes a Breaker.
type BreakerConfig struct {
	// FailureRate over the last FailureRateWindow completed calls trips the
	// breaker once at least MinRequests calls are in the window. This is the
	// primary rule: it is robust to fast-failing errors clustering ahead of
	// slow in-flight successes under high concurrency.
	FailureRateWindow int
	FailureRate       float64
	MinRequests       int
	// FailureThreshold consecutive failures also trip the breaker
	// (0 disables this rule).
	FailureThreshold int
	// OpenTimeout is how long to stay OPEN before probing (HALF-OPEN).
	OpenTimeout time.Duration
	// HalfOpenMaxRequests concurrent probes are admitted in HALF-OPEN.
	HalfOpenMaxRequests int
	// HalfOpenSuccesses consecutive probe successes close the breaker.
	HalfOpenSuccesses int
}

func (c *BreakerConfig) defaults() {
	if c.FailureRateWindow <= 0 {
		c.FailureRateWindow = 20
	}
	if c.MinRequests <= 0 || c.MinRequests > c.FailureRateWindow {
		c.MinRequests = min(10, c.FailureRateWindow)
	}
	if c.FailureRate <= 0 {
		c.FailureRate = 0.5
	}
	if c.OpenTimeout <= 0 {
		c.OpenTimeout = 5 * time.Second
	}
	if c.HalfOpenMaxRequests <= 0 {
		c.HalfOpenMaxRequests = 1
	}
	if c.HalfOpenSuccesses <= 0 {
		c.HalfOpenSuccesses = 1
	}
}

// Breaker is a CLOSED -> OPEN -> HALF-OPEN -> CLOSED state machine.
// Each admitted call receives a Ticket bound to the breaker's generation so
// late results from a previous state cannot corrupt the current one.
//
// In CLOSED state outcomes are evaluated in *admission order*: a result only
// enters the failure-rate window once every earlier-admitted call has
// resolved. Otherwise, under high concurrency, fast-failing calls (an instant
// 503) report ahead of slow in-flight successes and the window would see a
// burst of "consecutive" failures even when the true error rate is low.
type Breaker struct {
	name string
	cfg  BreakerConfig
	now  func() time.Time

	mu          sync.Mutex
	state       State
	generation  uint64
	consecutive int
	window      []bool // ring of recent outcomes (true = failure)
	wpos, wlen  int
	openedAt    time.Time
	probes      int // in-flight half-open probes
	probeOK     int

	// admission-ordered outcome sequencing (CLOSED state)
	slots   []int8 // 0 = pending, 1 = success, 2 = failure; indexed by seq % len
	seqNext uint64
	seqHead uint64 // oldest unresolved admission

	onChange func(name string, from, to State)
}

// Ticket is returned by Allow and must be passed to Done exactly once.
type Ticket struct {
	gen   uint64
	probe bool
	seq   uint64
}

const breakerSlots = 8192

// NewBreaker creates a breaker. onChange (optional) observes transitions.
func NewBreaker(name string, cfg BreakerConfig, onChange func(name string, from, to State)) *Breaker {
	cfg.defaults()
	return &Breaker{name: name, cfg: cfg, now: time.Now, window: make([]bool, cfg.FailureRateWindow), slots: make([]int8, breakerSlots), onChange: onChange}
}

// Name returns the provider name.
func (b *Breaker) Name() string { return b.name }

// State returns the current state, advancing OPEN -> HALF-OPEN if due.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateOpen && b.now().Sub(b.openedAt) >= b.cfg.OpenTimeout {
		b.transition(StateHalfOpen)
	}
	return b.state
}

// Allow admits a call or returns ErrCircuitOpen.
func (b *Breaker) Allow() (Ticket, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateOpen:
		if b.now().Sub(b.openedAt) < b.cfg.OpenTimeout {
			return Ticket{}, ErrCircuitOpen
		}
		b.transition(StateHalfOpen)
		fallthrough
	case StateHalfOpen:
		if b.probes >= b.cfg.HalfOpenMaxRequests {
			return Ticket{}, ErrCircuitOpen
		}
		b.probes++
		return Ticket{gen: b.generation, probe: true}, nil
	default:
		seq := b.seqNext
		b.seqNext++
		if b.seqNext-b.seqHead > uint64(len(b.slots)) {
			b.seqHead++ // pathological backlog: drop the oldest pending call
			b.drain()
		}
		b.slots[seq%uint64(len(b.slots))] = 0
		return Ticket{gen: b.generation, seq: seq}, nil
	}
}

// Done records the outcome of an admitted call.
func (b *Breaker) Done(t Ticket, success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.gen != b.generation {
		return // stale result from a previous state
	}
	switch b.state {
	case StateHalfOpen:
		if t.probe {
			b.probes--
		}
		if !success {
			b.transition(StateOpen)
			return
		}
		b.probeOK++
		if b.probeOK >= b.cfg.HalfOpenSuccesses {
			b.transition(StateClosed)
		}
	case StateClosed:
		if t.seq < b.seqHead {
			return
		}
		outcome := int8(1)
		if !success {
			outcome = 2
		}
		b.slots[t.seq%uint64(len(b.slots))] = outcome
		b.drain()
	}
}

// drain feeds resolved outcomes into the window in admission order.
func (b *Breaker) drain() {
	for b.state == StateClosed && b.seqHead < b.seqNext {
		i := b.seqHead % uint64(len(b.slots))
		o := b.slots[i]
		if o == 0 {
			return
		}
		b.slots[i] = 0
		b.seqHead++
		b.observe(o == 2)
	}
}

func (b *Breaker) observe(failure bool) {
	b.record(failure)
	if !failure {
		b.consecutive = 0
		return
	}
	b.consecutive++
	if (b.cfg.FailureThreshold > 0 && b.consecutive >= b.cfg.FailureThreshold) || b.rateTripped() {
		b.transition(StateOpen)
	}
}

func (b *Breaker) record(failure bool) {
	b.window[b.wpos] = failure
	b.wpos = (b.wpos + 1) % len(b.window)
	if b.wlen < len(b.window) {
		b.wlen++
	}
}

func (b *Breaker) rateTripped() bool {
	if b.wlen < b.cfg.MinRequests {
		return false
	}
	fails := 0
	for i := 0; i < b.wlen; i++ {
		if b.window[i] {
			fails++
		}
	}
	return float64(fails)/float64(b.wlen) >= b.cfg.FailureRate
}

// transition must be called with mu held.
func (b *Breaker) transition(to State) {
	from := b.state
	if from == to {
		return
	}
	b.state = to
	b.generation++
	b.consecutive, b.probes, b.probeOK = 0, 0, 0
	b.seqHead = b.seqNext // abandon pending admissions from the old generation
	switch to {
	case StateOpen:
		b.openedAt = b.now()
	case StateClosed:
		b.wpos, b.wlen = 0, 0
		for i := range b.window {
			b.window[i] = false
		}
	}
	if b.onChange != nil {
		// Invoke asynchronously-safe: callback must not call back into b.
		b.onChange(b.name, from, to)
	}
}

// ForceState sets the state directly (admin/testing).
func (b *Breaker) ForceState(s State) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transition(s)
}
