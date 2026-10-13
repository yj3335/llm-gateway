package guardrails

import (
	"strings"
	"time"
)

// Action is the policy outcome for a request or response.
type Action string

const (
	ActionNone   Action = "NONE"
	ActionRedact Action = "REDACT"
	ActionBlock  Action = "BLOCK"
	ActionFlag   Action = "FLAG" // allowed, but recorded for review
)

// Violation codes returned to clients on BLOCK.
const (
	CodePIIDetected     = "PII_DETECTED"
	CodePromptInjection = "PROMPT_INJECTION"
)

// Mode is the per-tenant PII policy.
type Mode string

const (
	ModeRedact Mode = "REDACT"
	ModeBlock  Mode = "BLOCK"
)

// Message is a role/content pair (decoupled from the wire format).
type Message struct {
	Role    string
	Content string
}

// Engine applies the guardrail policy.
type Engine struct {
	// InjectionThreshold at or above which a prompt is blocked.
	InjectionThreshold float64
	// FlagThreshold at or above which a prompt is flagged (allowed + audited).
	FlagThreshold float64
}

// NewEngine returns an engine with the given block threshold.
func NewEngine(injectionThreshold float64) *Engine {
	if injectionThreshold <= 0 || injectionThreshold > 1 {
		injectionThreshold = 0.7
	}
	return &Engine{InjectionThreshold: injectionThreshold, FlagThreshold: injectionThreshold * 0.6}
}

// InputDecision is the result of inspecting an inbound prompt.
type InputDecision struct {
	Action    Action
	Code      string // set when Action == BLOCK
	Reason    string
	Messages  []Message // possibly redacted copies, same order as input
	PII       map[Category]int
	Injection InjectionResult
	Elapsed   time.Duration
}

// Triggered reports whether any guardrail fired.
func (d InputDecision) Triggered() bool { return d.Action != ActionNone }

// InspectInput scans every message for PII and user/tool-authored messages
// for prompt injection. System messages are operator-authored and trusted for
// injection purposes but are still PII-scanned since they are sent upstream.
func (e *Engine) InspectInput(msgs []Message, mode Mode) InputDecision {
	start := time.Now()
	d := InputDecision{Action: ActionNone, Messages: make([]Message, len(msgs))}
	copy(d.Messages, msgs)

	var worst InjectionResult
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "assistant" {
			continue
		}
		r := ScoreInjection(m.Content)
		if r.Score > worst.Score {
			worst = r
		}
	}
	d.Injection = worst
	if worst.Score >= e.InjectionThreshold {
		d.Action = ActionBlock
		d.Code = CodePromptInjection
		d.Reason = "prompt-injection score " + formatScore(worst.Score) + " >= threshold " + formatScore(e.InjectionThreshold) + " (" + signalNames(worst.Signals) + ")"
		d.Elapsed = time.Since(start)
		return d
	}

	for i, m := range msgs {
		findings := ScanPII(m.Content)
		if len(findings) == 0 {
			continue
		}
		if d.PII == nil {
			d.PII = map[Category]int{}
		}
		for _, f := range findings {
			d.PII[f.Category]++
		}
		d.Messages[i].Content = Redact(m.Content, findings)
	}
	if len(d.PII) > 0 {
		if mode == ModeBlock {
			d.Action = ActionBlock
			d.Code = CodePIIDetected
			d.Reason = "prompt contains PII: " + categoriesString(d.PII)
			d.Messages = msgs
		} else {
			d.Action = ActionRedact
			d.Reason = "redacted PII: " + categoriesString(d.PII)
		}
	} else if worst.Score >= e.FlagThreshold {
		d.Action = ActionFlag
		d.Reason = "suspicious prompt score " + formatScore(worst.Score)
	}
	d.Elapsed = time.Since(start)
	return d
}

// OutputDecision is the result of inspecting a model completion.
type OutputDecision struct {
	Content string
	PII     map[Category]int
	Elapsed time.Duration
}

// InspectOutput redacts PII from a completion (outputs are always redacted,
// never blocked, so clients still receive a usable answer).
func (e *Engine) InspectOutput(text string) OutputDecision {
	start := time.Now()
	findings := ScanPII(text)
	d := OutputDecision{Content: text}
	if len(findings) > 0 {
		d.PII = map[Category]int{}
		for _, f := range findings {
			d.PII[f.Category]++
		}
		d.Content = Redact(text, findings)
	}
	d.Elapsed = time.Since(start)
	return d
}

func categoriesString(m map[Category]int) string {
	order := []Category{CatSSN, CatCreditCard, CatEmail, CatPhone, CatAWSKey, CatAPIKey}
	var parts []string
	for _, c := range order {
		if m[c] > 0 {
			parts = append(parts, string(c))
		}
	}
	return strings.Join(parts, ",")
}

func signalNames(s []Signal) string {
	names := make([]string, len(s))
	for i, x := range s {
		names[i] = x.Name
	}
	return strings.Join(names, ",")
}

func formatScore(f float64) string {
	v := int(f*100 + 0.5)
	return string([]byte{byte('0' + v/100), '.', byte('0' + (v/10)%10), byte('0' + v%10)})
}
