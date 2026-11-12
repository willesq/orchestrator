# CLI

`orch` is a cobra command tree (`internal/cli`). Commands that need orchd dial its socket; those
marked *operator-only* refuse when `ORCH_TASK_ID` is set (inside a queued task).

| Command | Purpose |
|---|---|
| `version`, `ping` | build version; orchd reachability |
| `status [--json]` | whole-system report, including the `safety-net` line; works with orchd down; always exits 0 |
| `check` | pass/fail preconditions: ONNX Runtime dylib, embedding model, `gh auth status`, orchd ping |
| `config get\|set\|list` | settings ([config.md](config.md)) |
| `daemon status\|start\|stop\|restart` | orchd via `brew services` or launchd |
| `restart [orchd\|subsystems\|<unit>] [--list]` | bounce orchd, or restart units in place |
| `install [--apply] [--target] [--home] [--bin-dir]` | wire harnesses, and for Claude Code the pinned cc-safety-net and its guard rules ([install.md](install.md)) |
| `mcp rag\|queue` | stdio MCP servers |
| `ingest text --source-uri <uri> [--type] [--scope] [--tag]` | store stdin |
| `search <query> [--top-k] [--scope] [--type] [--tag] [--require-tag] [--uri] [--mode] [--ranking] [--per-source] [--since] [--until]` | search memory |
| `rag sources [--type] [--scope] [--prefix] [--limit] [--since] [--until]` | list stored sources |
| `rag forget <uri> --yes` | delete one source |
| `rag eval [--set] [--top-k] [--save] [--compare] [--mode] [--ranking] [--per-source] [--section] [--split] [--inject-format] [--cases] [--json]`, `rag eval add <query> <uri>… [--span] [--section]`, `rag eval seed` | retrieval golden set, scoring and paired-bootstrap comparison ([memory.md](memory.md#retrieval-evaluation)) |
| `rag why [prompt] [--cwd] [--json]` | the prompt from stdin when omitted or `-`; replay prompt context for a prompt: whether it would search, each candidate's score and verdict, the exact block; changes nothing ([hooks.md](hooks.md#prompt-context)) |
| `rag backfill-sessions [--all [--summaries]] [--apply]` | store undated sessions again, or with `--all` every transcript still on disk, without harness text, and with `--summaries` their summaries too ([memory.md](memory.md#transcripts-and-harness-text)) |
| `rag dedupe [--apply]` | delete path-keyed copies identical to a synced doc of a registered source (*operator-only*) |
| `rag reindex-keywords [--apply]` | rebuild keyword vectors for the current tokenizer, in place (*operator-only*) |
| `rag migrate-hybrid [--target] [--apply] [--rollback] [--drop-backup]` | layout migration (*operator-only*) |
| `rag backfill-sessions [--apply]` | date older sessions (*operator-only*) |
| `source add\|list\|remove\|refresh [--confirm-prune]`, `repo add\|list` | cadence-refreshed sources; `--confirm-prune` lets the next refresh of one source prune past the cap (through orchd, operator-only) |
| `docs add\|list\|owners list\|status\|sync [--confirm-prune]\|refresh\|remove` | GitHub owners' markdown |
| `dispatch <body> [--agent] [--cwd] [--kind] [--model]` | queue a task (refused inside a task) |
| `tasks [--status] [--agent] [--limit] [--stats]`, `task <id> [--rate]` | task ledger; rating refused inside a task |
| `model fetch\|refresh\|list\|enable\|disable\|tier`, `provider list\|enable\|disable` | embedding model and model registry (changes *operator-only*) |
| `routing show\|evaluate\|apply\|rollback\|set\|clear` | routing table (changes *operator-only*) |
| `qdrant status\|cutover\|migrate` | vector store engine |
| `github scopes [--refresh] [--json]`, `github enable <owner> [--all-items\|--everything-in <repos>] [--include\|--exclude <repos>]`, `github disable <owner>`, `github teams [--refresh]`, `github repos <owner>` | choose which GitHub accounts and orgs orch monitors (enable/disable *operator-only*) |
| `spaces draft\|diff [--full]\|apply --revision N [--accept-agent-draft] [--confirm-github-org]` | the spaces policy: save a draft, review it, enforce it (diff and apply *operator-only*, refused inside harness sessions too; [config.md](config.md#spaces-policy)) |
| `spaces why <path\|url\|owner/repo\|slack:<id>\|teams:<id>>` | how the applied policy places a target: matching roots shallowest first, the remote's effect, the deciding rule and the result (`ExplainPlacement`); a path starts with `/`, `~` or `.` and is made absolute before it is sent. Agents get an answer only when it names nothing but public spaces ([memory.md](memory.md#placement)) |
| `spaces log [<session>\|task:<id>\|process:<pid>] [--after N] [--limit N]` | a session's, task's or unpinned harness process's [command log](daemon.md#command-log) records in order: time, event, tool, decision and the command, directory move or gap (`ListToolEvents`). Outside any agent session any log can be read and one must be named; inside a session only its own, which is also what no argument prints. Use `session:<id>` for a session ID named like a subcommand |
| `spaces log summary [--since 168h]` | per space over the last week (or `--since`): sessions with events, tool calls, directory moves, moves into another space under the applied policy, and refusals; tasks and sessions with no space show as `-` (`SummarizeToolLog`, *operator-only*) |
| `spaces enforce [<layer> <state>] [--for 15m]` | with no arguments, every [enforcement switch](daemon.md#enforcement-switches): state, default, since when, timer and (for the operator) who set it (`GetEnforcement`). With a layer (`memory`, `guard`, `terms`, `sandbox`, `hooks`) and a state, switches it, optionally for a time (1 s to 24 h) after which it reverts (`SetEnforcement`, *operator-only*, refused inside harness sessions too); prints what the hooks switch did to Claude Code's settings. `orch status` names every layer that is off, and since when, in its `spaces` section |
| `spaces log verify <session\|task:<id>\|process:<pid>>` | recompute a session's, task's or unpinned process's [command log](daemon.md#command-log) hash chain and list every modified, missing or moved record, and every anchor that disagrees with its copy in `orch.db`; exits non-zero when broken (`VerifyToolLog`, *operator-only*) |
| `spaces replay <session\|task:<id>\|process:<pid>> [--policy <rev\|file>] [--mode M] [--all]` | [policy replay](daemon.md#policy-replay): orchd decides the log's recorded tool calls again with the guard's own `Decide` and lists each call whose verdict or rule differs from the recorded one, with both rule ids; counts calls replayed, compared, changed, with no recorded verdict, and not decided (their revision no longer verifies). Each call is decided under the revision applied when it was recorded unless `--policy` names a saved revision (digits) or a policy file; `--mode` judges in `off`, `log`, `block-other-jobs` or `strict` (default: the guard switch); `--all` lists every call (`ReplaySpaces`, *operator-only*, refused inside harness sessions too). Changes nothing |
| `spaces replay <payloads.jsonl\|folder> --policy <file> [--mode M] [--all]` | the same over recorded hook payload files (the SP-001 fixtures in `internal/hook/testdata/payloads`), in the CLI without orchd: each session is pinned to the space of its first directory, targets are read from each payload as orchd reads a live call's, the guard protects what orchd's does (`spaces.GuardProtection`: orch's directories from `ORCH_CONFIG_DIR`/`ORCH_DATA_DIR`, the home, the binaries beside `orch`, the policy's terms files; RC-110), and the mode defaults to `block-other-jobs`. An argument holding `/` or `.` is a path. Open to every caller: it reads only the files named, never orchd's log or stored revisions; only regular files are read, at most 50,000 calls per file. Payloads carry no recorded verdict, so `--all` lists the verdicts; `TestSpacesReplayFixtures` runs this in CI and compares it with `internal/cli/testdata/replay/fixtures.golden` |
| `sessions list [--all]` | pinned claude and codex sessions with harness, session, PID, space, pinned directory, state (`active`, `idle`, `ended`, `unpinned`), last activity, tool events and refusals; ended ones only with `--all`; running harness processes with no pin as `unpinned`, with the tool events their own command log chain holds (with `--all` also those of unpinned processes since exited). Outside any agent session every session is shown, inside one only that session's row ([session liveness](daemon.md#session-liveness), `ListSessions`) |
| `settings list [--provider] [--kind] [--json]`, `settings enable\|disable <kind> <id> [--provider]` | managed Claude Code hooks, permissions and plugins and cc-safety-net rules: list with state and origin, turn one on or off (enable/disable *operator-only*; [settings.md](settings.md)) |
| `prs [--blocked\|--ready\|--needs-me] [--json]` | cached status of PRs in your enabled GitHub scopes (never calls `gh`); `--needs-me` lists what needs you, most urgent first |
| `prs watches` | the PRs orchd watches for review: state, what each may post, verdict and note, including why a watch ended (merged or closed, or the 14-day limit) (`ListPRReviews`) |
| `prs stop <pr-url> [--forget]` | stop watching a PR, or also delete its verdict (`StopPRReview`); says how to cancel a review still running (*operator-only*) |

## MCP tools

| Server | Tool | Parameters |
|---|---|---|
| `rag` | `rag_search` | query, top_k (5), scope, source_type, tags (any), all_tags, source_uri, mode, per_source, since, until |
| `rag` | `rag_ingest_text` | text, source_uri, source_type (`manual`), scope; same URI replaces |
| `rag` | `rag_list_sources` | source_type, scope, uri_prefix, limit (50), since, until |
| `rag` | `rag_delete_source` | source_uri, confirm (must be true) |
| `queue` | `queue_dispatch` | body, agent (`dev`), cwd, task_kind, model; refused inside a task |
| `queue` | `queue_list`, `queue_read` | status, agent, limit (20); id |
| `queue` | `model_recommend` | task_kind |

Numeric arguments are clamped (negative → default, above 10000 → 10000); orchd still rejects a
search `top_k` above 100. Results are plain text;
search hits read `N. [score] "<uri>" ("<type>") · <when> · <trust label>` (URI and type quoted)
followed by the chunk text fenced as quoted data, in every tier, with instruction-like text
flagged ([hooks.md](hooks.md#prompt-context)). `rag_list_sources` quotes every stored field (URI,
type, scope, tags), and so does `orch search` (URI and type, with the text on one line).
