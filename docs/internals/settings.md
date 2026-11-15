# Managed settings

orch keeps items it finds in other tools' configuration files as desired state in `orch.db`, and
writes that state back to the files. The providers cover Claude Code
(`~/.claude/settings.json`), cc-safety-net's custom rules, and orch's own instruction modules
(the managed block of each harness entry file). The app's **Settings → Claude** section, the
`orch settings` command and the settings RPCs are views over this one store.

Code: `internal/settings` (store, reconciler, provider interface),
`internal/settings/claudecode`, `internal/settings/safetynetrules`,
`internal/settings/instructions`, `internal/daemon/settings.go`.

## Per machine

Each machine's `orch.db` holds that machine's choices. Nothing syncs them and nothing is shared:
a hook turned off on one laptop stays on on another. There are no machine IDs or profiles.

## Items

An item is keyed by **provider**, **kind** and an **identity** stable within them. The row keeps
its desired state (`enabled`), its **origin**, its full payload as JSON, whether it was in its
file at the last look (`present`), when it vanished from the file outside orch (`removed_at`),
and timestamps (table `settings_items`, migration `0012`).

| Provider | Kind | One item | Identity | Off means |
|---|---|---|---|---|
| `claude-code` | `hook` | one handler of `hooks.<Event>[].hooks[]`, with its event and its group's `matcher` | orch-hook's: `<Event>/orch-hook <slug>`; any other: `<Event>/<12 hex of sha256(matcher, handler)>` | taken out of the file |
| `claude-code` | `permission` | one rule string of `permissions.allow`, `ask` or `deny` | `<list>/<rule>` | taken out of the file (every copy) |
| `claude-code` | `plugin` | one `enabledPlugins` entry (booleans only) | `<plugin>@<marketplace>` | set to `false` in the file, as `claude plugin disable` does |
| `cc-safety-net` | `rule` | one rule of a rulebook listed in `~/.cc-safety-net/rules/rule.json` | `<rulebook>/<rule>` | `"overrides": {"<rulebook>/<rule>": "off"}` in rule.json |
| `orch-instructions` | `module` | one instruction module shipped in the binary (`internal/install/modules/<name>.md`) | `<name>` | its import (Claude, Gemini) or read line (Codex) left out of every managed block |
| `orch-instructions` | `agent`, `skill` | one Claude Code subagent or skill orch ships, with all its files | `<name>` | every file orch installed for it removed from `~/.claude`, then its emptied directories |

**Instruction modules** are listed whether imported or not, with origin `orch` and their
frontmatter `default` (on unless `default: false`) until toggled. The payload carries the title,
topic, description, installed path and the full text agents read, so clients show the module
without opening its file. They are toggled, never added or removed here: a module is added by
adding a file and rebuilding.

Identical handlers (same event, matcher and fields) are one item, as a rule written twice in one
permission list is: turning it off removes every copy, and turning it on writes one back. A hook
edited outside orch (a new timeout, say) becomes a new item and the old one is marked removed;
orch-hook's identities name the subcommand instead, so a new binary path or new timeouts keep the
toggle.

