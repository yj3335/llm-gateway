package guardrails

import (
	"encoding/base64"
	"regexp"
	"strings"
)

// Signal is one matched injection heuristic.
type Signal struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
}

// InjectionResult is the outcome of prompt-injection scoring.
type InjectionResult struct {
	// Score in [0,1], combined from signals via noisy-OR: 1 - Π(1 - w_i).
	Score   float64  `json:"score"`
	Signals []Signal `json:"signals,omitempty"`
}

type rule struct {
	name   string
	weight float64
	// any: the regex only runs when the normalized text contains at least
	// one of these literals. This keeps benign prompts on a pure
	// strings.Contains fast path well under the 1ms budget.
	any []string
	re  *regexp.Regexp
}

// Rules run against normalized text (lower-cased, leetspeak-folded,
// punctuation collapsed to single spaces).
var semanticRules = []rule{
	{"instruction_override", 0.92, []string{"ignore", "disregard", "forget", "override", "bypass", "skip", "drop"}, regexp.MustCompile(`\b(ignore|disregard|forget|override|bypass|skip|drop)( (all|any|every|the|your|my|of|these|those|previous|prior))* (previous|prior|above|earlier|preceding|initial|original|system|developer|safety)( \w+)? (instructions?|prompts?|rules|directions|guidelines|context|messages|constraints|guardrails)\b`)},
	{"instruction_reset", 0.8, []string{"ignore", "disregard", "forget"}, regexp.MustCompile(`\b(ignore|disregard|forget) (everything|all)( (that|you))?( (were|have been|was|ve been))? (told|said|instructed|given)\b`)},
	{"system_prompt_exfiltration", 0.85, []string{" prompt", "instructions", " message", " rules"}, regexp.MustCompile(`\b(reveal|show|print|repeat|output|display|leak|dump|tell me|give me|what (is|are)) (me )?(your|the)( (full|entire|exact|hidden|secret|initial|original|real))* (system|developer|hidden|initial|original) (prompt|instructions|message|rules)\b`)},
	{"roleplay_jailbreak", 0.85, []string{"unrestricted", "unfiltered", "uncensored", "jailbroken", "evil", "amoral", "without", "no ethical", "no moral", "no content", "not bound"}, regexp.MustCompile(`\b(you are|you re|youre|act as|acting as|pretend (to be|you are|that you are)|roleplay as|role play as|from now on you( are| will be)?|simulate)\b.{0,60}\b(unrestricted|unfiltered|uncensored|jailbroken|evil|amoral|without (any )?(restrictions|limits|rules|filters|guidelines|censorship)|no (ethical|moral|content) (guidelines|restrictions|limits|rules)|not bound by)`)},
	{"dan_persona", 0.75, []string{"dan", "do anything", "stan", "dude mode", "aim mode"}, regexp.MustCompile(`\b(dan|do anything now|stan|dude mode|aim mode)\b.{0,40}\b(mode|prompt|persona|jailbreak|anything)\b|\bdo anything now\b`)},
	{"developer_mode", 0.6, []string{" mode"}, regexp.MustCompile(`\b(developer|debug|god|admin|sudo|maintenance) mode( (enabled|on|activated))?\b`)},
	{"jailbreak_keyword", 0.45, []string{"jailbreak"}, regexp.MustCompile(`\bjailbreak(ed|ing)?\b`)},
	{"new_instructions", 0.55, []string{"instructions", "directives", "rules"}, regexp.MustCompile(`\b(new|updated|real|actual|true|revised) (system )?(instructions|directives|rules) (are|follow|below|is)\b`)},
	{"safety_disable", 0.6, []string{"safety", "content", "ethical", "moderation"}, regexp.MustCompile(`\b(disable|turn off|remove|deactivate|ignore|bypass) (your |all |the )?(safety|content|ethical|moderation) (filters?|guidelines|policies|policy|restrictions|protocols|checks)\b`)},
	{"forced_compliance", 0.35, []string{"obey", "comply", "answer everything", "follow my"}, regexp.MustCompile(`\byou (must|will|have to|shall) (now )?(obey|comply|answer everything|follow my (every )?(command|instruction)s?)\b`)},
	{"priv_escalation", 0.5, []string{"developer", "creator", "admin", "owner", "openai", "anthropic"}, regexp.MustCompile(`\b(i am|im) (your|the) (developer|creator|administrator|admin|owner|openai|anthropic)\b`)},
}

// Delimiter / chat-template injection tokens, matched case-insensitively
// against the raw text.
var delimiterTokens = []string{
	"<|im_start|>", "<|im_end|>", "<|system|>", "<|endoftext|>", "<|assistant|>",
	"[inst]", "[/inst]", "<<sys>>", "<</sys>>", "</s>", "<|begin_of_text|>",
	"<|start_header_id|>", "### system", "###system", "### instruction", "```system",
}

var (
	reRoleLine   = regexp.MustCompile(`(?im)^\s*(system|assistant|developer)\s*:\s*\S`)
	reJSONRole   = regexp.MustCompile(`(?i)"role"\s*:\s*"(system|developer)"`)
	reDecodeVerb = regexp.MustCompile(`(?i)\b(decode|base64|b64|rot13|hex[- ]?decode)\b`)
	leetReplacer = strings.NewReplacer("0", "o", "1", "i", "3", "e", "4", "a", "5", "s", "7", "t", "@", "a", "$", "s")
)

