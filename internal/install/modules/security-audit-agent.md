---
title: "Security audit agent"
group: Security
order: 180
sensitive: true
description: "Invoke the security-audit subagent and scanners before commits and PRs."
---

### 🔒 Security — how to invoke the audit agent

The shared policy requires security review before commits and PRs. Use the `security-audit`
subagent via the `Agent` tool:

- **Before commit:** `Agent(subagent_type="security-audit", prompt="Audit the following changed files for security vulnerabilities: [files]")`
- **Before PR:** `Agent(subagent_type="security-audit", prompt="Perform a comprehensive security audit of all changes in the current branch compared to main/master")`
- **Feature-specific:** `Agent(subagent_type="security-audit", prompt="Audit [feature] for security vulnerabilities, focusing on [auth / input validation / …]")`

When available, also run `bandit -r . -ll`, `safety check`, `semgrep --config=auto .`, and
`gitleaks detect --source .` before commits.

**Ordering before any commit:** tests pass → coverage meets target → security audit → commit.

**Findings response:** Critical/High → fix immediately. Medium → fix before committing. Low →
record as a follow-up in the project's task log. Accepted risk → document why, in a code
comment or the project's planning doc.
