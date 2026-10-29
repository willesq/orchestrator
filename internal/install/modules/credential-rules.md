---
title: "Credential handling hard rules"
group: Security
order: 190
sensitive: true
description: "Never bring a live credential into the session, however it is spelled."
---

#### User credential handling — HARD RULES (no exceptions, no obfuscation)

The shared policy states this boundary. These are the specifics it doesn't enumerate, and they
are **load-bearing** — a violation is a credential incident even if the task ends up working.

**Covered credential types** (non-exhaustive but explicit): API tokens / keys (gh, GitHub
fine-grained PATs, GitHub App installation tokens, Azure DevOps PATs, AWS access keys, AWS
session tokens, Azure CLI access tokens, OpenAI/Anthropic/etc. API keys, npm, PyPI, Docker Hub),
browser session cookies, already-minted OAuth refresh/access tokens, SSH private keys, GPG/SSH
passphrases, macOS Keychain entries, kubeconfig user credentials / service-account tokens,
`~/.aws/credentials`, `~/.netrc`, OIDC ID tokens, JWT bearer tokens captured from a live
session — anything that would let an attacker act as the user.

The rules are **intent-based, not pattern-based**: if the OUTCOME is a live credential in a new
location *that this Claude session caused to exist*, it's forbidden — no matter how it's spelled
or which credential type it is. **Never obfuscate around it**: renaming the env var, wrapping
the fetch in a shell function, `subprocess.check_output(["gh","auth","token"])`, scripting it
inside a brew command — all the same violation. The user said "i dont care how you obfuscate
it" (2026-06-03). Equally forbidden to **suggest** the user run such a command; if the only
viable diagnosis requires materializing the token that way, stop and surface the constraint.

**Scope line (2026-06-03):** these rules govern *what this session does*, not what the code it
authors does. The test is: "is the token's value passing through chat / my context / a place it
didn't already live?" If yes, forbidden. If it's flowing through code we wrote into a process
that needs it, and never lands in chat or persistently on disk, it's fine. So this is allowed:
writing code in `custom_download_strategy.rb`, a formula, or a workflow step that sets
`ENV["HOMEBREW_GITHUB_API_TOKEN"]` programmatically as normal subprocess configuration, or that
calls `gh auth token` and consumes the result within the same process. I write the code that
handles the token; I never read its bytes.

**When a tap/formula auth flow fails, allowed moves:**
- Read `custom_download_strategy.rb` or similar and propose code-level fixes to how it obtains
  credentials. Don't print the token value when testing the change.
- `gh auth status` is fine (it elides the token). `grep oauth_token ~/.config/gh/hosts.yml` is NOT.
- Inspect Homebrew sandbox semantics; write diagnostics that don't shell out to `gh auth token`.
- Open upstream Homebrew issues with anonymised reproduction steps.
- Ask the user how they want to proceed.
