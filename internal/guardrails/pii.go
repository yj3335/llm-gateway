// Package guardrails implements the Responsible-AI firewall: deterministic
// PII detection/redaction (regex + checksum validation) and a heuristic
// prompt-injection / jailbreak scoring engine. Every check is pure CPU,
// allocation-light, and designed to run well under 1ms per request.
package guardrails

import (
	"regexp"
	"sort"
	"strings"
)

// Category identifies the kind of sensitive data or attack detected.
type Category string

const (
	CatSSN             Category = "SSN"
	CatCreditCard      Category = "CREDIT_CARD"
	CatEmail           Category = "EMAIL"
	CatPhone           Category = "PHONE"
	CatAWSKey          Category = "AWS_KEY"
	CatAPIKey          Category = "API_KEY"
	CatPromptInjection Category = "PROMPT_INJECTION"
)

// Finding is one detected PII span in a text.
type Finding struct {
	Category Category
	Start    int
	End      int
}

// priority resolves overlapping matches: lower wins.
var priority = map[Category]int{
	CatAWSKey: 0, CatAPIKey: 1, CatCreditCard: 2, CatSSN: 3, CatEmail: 4, CatPhone: 5,
}

// RedactionToken returns the placeholder used in REDACT mode.
func RedactionToken(c Category) string { return "[REDACTED_" + string(c) + "]" }

var (
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,24}`)
	// Formatted SSN (AAA-GG-SSSS / AAA GG SSSS); validated in code because RE2
	// has no lookahead.
	reSSN = regexp.MustCompile(`\b(\d{3})[- ](\d{2})[- ](\d{4})\b`)
	// Unformatted 9-digit SSN only when introduced by an SSN keyword.
	reSSNContext = regexp.MustCompile(`(?i)\b(?:ssn|social security(?: number| no\.?)?)\s*(?:is|:|#|=)?\s*(\d{9})\b`)
	// 13–19 digits with optional single space/dash separators; Luhn-validated.
	reCard  = regexp.MustCompile(`\b\d(?:[ \-]?\d){12,18}\b`)
	rePhone = regexp.MustCompile(`(?:\+?1[\s.\-]?)?(?:\(\d{3}\)\s?|\b\d{3}[\s.\-])\d{3}[\s.\-]\d{4}\b`)
	reAWSID = regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}\b`)
	// 40-char AWS secret access key when labelled as such.
	reAWSSecret = regexp.MustCompile(`(?i)aws[a-z_ \-]{0,20}(?:secret|key)[a-z_ \-]{0,20}["'=:\s]+([A-Za-z0-9/+]{40})\b`)
	reAPIKey    = regexp.MustCompile(`\b(?:sk-(?:proj-|ant-(?:api\d{2}-)?)?[A-Za-z0-9_\-]{20,}|ghp_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{40,}|xox[abprs]-[A-Za-z0-9\-]{10,}|AIza[0-9A-Za-z_\-]{35})`)
)

// ScanPII returns non-overlapping PII findings sorted by position.
//
// Regexes run only over small windows around anchors (digit clusters for
// SSN/card/phone, '@' for email, literal prefixes for keys) instead of the
// whole prompt, which keeps large prompts comfortably inside the latency
// budget. Windows are padded so \b boundaries and keyword context
// ("SSN is ...") behave exactly as on the full text.
func ScanPII(text string) []Finding {
	if len(text) == 0 {
		return nil
	}
	var out []Finding

	for _, w := range emailWindows(text) {
		seg := text[w.a:w.b]
		for _, m := range reEmail.FindAllStringIndex(seg, -1) {
			out = append(out, Finding{CatEmail, w.a + m[0], w.a + m[1]})
		}
	}

	ssnContext := containsFold(text, "ssn") || containsFold(text, "social security")
	for _, w := range digitWindows(text, 9, 40) {
		seg := text[w.a:w.b]
		for _, m := range reSSN.FindAllStringSubmatchIndex(seg, -1) {
			if validSSN(seg[m[2]:m[3]], seg[m[4]:m[5]], seg[m[6]:m[7]]) {
				out = append(out, Finding{CatSSN, w.a + m[0], w.a + m[1]})
			}
		}
		if ssnContext {
			for _, m := range reSSNContext.FindAllStringSubmatchIndex(seg, -1) {
				d := seg[m[2]:m[3]]
				if validSSN(d[0:3], d[3:5], d[5:9]) {
					out = append(out, Finding{CatSSN, w.a + m[2], w.a + m[3]})
				}
			}
		}
		if w.digits >= 13 {
			for _, m := range reCard.FindAllStringIndex(seg, -1) {
				if validCard(seg[m[0]:m[1]]) {
					out = append(out, Finding{CatCreditCard, w.a + m[0], w.a + m[1]})
				}
			}
		}
		if w.digits >= 10 {
			for _, m := range rePhone.FindAllStringIndex(seg, -1) {
				out = append(out, Finding{CatPhone, w.a + m[0], w.a + m[1]})
			}
		}
	}

	if strings.Contains(text, "AKIA") || strings.Contains(text, "ASIA") || strings.Contains(text, "ABIA") || strings.Contains(text, "ACCA") {
		for _, m := range reAWSID.FindAllStringIndex(text, -1) {
			out = append(out, Finding{CatAWSKey, m[0], m[1]})
		}
	}
	if containsFold(text, "aws") {
		for _, m := range reAWSSecret.FindAllStringSubmatchIndex(text, -1) {
			out = append(out, Finding{CatAWSKey, m[2], m[3]})
		}
	}
	if strings.Contains(text, "sk-") || strings.Contains(text, "ghp_") || strings.Contains(text, "github_pat_") || strings.Contains(text, "xox") || strings.Contains(text, "AIza") {
		for _, m := range reAPIKey.FindAllStringIndex(text, -1) {
			out = append(out, Finding{CatAPIKey, m[0], m[1]})
		}
	}
	return resolveOverlaps(out)
}

