package main

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	gone := map[string]string{
		"aws":       "aws_access_key_id = AKIA" + "IOSFODNN7EXAMPLE",
		"github":    "git push https://ghp_" + "0123456789abcdefghijABCDEFGHIJ0123@github.com/x/y",
		"pat":       "GH_TOKEN=github_pat_" + "11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz",
		"anthropic": "export ANTHROPIC_API_KEY=sk-" + "ant-api03-abcdefghijklmnopqrstuvwxyz0123456789",
		"openai":    `{"key": "sk-` + `proj-abcdefghijklmnopqrstuvwxyz012345"}`,
		"slack":     "xox" + "b-1234567890-abcdefghijklmn",
		"stripe":    "sk_" + "live_abcdefghijklmnop1234",
		"google":    "AIza" + "SyA1234567890abcdefghijklmnopqrstuv",
		"jwt":       "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9." + "eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"bearer":    "curl -H 'Authorization: Bearer 3f9a8b7c6d5e4f3a2b1c'",
		"url":       "DATABASE_URL=postgres://app:hunter2secret@db.internal:5432/app",
		"password":  `password: "Tr0ub4dor&3"`,
		"secret":    "CLIENT_SECRET=a8f5f167f44f4964e6c998dee827110c",
		"entropy":   "value Zk9xR2pLbW5PcFFyU3R1VndYeVo0NTY3ODkwMTIz end",
		"pem":       "-----BEGIN RSA PRIVATE " + "KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----",
		"pem cut":   "-----BEGIN OPENSSH PRIVATE " + "KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA",
	}
	secrets := map[string]string{
		"aws": "AKIAIOSFODNN7EXAMPLE", "github": "ghp_0123", "pat": "github_pat_11", "anthropic": "sk-ant-api03",
		"openai": "sk-proj", "slack": "xoxb-", "stripe": "sk_live_", "google": "AIzaSy", "jwt": "eyJhbGci",
		"bearer": "3f9a8b7c", "url": "hunter2", "password": "Tr0ub4dor", "secret": "a8f5f167",
		"entropy": "Zk9xR2pL", "pem": "MIIEow", "pem cut": "b3BlbnNz",
	}
	for name, in := range gone {
		out := Redact(in)
		if strings.Contains(out, secrets[name]) || !strings.Contains(out, redacted) {
			t.Errorf("%s: %q -> %q", name, in, out)
		}
	}
	if got := Redact("postgres://app:hunter2secret@db"); got != "postgres://app:"+redacted+"@db" {
		t.Errorf("url keeps the user and host: %q", got)
	}

	kept := []string{
		"token := os.Getenv(\"GITHUB_TOKEN\")",
		"password = request.form.password",
		"apiKey: process.env.API_KEY",
		"max_tokens=4096, input_tokens: 12345",
		"commit 9fceb02d0ae598e95dc970b74767f19372d61af8",
		"session 0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000",
		"author: veluribharath",
		"Bearer token missing",
		"func TestRedactKeepsOrdinaryCode(t *testing.T)",
		"/home/me/projects/MyApp2/src/components/Header.tsx",
		"PWD=/home/me/app2",
		"token_file=/run/secrets/api2",
		"AuthorDate: 2026-10-01T10:00:00",
	}
	for _, in := range kept {
		if out := Redact(in); out != in {
			t.Errorf("over-redacted %q -> %q", in, out)
		}
	}
}

func TestRedactTranscript(t *testing.T) {
	key := "sk-" + "ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"
	tr := &Transcript{
		Session: &Session{Title: "use " + key, Prompt: key},
		Events:  []Event{{Text: key, Input: key, Output: key, Summary: key}},
		Changes: []FileChange{{Hunks: []Hunk{{Old: key, New: key}}}},
	}
	RedactTranscript(tr)
	e, h := tr.Events[0], tr.Changes[0].Hunks[0]
	for _, s := range []string{tr.Session.Title, tr.Session.Prompt, e.Text, e.Input, e.Output, e.Summary, h.Old, h.New} {
		if strings.Contains(s, "sk-ant") {
			t.Errorf("left a key: %q", s)
		}
	}
}
