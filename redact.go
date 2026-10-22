package main

import (
	"regexp"
	"strings"
	"unicode"
)

// Redaction hides credentials in transcript text before it leaves trail:
// in an export someone may share, and in MCP results, which an agent sends
// to its model provider. It is pattern matching, so it catches the common
// shapes of keys and tokens, not every secret a transcript can hold.

const redacted = "[redacted]"

type redactRule struct {
	re *regexp.Regexp
	// group is the submatch that holds the secret; 0 is the whole match.
	group int
	// keep, when set, vetoes a match that only looks like a secret.
	keep func(secret string) bool
}

var redactRules = []redactRule{
	// A PEM private key, to its END line or, when the text was cut, to the end.
	{re: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----.*?(-----END [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----|$)`)},
	{re: regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|AIPA)[A-Z0-9]{16}\b`)},
	{re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)},
	{re: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{re: regexp.MustCompile(`https://hooks\.slack\.com/services/[A-Za-z0-9/_-]+`)},
	{re: regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{16,}`)},
	{re: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)}, // Anthropic, OpenAI and others
	{re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{re: regexp.MustCompile(`\b(?:npm_[A-Za-z0-9]{36}|hf_[A-Za-z0-9]{30,}|pypi-[A-Za-z0-9_-]{50,})`)},
	{re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)}, // JWT
	// Credentials in a URL: keep the scheme and user, hide the password.
	{re: regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@"']+:([^/\s@"']+)@`), group: 1},
	{re: regexp.MustCompile(`(?i)\b(?:bearer|basic|token)\s+([A-Za-z0-9._~+/=-]{12,})`), group: 1, keep: wordLike},
	// key = value, key: value, KEY=value, "key": "value" for names that say secret.
	{re: regexp.MustCompile(`(?i)[A-Za-z0-9_.-]*(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|auth)[A-Za-z0-9_-]*["']?\s*(?::=|=|:)\s*["']?([^\s"'` + "`" + `,;(){}\[\]<>]{6,})`), group: 1, keep: wordLike},
	// A long run that mixes upper case, lower case and digits is almost
	// always a key. Git hashes are lower case and survive.
	{re: regexp.MustCompile(`[A-Za-z0-9+=-]{32,}`), keep: notRandom},
}

// Redact returns s with anything that looks like a credential replaced.
func Redact(s string) string {
	if s == "" {
		return s
	}
	for _, r := range redactRules {
		s = r.apply(s)
	}
	return s
}

func (r redactRule) apply(s string) string {
	ms := r.re.FindAllStringSubmatchIndex(s, -1)
	if ms == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range ms {
		start, end := m[2*r.group], m[2*r.group+1]
		if start < 0 || start < last || (r.keep != nil && r.keep(s[start:end])) {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(redacted)
		last = end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// wordLike reports whether a value after "token =" or "Bearer" is code
// rather than a secret: a name, a field, or a placeholder.
func wordLike(v string) bool {
	if strings.Contains(v, redacted) || strings.HasPrefix(v, "$") || strings.HasPrefix(v, "%") {
		return true
	}
	digits, upper := 0, 0
	if strings.Trim(v, "0123456789") == "" {
		return true // a count or a port: max_tokens=4096
	}
	for _, c := range v {
		switch {
		case unicode.IsDigit(c):
			digits++
		case unicode.IsUpper(c):
			upper++
		case unicode.IsLower(c), c == '_', c == '.', c == '-':
		default:
			return false // symbols: likely a password
		}
	}
	// os.environ, process.env.API_KEY, my_api_key, getToken, YOUR_KEY_HERE
	if digits == 0 && (upper == 0 || strings.Contains(v, ".") || upper == len(v)-strings.Count(v, "_") || len(v) < 16) {
		return true
	}
	return false
}

func notRandom(v string) bool {
	var lower, upper, digit bool
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= '0' && c <= '9':
			digit = true
		}
	}
	return !(lower && upper && digit)
}

// RedactTranscript redacts a transcript in place: its title and prompt, every
// step's text, tool input and output, and the before and after of each edit.
func RedactTranscript(t *Transcript) {
	s := t.Session
	s.Title, s.Prompt = Redact(s.Title), Redact(s.Prompt)
	for i := range t.Events {
		e := &t.Events[i]
		e.Text, e.Summary, e.Input, e.Output = Redact(e.Text), Redact(e.Summary), Redact(e.Input), Redact(e.Output)
	}
	for i := range t.Changes {
		for j := range t.Changes[i].Hunks {
			h := &t.Changes[i].Hunks[j]
			h.Old, h.New = Redact(h.Old), Redact(h.New)
		}
	}
}