type window struct {
	a, b   int
	digits int // max digits in any single cluster inside the window
}

func isDigitSep(c byte) bool {
	return c == ' ' || c == '-' || c == '.' || c == '(' || c == ')' || c == '+'
}

// digitWindows finds clusters of digits joined by short runs of formatting
// separators, keeps those with >= minDigits digits, pads them by pad bytes,
// and merges overlapping windows.
func digitWindows(text string, minDigits, pad int) []window {
	var out []window
	n := len(text)
	for i := 0; i < n; {
		if text[i] < '0' || text[i] > '9' {
			i++
			continue
		}
		start, end, digits, gap := i, i, 0, 0
		for j := i; j < n; j++ {
			c := text[j]
			if c >= '0' && c <= '9' {
				digits++
				gap = 0
				end = j + 1
				continue
			}
			if isDigitSep(c) && gap < 3 {
				gap++
				continue
			}
			break
		}
		i = end
		if digits < minDigits {
			continue
		}
		a, b := max(start-pad, 0), min(end+pad, n)
		if l := len(out); l > 0 && a <= out[l-1].b {
			out[l-1].b = b
			out[l-1].digits = max(out[l-1].digits, digits)
		} else {
			out = append(out, window{a, b, digits})
		}
	}
	return out
}

// emailWindows returns merged windows around each '@'. RFC 5321 caps the
// local part at 64 bytes and the domain at 255.
func emailWindows(text string) []window {
	var out []window
	for off := 0; ; {
		k := strings.IndexByte(text[off:], '@')
		if k < 0 {
			return out
		}
		at := off + k
		a, b := max(at-64, 0), min(at+256, len(text))
		if l := len(out); l > 0 && a <= out[l-1].b {
			out[l-1].b = b
		} else {
			out = append(out, window{a: a, b: b})
		}
		off = at + 1
	}
}

func resolveOverlaps(in []Finding) []Finding {
	if len(in) <= 1 {
		return in
	}
	sort.Slice(in, func(i, j int) bool {
		pi, pj := priority[in[i].Category], priority[in[j].Category]
		if pi != pj {
			return pi < pj
		}
		return in[i].Start < in[j].Start
	})
	kept := in[:0:0]
	for _, f := range in {
		overlaps := false
		for _, k := range kept {
			if f.Start < k.End && k.Start < f.End {
				overlaps = true
				break
			}
		}
		if !overlaps {
			kept = append(kept, f)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Start < kept[j].Start })
	return kept
}

// Redact replaces each finding span with its redaction token.
func Redact(text string, findings []Finding) string {
	if len(findings) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	prev := 0
	for _, f := range findings {
		b.WriteString(text[prev:f.Start])
		b.WriteString(RedactionToken(f.Category))
		prev = f.End
	}
	b.WriteString(text[prev:])
	return b.String()
}

func validSSN(area, group, serial string) bool {
	if area == "000" || area == "666" || area[0] == '9' {
		return false
	}
	return group != "00" && serial != "0000"
}

// validCard strips separators and applies length, IIN-prefix and Luhn checks.
func validCard(s string) bool {
	var digits [19]byte
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			if n == 19 {
				return false
			}
			digits[n] = c - '0'
			n++
		}
	}
	if n < 13 || n > 19 {
		return false
	}
	if !knownIIN(digits[:n]) {
		return false
	}
	return Luhn(digits[:n])
}

func knownIIN(d []byte) bool {
	p2 := int(d[0])*10 + int(d[1])
	p4 := p2*100 + int(d[2])*10 + int(d[3])
	switch {
	case d[0] == 4: // Visa
		return true
	case p2 >= 51 && p2 <= 55, p4 >= 2221 && p4 <= 2720: // Mastercard
		return true
	case p2 == 34 || p2 == 37: // Amex
		return true
	case p4 == 6011 || p2 == 65 || (p4 >= 6440 && p4 <= 6499): // Discover
		return true
	case p2 == 35: // JCB
		return true
	case p2 == 36 || p2 == 38 || p2 == 30: // Diners
		return true
	case p2 == 62: // UnionPay
		return true
	}
	return false
}

// Luhn reports whether the digit slice passes the Luhn (mod 10) checksum.
func Luhn(d []byte) bool {
	sum := 0
	double := false
	for i := len(d) - 1; i >= 0; i-- {
		v := int(d[i])
		if double {
			v *= 2
			if v > 9 {
				v -= 9
			}
		}
		sum += v
		double = !double
	}
	return sum%10 == 0
}

func containsFold(s, sub string) bool {
	n := len(sub)
	for i := 0; i+n <= len(s); i++ {
		if strings.EqualFold(s[i:i+n], sub) {
			return true
		}
	}
	return false
}
