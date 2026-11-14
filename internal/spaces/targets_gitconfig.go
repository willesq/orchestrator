package spaces

import (
	"errors"
	"strings"
)

// parseGitConfig reads the parts of a git config file the live GitHub
// guard needs (spaces plan SP-043): remotes' url and pushurl, remote
// groups, branch remotes, remote.pushDefault, and whether URLs can be
// rewritten (url.*.insteadOf, includes). It follows git's syntax closely
// enough not to be fooled by a comment after a value, a key on a section
// header line, a legacy [remote.name] header, quoting or a continued line;
// a file it cannot read is an error, never a partial result.
func parseGitConfig(b []byte) (*gitConfig, error) {
	c := &gitConfig{remotes: map[string][]string{}, groups: map[string][]string{},
		branchRemote: map[string]string{}, pushRemote: map[string]string{}}
	section, sub := "", ""
	for _, line := range logicalLines(string(b)) {
		l := strings.TrimLeft(line, " \t")
		if l == "" || l[0] == '#' || l[0] == ';' {
			continue
		}
		if l[0] == '[' {
			s, n, rest, err := parseSection(l)
			if err != nil {
				return nil, err
			}
			section, sub, l = s, n, strings.TrimLeft(rest, " \t")
			if l == "" || l[0] == '#' || l[0] == ';' {
				continue
			}
		}
		key, val, err := parseEntry(l)
		if err != nil {
			return nil, err
		}
		c.take(section, sub, key, val)
	}
	return c, nil
}

// take records one entry.
func (c *gitConfig) take(section, sub, key, val string) {
	switch {
	case section == "remote" && sub != "" && (key == "url" || key == "pushurl"):
		c.remotes[sub] = append(c.remotes[sub], val)
	case section == "remote" && sub == "" && key == "pushdefault":
		c.pushDefault = val
	case section == "remotes" && sub == "":
		c.groups[key] = append(c.groups[key], strings.Fields(val)...)
	case section == "branch" && sub != "" && key == "remote":
		c.branchRemote[sub] = val
	case section == "branch" && sub != "" && key == "pushremote":
		c.pushRemote[sub] = val
	case section == "url" && (key == "insteadof" || key == "pushinsteadof"),
		section == "include" || section == "includeif":
		c.rewrites = true
	}
}

// logicalLines splits text into lines, joining a line that ends in an
// unescaped backslash with the next, as git does.
func logicalLines(text string) []string {
	var out []string
	var cur strings.Builder
	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		n := len(raw) - len(strings.TrimRight(raw, `\`))
		if n%2 == 1 {
			cur.WriteString(raw[:len(raw)-1])
			continue
		}
		cur.WriteString(raw)
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

var errGitConfig = errors.New("git config could not be read")

// parseSection reads a section header: [name], [name "sub"] or the legacy
// [name.sub], whose sub is case-insensitive. rest is what follows "]".
func parseSection(l string) (section, sub, rest string, err error) {
	l = l[1:]
	i := 0
	for i < len(l) && (isAlnum(l[i]) || l[i] == '-' || l[i] == '.') {
		i++
	}
	name := l[:i]
	l = l[i:]
	if i == 0 {
		return "", "", "", errGitConfig
	}
	if strings.HasPrefix(l, "]") {
		section, sub, _ = strings.Cut(name, ".")
		return strings.ToLower(section), strings.ToLower(sub), l[1:], nil
	}
	l = strings.TrimLeft(l, " \t")
	if !strings.HasPrefix(l, `"`) || strings.Contains(name, ".") {
		return "", "", "", errGitConfig
	}
	var b strings.Builder
	for j := 1; j < len(l); j++ {
		switch l[j] {
		case '\\':
			if j+1 >= len(l) {
				return "", "", "", errGitConfig
			}
			j++
			b.WriteByte(l[j])
		case '"':
			if j+1 >= len(l) || l[j+1] != ']' {
				return "", "", "", errGitConfig
			}
			return strings.ToLower(name), b.String(), l[j+2:], nil
		default:
			b.WriteByte(l[j])
		}
	}
	return "", "", "", errGitConfig
}

// parseEntry reads "key = value": the key lower-cased, the value with
// quotes and escapes removed and a trailing comment dropped. A key with no
// "=" is a boolean true.
func parseEntry(l string) (key, val string, err error) {
	i := 0
	for i < len(l) && (isAlnum(l[i]) || l[i] == '-') {
		i++
	}
	if i == 0 {
		return "", "", errGitConfig
	}
	key = strings.ToLower(l[:i])
	rest := strings.TrimLeft(l[i:], " \t")
	if rest == "" || rest[0] == '#' || rest[0] == ';' {
		return key, "true", nil
	}
	if rest[0] != '=' {
		return "", "", errGitConfig
	}
	val, err = parseValue(strings.TrimLeft(rest[1:], " \t"))
	return key, val, err
}

// parseValue reads a value: double quotes keep spaces and comment
// characters, backslash escapes \n, \t, \b, \" and \\, and an unquoted ;
// or # starts a comment. Unquoted trailing space is dropped.
func parseValue(s string) (string, error) {
	var b strings.Builder
	quoted, keep := false, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			quoted = !quoted
			keep = b.Len()
		case c == '\\':
			if i+1 >= len(s) {
				return "", errGitConfig
			}
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case '"', '\\':
				b.WriteByte(s[i])
			default:
				return "", errGitConfig
			}
			keep = b.Len()
		case !quoted && (c == ';' || c == '#'):
			return b.String()[:keep], nil
		default:
			b.WriteByte(c)
			if quoted || (c != ' ' && c != '\t') {
				keep = b.Len()
			}
		}
	}
	if quoted {
		return "", errGitConfig
	}
	return b.String()[:keep], nil
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
