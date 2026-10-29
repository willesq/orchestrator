---
title: "Security and credential handling"
group: Security
order: 50
sensitive: true
description: "Security review before commits and the shared credential boundary."
---

## Security and credential handling

- Perform a security review of changed material before commits and a broader
  review before opening a PR. Use a dedicated security-review capability when
  available; otherwise perform and report the relevant checks directly.
- Review authentication, authorization, sessions, payments, file handling,
  cryptography, data access, external commands, dependencies, and user input for
  security regressions when those areas change.
- Validate input at trust boundaries. Check permissions before sensitive actions.
  Use parameterized queries or safe ORM operations; never construct executable
  SQL from untrusted string interpolation.
- Execute external commands through argument arrays rather than a shell when
  possible. Never evaluate untrusted input as code, and guard file operations
  against path traversal and unintended writes.
- Avoid unsafe deserialization, including untrusted pickle data and unrestricted
  YAML loaders. Choose safe parsers. Use maintained password-hashing libraries
  and established cryptographic interfaces rather than custom schemes.
- Keep secrets, credentials, and personal data out of source control, indexed
  memory, generated reports, logs, error messages, and temporary artifacts.
- Use existing authenticated tools and credential stores. A coding agent must not
  retrieve, capture, print, or persist the user's live token, cookie, private key,
  passphrase, keychain secret, or other credential into its context or a new file.
- Do not run `gh auth token`, inspect token-bearing authentication files, or
  extract credentials from processes. Authentication metadata such as
  `gh auth status` may be used when it does not expose credential values.
- Do not construct shell commands or suggest workarounds that fetch a live token
  into an environment variable, temporary file, shell profile, wrapper, or output
  stream. Renaming variables or hiding the retrieval in another language does
  not change this boundary.
- Code authored for an application may pass credentials internally through its
  documented subprocess configuration or credential provider when needed. The
  actual values must remain within that application's supported flow and never
  enter the coding agent's context, generated files, or persistent diagnostics.
- Reuse lockfiles and pinned dependencies; check new dependencies for known
  vulnerabilities. Use available project scanners for secrets, dependencies, and
  unsafe patterns without printing live credentials.
- Resolve critical, high, and medium security findings before committing affected
  behavior. Record lower-risk follow-ups and document any accepted risk explicitly.