**Origin:** `orch` for what `orch install` writes (an orch-hook handler whose command is exactly
`<dir>/orch-hook <slug>` with the slug install wires for that event and no matcher; a command that
merely mentions orch-hook is someone else's, so it cannot take over orch-hook's identity and a
toggle made on it; `mcp__rag__*` and `mcp__queue__*` allows, the credential-file `Read` denies,
`cc-safety-net@orch-pinned`, and the local `orch-guards` rulebook's rules); `you` for what was
added through orch; `found` for everything else seen in a file (Orch.app labels it "external").

**Status** (derived): `active` (on and in the file), `disabled` (turned off through orch),
`removed` (vanished from the file outside orch), `missing` (an enabled orch item absent from the
file; `orch install` restores it, or turning it on in orch does).

## Operations

- **Toggle.** Turning a hook or permission rule off removes it from the file and keeps its payload,
  so turning it on writes back exactly what was there: the handler object byte for byte (numbers
  as written), into the first group with the same matcher, else as a new group. A group emptied by
  a removal is dropped unless it carries a key orch does not know; an event left with no groups is
  dropped. Plugins and rules are switched in place. A plugin or rule gone from its file cannot be
  toggled.
- **Add** (hooks and permission rules only). Hooks are added as `"type": "command"` handlers.
  Validation: the event must be one Claude Code documents; the command non-empty, at most 8 KiB,
  free of control characters other than newlines and tabs; a matcher is refused on events that
  ignore it; `if` must be one permission rule and is refused outside the five tool events, where a
  hook with `if` never runs; the timeout a whole number of seconds from 1 to 86400; async `true`
  or `false`. A permission rule is a tool name (letters, digits, `_`, `-`, `*`) with an optional
  non-empty `(specifier)`, at most 1 KiB, on one line. Nothing is ever interpolated into a shell:
  the command is stored as the string Claude Code runs. orch never adds an `ask` rule of its own.
- **Remove** (hooks and permission rules with origin `you` or `found`). Takes the item out of the
  file and deletes the row. `orch` items can only be turned off, since the next install would put
  them back. Plugins and rules are never removed from their files: rulebooks are their authors'
  files. Any item already gone from its file (status `removed`), of any kind or origin, can be
  removed, which only forgets the row.

## Reconciler

`settings.Service` runs every list and change under one lock, so changes are serialized across
files and a poll never interleaves with a write.

- **Watching.** orchd's `settings-watcher` unit calls `Sync` every 2 s. A provider's files are
  read and hashed; if the hash is unchanged nothing is imported. It writes a file only to put an
  authoritative provider's files back to the stored state (below).
- **Authoritative providers: orch.db wins.** A provider implementing `settings.Authoritative`
  owns its files (or the part it manages) outright; `orch-instructions` is the only one. After
  each look, `Enforce` compares the files with the stored rows and returns what must change; the
  watcher writes it with the same backup, re-read and atomic rename as any change. An import keeps
  the stored choice of an existing row rather than the file's, so a hand edit inside the managed
  block (an import added back, a module file deleted or edited) is undone within one poll instead
  of imported. A toggle computes the files with the new state, writes them, then records the row,
  so a failed write (a file changed underneath) records nothing. A file `Enforce` must leave alone
  does not block the rest: an entry with broken markers, or a module path that is a symlink or
  other non-regular file, is skipped, the other files are written and the choice recorded, and
  the skipped files are the provider's error until fixed. Module files are never written through
  a symlink, since the watcher writes without anyone acting and a planted link would aim it at
  any file the user owns; entry files still follow links (dotfile managers). Text outside the
  managed block is never touched; an entry file without the block (a harness `orch install` never
  wired) is left alone, and module files are written only once some harness is wired.
- **Subagents and skills.** Enforced only once Claude Code is wired (its `CLAUDE.md` has the
  managed block). On: each file is written when missing, marked or byte-identical to the shipped
  copy without its marker (adopted). Off: each marked file is deleted, then the directories only
  that item uses are removed if empty; a file the user added there keeps its directory. A file
  without the marker that differs from the shipped copy is the user's: never overwritten or
  deleted, reported until moved aside. An item's file or directory that is a symlink is neither
  written nor deleted through. `Enforce` returns `settings.Changes` (`Write`, `Delete`, `Prune`).
  Deletes follow the write rules: only paths the provider read, a backup before this process's
  first change to the file, and removal only of a regular file still holding the bytes read.
  `Prune` only removes an empty directory that holds a path the provider read, and only a real
  directory: it uses `rmdir`, never `unlink`, so a symlink is never deleted, and an item with a
  symlink anywhere in it queues no directory removals at all.
  - **Pinned helpers:** an agent's helper script (`exploit-finder-sandbox.sh`) may be named by a
    pre-authorized permission rule, so whatever sits at its path runs without a prompt. It is
    never treated as the user's: on, any other content is replaced with the shipped copy; off,
    anything there is removed. `orch install` adds the sandbox's allow rule only while
    `exploit-finder` is on, and removes its own copy of the rule while it is off.
  - **Sensitive agents:** `security-audit`, `adversarial-reviewer` and `exploit-finder` are
    sensitive like the security modules: the app confirms turning one off, the CLI needs
    `--yes`, and `orch status` names it while off.
- **Sensitive modules.** A module with `sensitive: true` in its frontmatter (the agents' security
  policy) carries `"sensitive": true` in its payload. Turning one off asks for confirmation in
  the app and needs `--yes` with `orch settings disable`, and `orch status` warns with its name
  while it is off. The RPC itself does not refuse: the confirmation guards against a stray click,
  and queued tasks cannot call it at all.
- **External edits: the file wins** (every other provider). A new entry is imported as `orch` or `found`, enabled. An
  entry turned back on in the file is on again (a disabled hook re-added by hand, a plugin
  enabled with `claude plugin enable`). An entry removed from the file is marked `removed` and not
  put back, except an enabled orch item, which stays enabled and `missing` for `orch install` to
  restore. orch does not fight other tools: if one re-adds a hook you turned off, the UI shows it
  on again rather than orch removing it in a loop.
- **Writing a change.** The provider's files are re-read, and imported first if they changed,
  so the change is computed from the bytes on disk. The provider returns new bytes; any path it
  did not read is refused, so writes stay inside the files it declared. Before the first change a
  process makes to a file, the file is backed up to `<data>/backups/<stamp>/` (0600, as install's
  backups). The write goes to a temporary file beside the target and is renamed over it, so a
  reader sees the old file or the new one; a symlink is followed and kept, and the target's mode
  is kept (a new file is created 0600, since settings files can hold `env` secrets). A symlink to
  a missing file is refused rather than followed, so no directory is created where a link points.
  Just before the rename the link is resolved again and the file read again: if the link now
  points elsewhere, or the file changed since (Claude Code saving a permission, an editor),
  nothing is written and the change fails with `Aborted`, to be retried. The row is recorded,
  then the files are imported again as written. Log lines name the provider, file, origin and
  kind, and a short hash of the identity, never the identity itself: a permission rule can embed a
  secret, and orchd's log is readable by queued tasks.
- **Bounds.** Files are read up to 4 MiB; a provider with more than 5000 items imports none and
  reports why. Each import is one transaction.
- **Live effect.** Claude Code reloads `settings.json` when it changes; cc-safety-net reads
  rule.json on every command.
- **Unreadable files.** A file that does not parse is reported as the provider's error (in the
  list, the app and `orch settings list`), its items stay as last seen, and every change is
  refused with `FailedPrecondition` until it parses again.
