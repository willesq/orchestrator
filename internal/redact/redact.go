// Package redact replaces secret-shaped substrings with markers before content is
// ingested into RAG or stored in the command log. Used by the source refreshers
// (repo/web docs), the PostToolUse hook (native config) and orchd's command log,
// mirroring the Python system's redaction.
package redact

import "regexp"

// A rule whose pattern has capture groups keeps them: the first before the
// marker (the name a secret is assigned to, so "AWS_SECRET_ACCESS_KEY=..."
// still says what was removed) and the second after it (the "@" of a URL's
// user info). Other rules must use only non-capturing groups.
type rule struct {
	label string
	re    *regexp.Regexp
}

var rules = []rule{
	{"private-key", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{"aws-access-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"github-token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
	{"github-token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[abeprs](?:\.xox[bp])?-[A-Za-z0-9-]{10,}\b`)},
	{"npm-token", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{"gitlab-token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{"anthropic-key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`)},
	// Project/service-account keys carry a hyphenated prefix the legacy rule below
	// cannot cross, so without this they would pass through intact.
	{"openai-key", regexp.MustCompile(`\bsk-(?:proj|svcacct|admin)-[A-Za-z0-9_-]{20,}`)},
	{"openai-key", regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)},
	// An AWS secret key or session token has no prefix of its own; it is
	// found by the name it is assigned to in a command or config file.
	// The name may be followed by "=", ":" or a space ("aws configure set
	// aws_secret_access_key X"), and quotes may be JSON-escaped (tool input).
	{"aws-secret", regexp.MustCompile(`(?i)(\baws_(?:secret_access_key|secret_key|session_token)\\?["']?(?:\s*[=:]\s*|\s+)\\?["']?)[A-Za-z0-9/+=]{16,}`)},
	{"auth-header", regexp.MustCompile(`(?i)(\bauthorization(?::|%3a)(?:\s|%20)*(?:bearer|token|basic)(?:\s|%20)+)[A-Za-z0-9._~+/=-]{8,}`)},
	// A password in a URL's user info: scheme://user:secret@host.
	{"url-password", regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^\s:/@'"]+:)[^\s/@'"]+(@)`)},
	// The generic rules below skip a value starting with "[", one an
	// earlier rule already replaced.
	// An environment variable whose name says it holds a secret, set in a
	// command: PGPASSWORD=..., GH_TOKEN=..., OPENAI_API_KEY="...".
	// The secret word must end the name or be followed by "_", so
	// MAX_TOKENS=4096 is kept.
	{"secret-variable", regexp.MustCompile(`(\b[A-Z][A-Z0-9_]*(?:PASSWORD|PASSWD|SECRET|TOKEN|API_KEY|APIKEY|PRIVATE_KEY)(?:_[A-Z0-9_]*)?=\\?["']?)[^\s"'\\$\[][^\s"'\\]{5,}`)},
	// "password = x", or "password": "x"; an unquoted "password: x" is
	// prose or a schema as often as a secret, so it is kept.
	{"password", regexp.MustCompile(`(?i)((?:\b|_)(?:password|passwd)\\?["']?\s*(?:=\s*\\?["']?|:\s*\\?["']))[^\s"'\\\[][^\s"'\\]{3,}`)},
	// Secrets in a URL's query and in request headers.
	{"query-secret", regexp.MustCompile(`(?i)([?&](?:api_?key|access_token|auth_token|token|secret|password)=)[^&\s"'\\#\[]{8,}`)},
	{"secret-header", regexp.MustCompile(`(?i)(\b(?:x-api-key|x-auth-token|proxy-authorization):\s*)[^\s"'\\\[]{8,}`)},
	{"secret-header", regexp.MustCompile(`(?i)(\bcookie:\s*)[A-Za-z0-9_.-]+=[^"'\\\n]{7,}`)},
	// Secrets passed as options: curl -u user:pw, mysql -ppw, docker login
	// -p pw, --token X. Values never take a "\", so an escaped quote in
	// JSON stays escaped.
	{"password-option", regexp.MustCompile(`(\b(?:curl|wget|xh)\b[^\n;|&]*?\s(?:-u|--user)[\s=]*['"]?[^\s:'"\\]+:)[^\s'"\\]+`)},
	{"password-option", regexp.MustCompile(`(\b(?:mysql|mariadb|mysqldump|mysqladmin)\b[^\n;|&]*?\s-p)[^\s'"\\]+`)},
	{"password-option", regexp.MustCompile(`(\b(?:docker|podman|nerdctl|buildah|skopeo|helm|oras|crane)\b[^\n;|&]*?\blogin\b[^\n;|&]*?\s(?:-p|--password)[\s=]+\\?['"]?)[^\s'"\\-][^\s'"\\]*`)},
	{"password-option", regexp.MustCompile(`(\s--(?:token|api-key|password|secret)[\s=]+)[A-Za-z0-9._~+/=-]{16,}`)},
}

// commandRules are applied to the command log only, after rules. They
// favour removing a secret over keeping text, which suits a log of what
// agents ran but would damage ingested documents: a PEM header quoted in
// prose would lose the rest of the document.
var commandRules = []rule{
	// A key whose END line is missing (a capped field) is removed to the end.
	{"private-key", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*`)},
	// Any "login ... -p pw" (oc, argocd, cf, az, ...) and any --password.
	// A quoted value (possibly JSON-escaped) is found too; the value never
	// takes a "\", so JSON escapes stay intact.
	{"password-option", regexp.MustCompile(`(\blogin\b[^\n;|&]*?\s(?:-p|--password)[\s=]+\\?['"]?)[^\s'"\\-][^\s'"\\]*`)},
	{"password-option", regexp.MustCompile(`(\s--password[\s=]+\\?['"]?)[^\s'"\\$\[-][^\s'"\\]{3,}`)},
	// An unquoted YAML password ending its line, as in a config file an
	// agent writes: the value may be followed by spaces or tabs (also
	// JSON-escaped) and then the end of the line or text, a JSON-escaped
	// "\r" or "\n", the closing quote of a JSON string, or a comment.
	{"password", regexp.MustCompile(`(?im)((?:\b|_)(?:password|passwd)\s*:[ \t]*)[^\s"'\\\[][^\s"'\\]{3,}((?:[ \t]|\\t)*(?:$|\\r|\\n|\\?"|\s#))`)},
}

// Redact replaces secret-shaped substrings and returns the sanitized text plus
// the number of redactions performed.
func Redact(text string) (string, int) {
	return apply(rules, text)
}

// RedactCommand is Redact plus commandRules, for the command log.
func RedactCommand(text string) (string, int) {
	text, n := apply(rules, text)
	text, m := apply(commandRules, text)
	return text, n + m
}

func apply(rules []rule, text string) (string, int) {
	count := 0
	for _, r := range rules {
		text = r.re.ReplaceAllStringFunc(text, func(m string) string {
			count++
			var before, after string
			if r.re.NumSubexp() > 0 {
				if sub := r.re.FindStringSubmatch(m); sub != nil {
					before = sub[1]
					if len(sub) > 2 {
						after = sub[2]
					}
				}
			}
			return before + "[REDACTED:" + r.label + "]" + after
		})
	}
	return text, count
}

// Threshold is the redaction count above which a file is treated as a keystore
// (mostly secrets, low doc value) and should be skipped entirely by callers.
const Threshold = 25
