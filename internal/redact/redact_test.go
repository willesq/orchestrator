package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantCount int
		wantGone  string
	}{
		{name: "clean text untouched", in: "hello world", wantCount: 0},
		{name: "github token", in: "token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", wantCount: 1, wantGone: "ghp_"},
		{name: "aws key", in: "key AKIAIOSFODNN7EXAMPLE here", wantCount: 1, wantGone: "AKIA"},
		{name: "openai key", in: "sk-abcdefghijklmnopqrstuvwxyz012345", wantCount: 1, wantGone: "sk-abc"},
		{name: "openai project key", in: "key sk-proj-" + strings.Repeat("Ab3_-x", 8) + " end", wantCount: 1, wantGone: "sk-proj-"},
		{name: "openai service account key", in: "sk-svcacct-" + strings.Repeat("Zq9", 10), wantCount: 1, wantGone: "sk-svcacct-"},
		{name: "github fine-grained pat", in: "pat github_pat_" + strings.Repeat("A1b2C", 6) + "_" + strings.Repeat("x9", 20), wantCount: 1, wantGone: "github_pat_"},
		{name: "anthropic key not double counted", in: "sk-ant-" + strings.Repeat("k7", 15), wantCount: 1, wantGone: "sk-ant-"},
		{name: "aws secret key assignment", in: "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY aws s3 ls", wantCount: 1, wantGone: "wJalrXUtnFEMI"},
		{name: "aws session token in config", in: "aws_session_token = \"FwoGZXIvYXdzEJr//////////wEaDE5ample\"", wantCount: 1, wantGone: "FwoGZXIv"},
		{name: "bearer header", in: `curl -H "Authorization: Bearer abc123def456ghi789" https://x`, wantCount: 1, wantGone: "abc123def456"},
		{name: "aws secret after a space", in: "aws configure set aws_secret_access_key wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", wantCount: 1, wantGone: "wJalrXUtnFEMI"},
		{name: "aws secret json-escaped", in: `{"content":"AWS_SECRET_ACCESS_KEY=\"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\""}`, wantCount: 1, wantGone: "wJalrXUtnFEMI"},
		{name: "aws secret key variant", in: "export AWS_SECRET_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", wantCount: 1, wantGone: "wJalrXUtnFEMI"},
		{name: "url user info", in: "psql postgres://app:s3cretPw@db.internal:5432/x", wantCount: 1, wantGone: "s3cretPw"},
		{name: "https user info", in: "git clone https://u:p4ssw0rd@git.example.com/r.git", wantCount: 1, wantGone: "p4ssw0rd"},
		{name: "curl -u", in: "curl -u admin:hunter22 https://x", wantCount: 1, wantGone: "hunter22"},
		{name: "secret variable", in: "PGPASSWORD=hunter22 psql -h db", wantCount: 1, wantGone: "hunter22"},
		{name: "secret variable quoted", in: `MY_API_KEY="abcdef123456" ./run`, wantCount: 1, wantGone: "abcdef123456"},
		{name: "password key", in: `{"password": "hunter22"}`, wantCount: 1, wantGone: "hunter22"},
		{name: "mysql -p", in: "mysql -u root -pHunter22 db", wantCount: 1, wantGone: "Hunter22"},
		{name: "docker login -p", in: "docker login -u me -p hunter22 registry.example.com", wantCount: 1, wantGone: "hunter22"},
		{name: "bearer url-encoded", in: "curl 'https://x?h=Authorization:%20Bearer%20abcdefgh12345678'", wantCount: 1, wantGone: "abcdefgh1234"},
		{name: "npm token", in: "//registry.npmjs.org/:_authToken=npm_" + strings.Repeat("a1B2", 9), wantCount: 1, wantGone: "npm_a1B2"},
		{name: "gitlab token", in: "glpat-" + strings.Repeat("xY_1", 5), wantCount: 1, wantGone: "glpat-"},
		{name: "google api key", in: "key=AIza" + strings.Repeat("Sy9_", 8) + "abc", wantCount: 1, wantGone: "AIzaSy9"},
		{name: "slack refresh token", in: "xoxe.xoxp-1-" + strings.Repeat("Ab3", 10), wantCount: 1, wantGone: "xoxe.xoxp"},
		{name: "curl -u without space", in: "curl -uadmin:hunter22 https://x", wantCount: 1, wantGone: "hunter22"},
		{name: "query api key", in: "curl 'https://api.example.com/v1?api_key=abcd1234efgh5678'", wantCount: 1, wantGone: "abcd1234efgh"},
		{name: "access token query", in: "https://x/cb?code=1&access_token=abcd1234efgh5678", wantCount: 1, wantGone: "abcd1234efgh"},
		{name: "x-api-key header", in: `curl -H "X-Api-Key: abcd1234efgh5678" https://x`, wantCount: 1, wantGone: "abcd1234efgh"},
		{name: "cookie header", in: `curl -H 'Cookie: session=abcd1234efgh5678; theme=dark' https://x`, wantCount: 1, wantGone: "abcd1234efgh"},
		{name: "token option", in: "vault login --token s.abcd1234efgh5678", wantCount: 1, wantGone: "abcd1234efgh"},
		{name: "lowercase db_password", in: "db_password=hunter22 ./migrate", wantCount: 1, wantGone: "hunter22"},
		{name: "docker -u uid kept", in: "docker run -u 1000:1000 alpine", wantCount: 0},
		{name: "git push -u kept", in: "git push -u origin:main", wantCount: 0},
		{name: "pem header in prose kept", in: "If your key starts with -----BEGIN OPENSSH PRIVATE KEY-----, convert it.\n## Next section", wantCount: 0},
		{name: "password type kept", in: "| password: Optional[str] |", wantCount: 0},
		{name: "cookie prose kept", in: "**Cookie:** the browser stores it", wantCount: 0},
		{name: "go test -p kept", in: "After login, run go test -p 4 ./...", wantCount: 0},
		{name: "token argument kept", in: "Pass --token argument", wantCount: 0},
		{name: "url then -u kept", in: "see https://example.com -u bob:pw", wantCount: 0},
		{name: "max tokens kept", in: "MAX_TOKENS=4096 ./run", wantCount: 0},
		{name: "secret word mid-name", in: "GH_TOKEN_FILE=/run/secrets/gh_token_value ./x", wantCount: 1, wantGone: "/run/secrets"},
		{name: "variable reference kept", in: "GH_TOKEN=$GH_TOKEN gh pr list", wantCount: 0},
		{name: "plain url kept", in: "https://github.com/willesq/orchestrator", wantCount: 0},
		{name: "private key block", in: "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----", wantCount: 1, wantGone: "BEGIN RSA"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, n := Redact(tt.in)
			if n != tt.wantCount {
				t.Errorf("count = %d, want %d (out=%q)", n, tt.wantCount, out)
			}
			if tt.wantGone != "" && strings.Contains(out, tt.wantGone) {
				t.Errorf("secret not redacted: %q still in %q", tt.wantGone, out)
			}
			if tt.wantCount > 0 && !strings.Contains(out, "[REDACTED:") {
				t.Errorf("no redaction marker in %q", out)
			}
		})
	}
}