- **Formatting.** The file is re-encoded with two-space indentation and sorted keys, as
  `orch install` writes it, without escaping `<`, `>` and `&`. Every key orch does not track is
  kept.

## `orch install` and drift

`orch install` reads the toggles from `orch.db` directly (read-only, no migration), so it works
with orchd down, and leaves out every `claude-code` item turned off through orch: a disabled
orch-hook event is left exactly as it is in the file, and a disabled orch allow or deny rule is
not added. Because the plan already leaves them out, `orch status` does not report them as drift;
its `claude-code` line says `N disabled by you` instead (both providers' disabled items). Rule
toggles live in rule.json overrides, which install's merge preserves; a disabled
`cc-safety-net@orch-pinned` plugin is left disabled by install, as before.

**Fail safe.** A missing `orch.db`, or one from before the settings table, cannot hold a toggle,
so install proceeds. Any other failure to read the toggles (a locked or corrupt store) stops
install with an error before anything is written. It never falls back to "nothing is disabled",
which would silently turn everything back on.

## RPCs and callers

| RPC | Caller | Notes |
|---|---|---|
| `ListSettings` | tasks too | syncs first; returns providers (kinds with their add-form fields, files, error, last look), items (with group, summary and payload), and a revision that changes with any item or provider state |
| `SetSettingEnabled` | operator only | unknown → `NotFound`; vanished plugin or rule → `FailedPrecondition` |
| `AddSetting` | operator only | validation → `InvalidArgument`; already listed → `AlreadyExists` |
| `RemoveSetting` | operator only | orch item or unremovable kind → `FailedPrecondition` |

The three changes are operator-only (`operatorRPC` beside each handler in `internal/daemon/settings.go`): adding a hook means
choosing a command Claude Code will run, and a permission rule widens what agents may do, so a
queued task is refused (`PermissionDenied`) however it reaches the socket.
While a spaces policy is applied (or kept), the three are Orch.app's alone: a terminal is not the operator
(D-71), and an unlocked terminal is still refused, since a toggle outlasts the unlock (D-74, D-78;
`policyOperatorOnly` in `internal/daemon/policy_operator.go`). A policy kept while spaces are
switched off counts too; with no policy ever applied, nothing changes.

## Adding a provider

A provider adapts one tool's files; nothing else changes (no migration, no RPC, no new client
model).

