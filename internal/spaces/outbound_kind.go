package spaces

import "strings"

// A refusal names the action it refused by the command's own words, which
// the session wrote and which may hold a term: an alias, a gh subcommand,
// an API method (INV-07). safeKind keeps only words from gh's and git's
// own vocabulary.

// ghKindWords are gh subcommands and HTTP methods a refusal may name.
var ghKindWords = flagSet("create", "edit", "comment", "review", "merge", "close", "reopen", "delete",
	"ready", "upload", "set", "add", "remove", "run", "rename", "archive", "unarchive", "fork", "sync",
	"transfer", "lock", "unlock", "pin", "unpin", "develop", "cancel", "rerun", "enable", "disable",
	"update", "upgrade", "install", "login", "logout", "refresh", "setup-git", "switch", "deploy-key",
	"link", "unlink", "copy", "revert", "import", "set-default", "item-add", "item-edit", "item-create",
	"item-delete", "item-archive", "field-create", "field-delete", "mark-template", "exec", "trust",
	"POST", "PUT", "PATCH", "DELETE", "GET", "HEAD")

// safeKind is kind with every word after git or gh that is not one of
// their own commands dropped from there on: "gh pr <word>" becomes
// "gh pr", and an alias "a git command" or "a gh command".
func safeKind(kind string) string {
	f := strings.Fields(kind)
	if len(f) < 2 || f[0] != "git" && f[0] != "gh" {
		return kind
	}
	known := func(w string) bool {
		if f[0] == "git" {
			return gitKnown[w] || gitSends[w]
		}
		if ghKnown[w] || ghKindWords[w] {
			return true
		}
		for _, subs := range ghReadOnly {
			if subs[w] {
				return true
			}
		}
		return false
	}
	n := 1
	for n < len(f) && known(f[n]) {
		n++
	}
	if n == 1 {
		return "a " + f[0] + " command"
	}
	return strings.Join(f[:n], " ")
}