func TestRedactKeepsAssignedName(t *testing.T) {
	out, _ := Redact("export AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	if want := "export AWS_SECRET_ACCESS_KEY=[REDACTED:aws-secret]"; out != want {
		t.Errorf("Redact = %q, want %q", out, want)
	}
}

// TestRedactCommand: the command log also removes forms the shared rules
// keep to spare ingested documents.
func TestRedactCommand(t *testing.T) {
	tests := []struct{ name, in, gone string }{
		{"short --password", "mysqldump --password hunter2 db", "hunter2"},
		{"--password with symbols", "az login -u a@b.com --password Winter2024!", "Winter2024"},
		{"oc login -p", "oc login -u x -p s3cretpw https://api", "s3cretpw"},
		{"argocd login", "argocd login cd.example.com --password s3cretpw", "s3cretpw"},
		{"yaml password", "db:\n  password: s3cretPa55\n  user: app", "s3cretPa55"},
		{"yaml env password", "    POSTGRES_PASSWORD: example123", "example123"},
		{"yaml in json-escaped write", `{"content":"spring:\n  datasource:\n    password: s3cretPa55\n"}`, "s3cretPa55"},
		{"quoted -p", "az login -u me -p 'Pw12!x'", "Pw12!x"},
		{"double-quoted --password", `x --password "secret"`, "secret"},
		{"json-escaped quoted --password", `{"command":"x --password \"s3cret!\""}`, "s3cret"},
		{"quoted docker login", `docker login -u me -p "Pw12!x" r.io`, "Pw12!x"},
		{"yaml last line of json string", `{"content":"db:\n  password: hunter22"}`, "hunter22"},
		{"yaml trailing spaces", `{"content":"password: hunter22   \nx: 1"}`, "hunter22"},
		{"yaml trailing tab", `{"content":"password: hunter22\t\nx: 1"}`, "hunter22"},
		{"yaml crlf", `{"content":"password: hunter22\r\nx: 1"}`, "hunter22"},
		{"yaml comment", "password: hunter22 # dev only", "hunter22"},
		{"unterminated key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA", "MIIEow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, n := RedactCommand(tt.in)
			if n == 0 || strings.Contains(out, tt.gone) {
				t.Errorf("RedactCommand(%q) = %q", tt.in, out)
			}
			if strings.HasPrefix(tt.in, "{") {
				var v map[string]string
				if json.Unmarshal([]byte(out), &v) != nil {
					t.Errorf("RedactCommand(%s) = %s does not parse", tt.in, out)
				}
			}
			if shared, _ := Redact(tt.in); tt.name == "unterminated key" && shared != tt.in {
				t.Errorf("shared Redact changed an unterminated key header: %q", shared)
			}
		})
	}
	if out, _ := RedactCommand("password: the one you set earlier"); out != "password: the one you set earlier" {
		t.Errorf("prose changed: %q", out)
	}
	out, _ := RedactCommand(`{"content":"password: s3cretPa55\nnext: 1"}`)
	var v map[string]string
	if json.Unmarshal([]byte(out), &v) != nil || !strings.Contains(out, `[REDACTED:password]\nnext`) {
		t.Errorf("escaped newline not kept: %s", out)
	}
}

func TestRedactKeepsURLShape(t *testing.T) {
	out, _ := Redact("postgres://app:s3cretPw@db.internal/x")
	if want := "postgres://app:[REDACTED:url-password]@db.internal/x"; out != want {
		t.Errorf("Redact = %q, want %q", out, want)
	}
}

// TestRedactKeepsJSONEscapes: an option's value never takes the backslash of
// an escaped quote, so redacted JSON still parses.
func TestRedactKeepsJSONEscapes(t *testing.T) {
	for _, in := range []string{
		`{"command":"curl -u a:secretpw\" x"}`,
		`{"command":"mysql -pHunter22\" db"}`,
		`{"command":"docker login -p hunter22\""}`,
	} {
		out, n := Redact(in)
		var v map[string]string
		if n == 0 || json.Unmarshal([]byte(out), &v) != nil {
			t.Errorf("Redact(%s) = %s (%d redactions) does not parse", in, out, n)
		}
	}
}
