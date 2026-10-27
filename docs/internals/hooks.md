# Hooks and session memory

`orch-hook <event>` is the single binary every harness runs on lifecycle events. It reads the
event JSON from stdin and writes context for the agent to stdout as plain Markdown.

**Contract:** a hook never fails and never blocks the agent. Every handler returns success; slow
work is handed to orchd ([hand-offs](#hand-offs)); each orchd call has a short deadline (0.5 s for
writes, 3 s for prompt search). Unknown events, empty input and read errors are no-ops.

Payload fields read: `session_id`, `parent_session_id`, `prompt`, `transcript_path`, `cwd`,
`subagent_type`, `final_response`, `tool_name`, `tool_input`, `source`
(`startup`/`resume`/`clear`/`compact`). Session IDs must match `^[A-Za-z0-9_-]{1,128}$` before
they name a file.

## Events

| Event | Does |
|---|---|
| `session-start` | Prints `## Orchestrator digest`: the last 5 events of this session's stream and of `pr-babysitter`, `routing`, `queue`, plus a count of pending-ingest items. After compaction (`source=compact`) it first prints the [resume brief](#resume-brief). Marks the session and `queue` streams seen, so the next prompt's updates skip them. |
| `user-prompt-submit` | Prints the [updates block](#updates-block) (not inside queued tasks), then [prompt context](#prompt-context). |
| `stop` | Parses the transcript (redacted) and writes `sessions/<id>.yaml`. No orchd call. |
| `pre-compact` | Writes `sessions/<id>.checkpoint.yaml` with the last request and reply. |
| `session-end` | See [session end](#session-end). |
| `subagent-stop` | Appends `{kind: complete, source: subagent_type, summary}` (redacted, 200 bytes) to the parent session's event stream. |
| `post-tool-use` | After `gh pr create\|ready\|close\|merge\|reopen\|edit`, requests a PR refresh, and after `gh pr create` (also `rtk gh pr create`) registers the PR with PR Watch (below). After `Edit`/`Write`/`MultiEdit` of markdown or a file under `~/.claude/`, hands the file off to be stored again. |

## Prompt context

1. Skip if disabled or the prompt is shorter than `min_prompt_chars`.
2. **Question or statement.** A prompt is a question if it ends in `?`, starts with
   who/what/when/where/why/how/which/can/could/did/do/does/is/are/should/would/will, or contains
   `find `, `show me`, `search for`, `look up`, `remind me`. Questions use `min_score` (0.25);
   others the stricter `min_score_statement` (0.45), since they say less about what they need.
3. **Project first.** Resolve `cwd` to its main checkout (`git rev-parse --git-common-dir`, so
   worktrees map to their repo) and name it from `origin` (else the folder). Search with tags
   `repo:<name>` and `checkout:<path>`, then top up unscoped. Git runs with a 0.5 s timeout and
   without the location overrides `GIT_DIR`, `GIT_WORK_TREE`, `GIT_COMMON_DIR`, `GIT_INDEX_FILE`,
   `GIT_NAMESPACE`, `GIT_CEILING_DIRECTORIES` and `GIT_DISCOVERY_ACROSS_FILESYSTEM`.
4. **Gate.** Keep a hit if its cosine ≥ floor, or if it contains a code identifier from the
   prompt as a whole token and its cosine ≥ floor/2 (at most one such hit per prompt). Keyword
   matches alone never pass. Skip hits already injected this session.
5. **Print** `## Auto-injected RAG context` with one `### [i] <uri> (<type>, score=…)` section per
   hit, stopping before `max_chars`. Nothing is printed if nothing passes.

Per-session state `sessions/<id>.hook.json` (`{"injected": [...], "seen": {stream: count}}`) holds
injected-hit keys (first 12 bytes of SHA-256 of `uri\0text`, newest 500) and event cursors. It is
written atomically and deleted at session end.

## Updates block

Each prompt shows, once, events added since the last prompt to this session's stream and the
`queue` stream: `## Orchestrator updates` with `- [kind] source: summary` lines (redacted, 300
runes, control and format characters replaced by spaces), at most 10 with an "N earlier" note. A stream seen for the
first time is baselined silently. Task-completion events name the task and status only.

## Resume brief

After compaction, `## Resume brief (context was just compacted)` lists working directory, branch,
uncommitted files (up to 10), last three commits, and the last request and reply from the
pre-compact checkpoint, restoring live state the harness summary cannot see.

## Session end

1. Parse the transcript (redacted), write `sessions/<id>.yaml`, delete checkpoint and hook state.
2. Tag with `checkout:<main checkout>` only; `repo:` is not used for sessions because a repo's own
   `origin` could claim another project's name.
3. Write a hand-off for the transcript, marked to also store the record and queue a summary.
4. Store the record directly (`Ingest`, 0.5 s) and request a summary (`SummarizeSession`, 0.5 s).
   Each that succeeds is cleared from the hand-off; orchd does whichever did not.

The hook takes about 0.5 s on a 29 MB transcript; embedding is left to orchd, because harnesses
cancel exit hooks after a short limit. The install gives `session-end` an explicit 15 s timeout,
which also raises Claude Code's shared 1.5 s SessionEnd budget; per-event timeouts are listed in
[install.md](install.md#claude-code).

### Transcript format

Claude Code JSONL: one object per line, `{type, message: {role, content}, timestamp}`. `content`
is a string or an array of blocks; only `text` blocks count. Empty and malformed lines are skipped;
a bad timestamp leaves a turn undated. Stored text is
`[2026-10-21T14:02Z] role: text` per turn; marker-shaped strings inside a message are rewritten
`[..]` → `(..)` so quoted text cannot date a chunk ([memory.md](memory.md#timelines)).

### Session record

YAML: `session_id`, `cwd`, `started_at`, `ended_at` (RFC 3339 UTC), `summary`, `transcript_path`,
`messages`, `last_request`, `last_reply`. The initial summary is the assistant's text, whitespace
collapsed, cut at 280 bytes. Stored as `session://<id>/record` with text
`Session <start> to <end> UTC\n\n<summary>`; the transcript as `session://<id>/transcript`.

## Hand-offs

Files under `<data>/handoff/` (directory 0700), each written to a unique temp file and renamed:

| Path | Content |
|---|---|
| `sessions/<id>.json` | `{session_id, transcript_path, tags, record, summarize, attempts}` |
| `files/<hex(sha256(path))[:32]>.json` | `{path, attempts}` |
| `pr-sweep` | empty request file (opened `O_NOFOLLOW`) |
| `failed/{sessions,files}/` | entries that ran out of attempts |

orchd's `handoff` unit, every 10 s, takes entries older than 5 s (so the hook has finished
rewriting them), at most 20 of each kind per pass:

- **Session:** transcript must be an absolute, clean `.jsonl` regular file. Parse redacted;
  store the record if asked (re-read from disk under the session lock, dating it from the
  transcript if undated); store the transcript with per-turn dating, bounded by the session's
  span; queue the summary if asked.
- **File:** classify the clean absolute path (under `~/.claude/` → `claude_config`,
  `claude://<rel>`, checked first; other markdown → `repo_doc`, `file://<path>`), resolve symlinks and classify again, open `O_NOFOLLOW`,
  require a regular file ≤ 4 MiB, redact, and skip it if more than 25 redactions (a keystore).
- **Failure:** retry after 30 s × 2^(attempts−1); after 8 attempts move to `failed/`. orchd never
  deletes or rewrites an entry modified since it read it.
- Entries whose name does not match their content are deleted; temp files older than an hour are
  removed.

Record writes for one session are serialized between the hand-off and the summarizer by 64
striped mutexes (FNV-32a of the ID).

## Summaries

The `session-summarizer` unit drains a queue (32; full means dropped) and runs, in a fresh empty
directory and its own process group:

```
claude -p --safe-mode --no-session-persistence --tools "" --system-prompt <fixed prompt> \
  --model <sessions.summarize.model> --output-format json
```

Input on stdin is the redacted transcript as `[role] text` blocks; beyond 56 KiB it keeps the first
8 KiB and last 48 KiB. The result is redacted, written to the record on disk, then stored, under the
session lock. Vector-store credentials are removed from the child's environment. Any failure keeps
the excerpt.

## Events

`<data>/events/<stream>.jsonl`, appended one object per line:
`{"kind", "source", "task_id"?, "summary", "timestamp"}` with kind `complete`, `blocked`, `failed`
or `info`. Streams: one per session, plus `queue`, `routing`, `pr-babysitter`.

## PR Watch registration

After a Bash `gh pr create`, `post-tool-use` takes the last
`https://github.com/<owner>/<repo>/pull/<n>` URL from `tool_response.stdout`, then `stderr`
(where gh reports "a pull request ... already exists: <url>"), or from `tool_response` when it
is a string, and appends `{"url", "added_at"}` (RFC 3339 UTC) to `~/.pr-watch.json`, the file
the PR Watch menu bar app reads.

- Only an existing file is changed; without one the user is not using PR Watch.
- A URL already present is skipped; entries and other top-level keys are kept as they are and
  never removed.
- A file whose `prs` is not an array, or that does not parse, is left untouched.
- The write takes the lock directory `~/.pr-watch.json.lock` (shared with other writers; one
  older than a minute is moved aside atomically and removed, so two writers cannot both claim it,
  and the hook gives up after 3 s), writes a temp file in the target's directory with the
  original mode, and renames it over the target. A symlinked registry is written through to its
  target and keeps its link. Anything but a regular file under 4 MiB is left alone.
- There is no `gh` fallback when the output holds no URL, so the hook stays local and fast.
