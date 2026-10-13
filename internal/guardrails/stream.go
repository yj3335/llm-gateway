package guardrails

import "strings"

// streamHoldback is the number of trailing bytes withheld from the client
// so PII that straddles SSE chunk boundaries is still detected. It exceeds
// the longest pattern we redact in practice (emails, API keys).
const streamHoldback = 128

// StreamRedactor applies PII redaction to a token stream incrementally.
// Push returns text that is safe to emit; Flush returns the remainder.
type StreamRedactor struct {
	buf strings.Builder
	PII map[Category]int
}

// Push appends a delta and returns the redacted prefix that can no longer be
// affected by future input.
func (s *StreamRedactor) Push(delta string) string {
	s.buf.WriteString(delta)
	text := s.buf.String()
	if len(text) <= streamHoldback {
		return ""
	}
	cut := len(text) - streamHoldback
	// Never split inside a word: back up to the last whitespace before cut.
	if i := strings.LastIndexAny(text[:cut], " \n\t"); i >= 0 {
		cut = i + 1
	} else {
		return ""
	}
	findings := ScanPII(text)
	var emit []Finding
	for _, f := range findings {
		if f.Start < cut && f.End > cut {
			cut = f.Start // match straddles the boundary: hold it back entirely
		}
	}
	for _, f := range findings {
		if f.End <= cut {
			emit = append(emit, f)
		}
	}
	s.count(emit)
	out := Redact(text[:cut], emit)
	rest := text[cut:]
	s.buf.Reset()
	s.buf.WriteString(rest)
	return out
}

// Flush redacts and returns all buffered text.
func (s *StreamRedactor) Flush() string {
	text := s.buf.String()
	s.buf.Reset()
	f := ScanPII(text)
	s.count(f)
	return Redact(text, f)
}

func (s *StreamRedactor) count(f []Finding) {
	if len(f) == 0 {
		return
	}
	if s.PII == nil {
		s.PII = map[Category]int{}
	}
	for _, x := range f {
		s.PII[x.Category]++
	}
}