1. Implement `settings.Provider` in a package under `internal/settings/`:
   - `Name` (the stable key stored with each row) and `Title`.
   - `Kinds`: each kind's title, `GroupTitle`, whether it is `Addable` and `Removable`, whether
     the file records an item's off state (`StateInFile`), and the add form's `Fields` (types
     `string`, `text`, `int`, `bool`, `enum` with `Options`). Clients build forms from these.
   - `Read`: the bytes of every file it depends on, keyed by absolute path (`settings.ReadFile`
     caps size and refuses non-regular files). Only these paths can be written.
   - `Observe(files)`: the items the bytes hold, each with a stable identity, its payload, its
     on/off state and its origin. Identities must be unique within a kind: collapse identical
     entries into one item, or tell them apart with `settings.Dedupe`.
   - `Apply(files, item, enabled)`: new bytes for each file that must change, or none. Preserve
     every key it does not track, refuse a file of an unexpected shape with `settings.ErrInvalid`
     rather than overwriting it, and validate anything from the item that becomes a path or key.
   - `Validate(kind, fields)`: build a new item from an add form, or `settings.Invalidf`.
   - `Describe(kind, payload)`: the group and one-line summary clients show.
   - Optionally `Enforce` (`settings.Authoritative`), when orch owns the files outright and the
     store, not the file, must win; its `Apply` is then never called.
   Every method but `Read` is a pure function of its arguments; test them on bytes.
2. Register it in `settingsFor` in `internal/daemon/settings.go`.
3. If `orch install` writes items of that tool, give them deterministic identities install can
   compute and have install skip the disabled ones, as `internal/install/toggles.go` does for
   Claude Code, so install and drift honor the toggles.
4. In the app, a tab is a `SettingsModel(api:providers:)` over the new provider name, with
   `SettingsKindList`, `SettingRow`, `SettingDetail` and `SettingAddForm` from
   `macos/Sources/OrchApp/SettingsComponents.swift`.

## Per-space checkout files

The per-space harness settings orchd keeps in each checkout's `.claude/settings.local.json`
(spaces SP-046) are not settings items: they are not listed here, cannot be toggled one by one,
and follow the applied spaces policy, the `sandbox` enforcement switch and the operator's
[sandbox setting](daemon.md#sandbox-setting) (`orch spaces sandbox auto|on|off`, D-61) instead. The Claude Code
provider reads only `~/.claude/settings.json`, so it never sees them. Their merge, ownership
record, backups and git handling are in [install.md](install.md#per-space-harness-settings). The
ownership record (`.orch/checkouts.json`) also keeps the folders each file denies, which the tool
guard reads to refuse calls in a checkout whose own settings re-open one (D-67).

## Limits and accepted risks

- One machine, one store; no sync.
- The final check and the rename are two steps, so a writer landing in the microseconds between
  them is overwritten. The backup taken before orch's first change to a file is kept.
- The operator-only rule covers the RPCs. A same-user process with a shell (a queued task, say)
  can still edit `orch.db` or the files directly, as for the rest of orch's state; for example, a
  row marking a credential deny rule off keeps `orch install` from restoring it. The Settings → Claude section
  shows it as off and `orch status` counts it among `N disabled by you`.
- Payloads of disabled and removed items stay in `orch.db` until removed (forgotten) in the
  Settings → Claude section; a permission rule saved with a secret in it stays there too. `ListSettings` returns
  them, and queued tasks may call it.
- `orch install --home <dir>` still reads this machine's toggles from the real `orch.db`; it only
  ever leaves items out, so a test install against a temporary home may skip items turned off
  here.
- A disabled item re-added to its file by another tool shows as on again: the file wins. Not for
  instruction modules, where orch.db wins and orchd removes the import again.
- orchd's watcher rewrites the managed block of the real home's entry files, so a session started
  in the seconds between a hand edit and the next poll may load the hand-edited block once.
- A toggle writes each changed file in turn: if a later file fails, the earlier ones are already
  written and the row is not recorded, so the next poll puts them back.
- Two writers that disagree about the managed block (two orchd builds on one home, a dotfile
  manager re-applying a template) rewrite it in turn every poll; nothing backs off.
- A module path turned into a symlink between the watcher's look and its write is followed for
  that one write; the look refuses it on every poll after.
- The watcher takes over an unmarked agent or skill file byte-identical to the shipped copy
  without asking; `orch install` lists the same takeover in its preview.
- Marker detection matches the marker text anywhere in a file, so a file of the user's own that
  keeps the marker line is treated as orch's.
- A delete is backed up only before this process's first change to that file; a later edited
  copy of an off item's marked file is removed without a new backup.
- Writes and deletes follow a symlinked `~/.claude`, `agents/` or `skills/` directory (a dotfiles
  setup) into its target; only the item's own files and directories are checked for links.
- Module text ships in the binary: every machine running a build gets every module's text in
  `~/.config/orch/modules/`, on or off. Keep machine-specific secrets and absolute home paths out
  of modules (a test rejects `/Users/` and `/home/`).
- `orch install` restores a missing orch item the user removed by hand (not through orch), as it
  always has; turn it off in orch to keep it out.