// normalize lower-cases, folds leetspeak, and collapses every run of
// non-alphanumerics into a single space so obfuscations like
// "I.g.n.o.r.e" or "ign0re   prev10us" still match.
func normalize(s string) string {
	s = leetReplacer.Replace(strings.ToLower(s))
	var b strings.Builder
	b.Grow(len(s))
	space := true
	for _, r := range s {
		isWord := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '\''
		if r == '\'' {
			continue // "you're" -> "youre"
		}
		if isWord || r > 127 {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	out := strings.TrimSpace(b.String())
	return collapseSpacedLetters(out)
}

// collapseSpacedLetters joins runs of single letters ("i g n o r e") that
// attackers use to dodge keyword filters.
func collapseSpacedLetters(s string) string {
	if !strings.Contains(s, " ") {
		return s
	}
	words := strings.Split(s, " ")
	var b strings.Builder
	b.Grow(len(s))
	run := 0
	for i, w := range words {
		single := len(w) == 1
		if single {
			// look ahead: only collapse runs of >= 4 single letters
			if run == 0 {
				j := i
				for j < len(words) && len(words[j]) == 1 {
					j++
				}
				if j-i < 4 {
					single = false
				}
			}
		}
		if single {
			if run == 0 && b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(w)
			run++
			continue
		}
		run = 0
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(w)
	}
	return b.String()
}

// ScoreInjection scores text for prompt-injection / jailbreak intent.
func ScoreInjection(text string) InjectionResult {
	return scoreInjection(text, 0)
}

func scoreInjection(text string, depth int) InjectionResult {
	var res InjectionResult
	if strings.TrimSpace(text) == "" {
		return res
	}
	norm := normalize(text)
	add := func(name string, w float64) {
		res.Signals = append(res.Signals, Signal{Name: name, Weight: w})
	}
	for _, r := range semanticRules {
		if containsAny(norm, r.any) && r.re.MatchString(norm) {
			add(r.name, r.weight)
		}
	}

	lower := strings.ToLower(text)
	delims := 0
	for _, tok := range delimiterTokens {
		if strings.Contains(lower, tok) {
			delims++
		}
	}
	if delims > 0 {
		add("delimiter_injection", min(0.55+0.15*float64(delims-1), 0.9))
	}
	if containsAny(lower, []string{"system", "assistant", "developer"}) {
		if reRoleLine.MatchString(text) {
			add("role_spoofing", 0.45)
		}
		if strings.Contains(lower, `"role"`) && reJSONRole.MatchString(text) {
			add("json_role_injection", 0.5)
		}
	}

	if depth == 0 {
		runs := base64Runs(text, 24, 8)
		hasVerb := len(runs) > 0 && reDecodeVerb.MatchString(text)
		for _, m := range runs {
			decoded, ok := decodeBase64Text(m)
			if !ok {
				continue
			}
			inner := scoreInjection(decoded, depth+1)
			switch {
			case inner.Score >= 0.5:
				add("base64_encoded_injection", min(inner.Score+0.1, 0.97))
			case hasVerb:
				add("base64_decode_request", 0.3)
			}
		}
	}

	res.Score = combine(res.Signals)
	return res
}

func combine(signals []Signal) float64 {
	keep := 1.0
	for _, s := range signals {
		keep *= 1 - s.Weight
	}
	score := 1 - keep
	if score > 1 {
		return 1
	}
	return score
}

func decodeBase64Text(s string) (string, bool) {
	var raw []byte
	var err error
	if raw, err = base64.StdEncoding.DecodeString(s); err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "=")); err != nil {
			return "", false
		}
	}
	if len(raw) < 12 {
		return "", false
	}
	printable := 0
	for _, c := range raw {
		if (c >= 32 && c < 127) || c == '\n' || c == '\t' || c == '\r' {
			printable++
		}
	}
	if float64(printable)/float64(len(raw)) < 0.9 {
		return "", false
	}
	return string(raw), true
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func isB64(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/'
}

// base64Runs returns up to limit maximal runs of base64-alphabet characters
// of at least minLen (plus trailing '=' padding). Runs made only of letters
// are skipped: encoded payloads virtually always contain digits, '+' or '/'
// or mixed case beyond what natural-language words show.
func base64Runs(s string, minLen, limit int) []string {
	var out []string
	for i := 0; i < len(s) && len(out) < limit; {
		if !isB64(s[i]) {
			i++
			continue
		}
		j := i
		upper, lower, other := 0, 0, 0
		for j < len(s) && isB64(s[j]) {
			c := s[j]
			switch {
			case c >= 'A' && c <= 'Z':
				upper++
			case c >= 'a' && c <= 'z':
				lower++
			default:
				other++
			}
			j++
		}
		k := j
		for k < len(s) && k-j < 2 && s[k] == '=' {
			k++
		}
		if j-i >= minLen && (other > 0 || (upper > 1 && lower > 1)) {
			out = append(out, s[i:k])
		}
		i = k
		if k == j {
			i = j
		}
	}
	return out
}
