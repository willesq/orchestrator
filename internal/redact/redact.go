// Package redact replaces secret-shaped substrings with markers before content is
// ingested into RAG. Used by the source refreshers (repo/web docs) and the
// PostToolUse hook (native config), mirroring the Python system's redaction.
package redact

import "regexp"

type rule struct {
	label string
	re    *regexp.Regexp
}

var rules = []rule{
	{"private-key", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{"aws-access-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"github-token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
	{"github-token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
	{"anthropic-key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`)},
	// Project/service-account keys carry a hyphenated prefix the legacy rule below
	// cannot cross, so without this they would pass through intact.
	{"openai-key", regexp.MustCompile(`\bsk-(?:proj|svcacct|admin)-[A-Za-z0-9_-]{20,}`)},
	{"openai-key", regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)},
}

// Redact replaces secret-shaped substrings and returns the sanitized text plus
// the number of redactions performed.
func Redact(text string) (string, int) {
	count := 0
	for _, r := range rules {
		text = r.re.ReplaceAllStringFunc(text, func(string) string {
			count++
			return "[REDACTED:" + r.label + "]"
		})
	}
	return text, count
}

// Threshold is the redaction count above which a file is treated as a keystore
// (mostly secrets, low doc value) and should be skipped entirely by callers.
const Threshold = 25
