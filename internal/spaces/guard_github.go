package spaces

import "strings"

// The live GitHub guard (spaces plan SP-043). Decide judges GitHub repos by
// their owner's space (other_job_repo); this judges the accounts a call
// reaches without naming one repo, such as gh api orgs/<o> or gh repo list
// <o>, the same way.

// validAccount reports whether org is a login as Targets reports one:
// lowercase owner characters, no slash.
func validAccount(org string) bool {
	return org != "" && !strings.Contains(org, "/") && repoPattern.MatchString(org+"/x")
}

// judgeOrgs places the accounts a call reaches: the furthest reach of any,
// and whether one could not be read.
func (g guardScope) judgeOrgs(orgs []string) (out reach, malformed bool) {
	for _, org := range orgs {
		if !validAccount(org) {
			malformed = true
			continue
		}
		space, _ := g.pol.res.GitHubOrgSpace(org)
		out = max(out, g.judge(space))
	}
	return out, malformed
}
