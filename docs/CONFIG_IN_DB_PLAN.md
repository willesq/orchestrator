# Plan: remove config.yaml; settings live in orch.db, edited in Orch.app

Status: phase 1 in progress (branch `feat/config-in-db`), 2026-11-08. Open decisions 1-3 proceed
with the recommended option unless the user says otherwise.

## Intent

`~/.config/orch/config.yaml` goes away. The settings it holds are stored in `orch.db`, and Orch.app's
Settings pane is the only way to change them. This is a deliberate breaking change: no file
fallback, no dual-read period, no hand-editing path.

## What exists today

- `internal/config` builds a Viper: compiled defaults (`setDefaults`) < `config.yaml` < `ORCH_<KEY>`
  environment variables. `config.Fields` (`schema.go`) describes every key: section, type, range,
  `Sensitive`, `Live`, `Hidden`.
- Every write (`Set`, `SetMany`, `Apply`) calls `WriteCanonical`, which dumps the **whole resolved
  view**, defaults included, to YAML. A file written once freezes every default it saw, so a later
  change to a default never reaches that user (`docs/TASKS.md` already warns about a frozen
  `queue.worker.enabled: false`).
- The app already edits every schema field: `GetConfig` and `SetConfig`
  (`internal/daemon/config_rpc.go`) serve `config.Fields`, and `Navigator.configSections` covers all
  12 sections. The UI is mostly done. The work is the storage layer and its callers.
- Callers of `config.Load()` outside the package:
  - orchd: `daemon.go` (startup and `configStart` snapshot), `config_rpc.go`, `migrate.go`
    (`recordSwitch` and the backup drop write `rag.qdrant.collection` and `backup_collection`).
  - orch-hook: `hook.go` `loadAutoRAG`, on every prompt. These are the `Live` keys.
  - CLI: `commands.go` (`orch config get/set/list`, plus one more read), `status_cmd.go`,
    `check.go`, `models_cmd.go`, `routing_cmd.go`, `rag_dedupe_cmd.go`, `rag_migrate_cmd.go`
    (writes), `migrate_cmd.go` (writes `rag.qdrant.storage`), `cutover_cmd.go` (writes storage,
    collection and port).
- `orch.db` already holds user state written by both CLI and orchd (routing, model registry,
  `settings_items` for other tools' config), opened through `internal/db` with goose migrations
  (latest is `0021`).
- orchd opens its socket only after config load, `resolveQdrantAPIKey`, `ensureModel` and
  `buildRAG` (`daemon.go:556-668`). A bad `embedder.model_dir`, storage path or port today makes orchd
  exit before any client can reach it. Today you recover by editing the YAML file. That option goes
  away.

## Design

### Storage: overrides only

New goose migration `0022_config_values.sql`:

```sql
CREATE TABLE config_values (
  key        TEXT PRIMARY KEY,   -- dotted key from config.Fields
  value      TEXT NOT NULL,      -- JSON, the shape ParseJSON accepts
  updated_at TEXT NOT NULL,      -- RFC 3339 UTC
  updated_by TEXT NOT NULL       -- 'app', 'orchd:migrate', 'cli:cutover', 'import'
);
CREATE TABLE config_import (
  path        TEXT NOT NULL,
  sha256      TEXT NOT NULL,
  imported_at TEXT NOT NULL
);
```

- Only values that differ from the compiled default are stored. Deleting a row resets the key to
  its default. Default changes in new releases reach every user, which fixes the frozen-default
  problem.
- One row per schema key. Map keys (`routing.kinds`, `rag.search.source_weights`,
  `queue.worker.allowed_tools`) are stored whole as a JSON object, matching how `SetConfig` already
  takes them. Keys outside `config.Fields` are refused. Today `orch config set` writes them as
  given; that path goes away.
- `updated_by` is for logs and the app ("changed by a migration"), not for authorization.

### `internal/config` API

- Replace `Load() (*viper.Viper, error)` with a function that reads `config_values` from an
  `*sql.DB`, layers the rows over the defaults, and returns a resolved view. `Unmarshal`, `Entries`,
  `Snapshot` and `PendingRestart` keep working on that view.
- `Apply(db, values, confirmSensitive)` validates as today, then upserts the changed keys in one
  transaction. It deletes a key's row when the new value equals the default. A new
  `Reset(db, keys)` deletes rows.
- Drop `SetConfigFile`, `ReadInConfig`, `AutomaticEnv`, `WriteCanonical`, `Set`, `SetMany`, and
  the `EnvOverridden`/`ErrEnvOverride` machinery.
- Phase 1 can keep Viper as an in-memory merger (`SetDefault` + `Set` from rows) so `Unmarshal` and
  the typed `Settings` stay unchanged. Removing the Viper dependency is an optional follow-up.
- `normalizeQueue` clamping stays on read. `Apply` still refuses out-of-range values.

### Who reads and writes

| Process | How it gets config | Writes |
|---|---|---|
| orchd | `db.Open()` (it owns migrations), at startup and per `GetConfig` | `SetConfig`/`ResetConfig` from the app; `recordSwitch`/backup drop |
| orch-hook | `db.OpenReader()` per prompt, for `hooks.auto_rag.*` only; on any error, defaults (same as a missing file today) | never |
| CLI read commands (`status`, `check`, `models`, `routing`, `rag dedupe`) | `db.OpenReader()` | never |
| CLI maintenance (`migrate`, `cutover`, `rag migrate-hybrid`) | `db.Open()` | write their state keys through `config.Apply` with `updated_by='cli:<cmd>'`, in one transaction |

WAL readers don't block the writer, and a per-prompt read-only open is a few milliseconds. orch-hook
must not migrate a store it doesn't own: `OpenReader` already guarantees that.

### Recovery when a setting breaks startup

Without a file, a bad value must never lock the user out. Two parts:

1. **orchd serves before it validates the environment.** Move `net.Listen` and the config,
   status and log RPCs ahead of `ensureModel`, `resolveQdrantAPIKey` and `buildRAG`. If a
   config-dependent step fails, orchd stays up in a degraded state, reports the failing key and
   error through `GetStatus`/`GetConfig`, and lets the app change or reset the key and `Restart`.
   Subsystems that need RAG return `Unavailable` until then.
2. **A last-resort reset outside the app**, for when orchd can't run at all, such as a crash before
   listen. Recommended: keep `orch config reset <key>|--all`, which writes `orch.db` directly and
   refuses inside a queued task (`operatorOnlyMsg`). Open decision 1 covers this.

### One-time import of an existing config.yaml

Runs at orchd startup, before config is read, only if `config_import` is empty and the file exists:

1. Parse the YAML with the old loader (kept in `internal/config/legacy.go` until a later release).
2. For each schema key whose value differs from the current default and passes `checkValue`, stage
   a row with `updated_by='import'`. Log invalid values and unknown keys by **name only**, as
   `SetConfig` does, since values can be paths or rules.
3. Import values exactly as they are, including defaults an older `WriteCanonical` froze (for
   example `queue.worker.enabled: false`). Skipping those would silently change what runs, such as
   turning on a worker that runs agents unattended. Each imported key is logged by name, and the
   app's per-row "Reset to default" clears any the user doesn't want.
4. In one transaction, insert the rows and a `config_import` row (path, sha256). That row is the
   only marker, so the import never runs twice.
5. Leave the file where it is. From then on it's ignored, and the app offers to remove it (see
   below).

### RPC and app

- `ConfigState.path` (field 2): delete and `reserved 2;`. `ConfigField.env_override`: delete and
  reserve. Run `make proto` and regenerate the Swift client.
- Add `ConfigField.overridden` (a row exists) and `ResetConfig(keys)`. `ResetConfig` is
  operator-only, refused for queued sessions like `SetConfig`, and `Sensitive` keys need
  `confirm_sensitive`.
- Add `ConfigState.legacy_file`: the path of `<config>/config.yaml` when it still exists, else
  empty. Add `RemoveLegacyConfig`, which is operator-only, deletes only that fixed path (never a
  path from the request), and refuses unless the import has run (a `config_import` row exists), so
  nothing unimported is lost.
- Add the degraded-start report to `ConfigState` (`startup_error`, `failing_key`), or to
  `GetStatus`.
- App (`SettingsView.swift`, `Config.swift`, `DaemonClient+Settings.swift`):
  - Remove the "Configuration file" section under Settings → App.
  - Add an **Overview** section at the top of Settings. When `legacy_file` is set it shows a passive
    row, not a warning: "Legacy config.yaml found. orch no longer reads it; its settings were
    imported." with the path and a **Remove** button that calls `RemoveLegacyConfig`. The row
    disappears once the file is gone.
  - Add "Reset to default" per row when `overridden` is true.
  - Show the degraded-start banner with a link to the failing row.
  - Drop config.yaml wording from doc comments.

### CLI surface

- Remove `orch config set` and `orch config list`'s YAML dump.
- Decision 1 settles whether `get`/`list` stay as read-only diagnostics and whether `reset` stays as
  the recovery hatch.
- `agentrun.operatorTools` drops `Bash(orch config set:*)`, or swaps in `orch config reset:*` if
  reset stays.
- `mcpserver.go:304` changes its message to point at Orch.app → Settings → Tasks.

### Environment overrides

`ORCH_<KEY>` setting overrides go away. Location variables stay, because they say where `orch.db`
is: `ORCH_DATA_DIR`, `ORCH_CONFIG_DIR` (still holds the installed AGENTS.md and modules),
`ORCH_SOCKET`, `ORCH_LIVE_SOCKET`, and `ORCH_TASK_ID`. Tests that set
`ORCH_RAG_QDRANT_MODE`, `ORCH_PR_BABYSITTER_ENABLED` and others (`internal/cli/cli_test.go`,
`internal/daemon/run_test.go`) switch to a test helper that seeds `config_values` in a temporary
`ORCH_DATA_DIR`.

## Phases (one PR each)

Storage must switch in a single PR. If it doesn't, the app writes to the DB while orch-hook still
reads the file.

1. **Store and API (no behavior change).** Migration `0022`, the read, `Apply` and `Reset` against
   `*sql.DB`, the one-time import, and the test seeding helper. Unit tests cover the overrides-only
   invariant (setting a default deletes its row), atomic multi-key apply, an import that runs once
   and leaves the file in place, and unknown and invalid keys. Not wired in yet.
2. **Cutover.** Every caller in the table switches to the DB, orchd runs the import at startup,
   `paths.ConfigFile`, `WriteCanonical` and env overrides are removed, and env-based tests are
   rewritten. Update `docs/internals/config.md` (rewrite), `daemon.md` (data files table, the
   "can edit config.yaml" limit, the `GetConfig` row) and `docs/ARCHITECTURE.md:76`.
3. **Degraded start.** orchd listens first and reports config-caused startup failures. Tests:
   a bad `embedder.model_dir` and a taken `http_port` both leave orchd reachable, and resetting the
   key followed by `Restart` recovers.
4. **RPC, app and CLI surface.** Proto changes, `ResetConfig`, `legacy_file` and
   `RemoveLegacyConfig` with the Settings Overview row, the app's reset and banner, removal
   of the config-file row, the `orch config` changes, `operatorTools`, and the MCP message. Update
   `docs/internals/app.md` (lines 12, 382, 411) and `docs/TASKS.md` (its `orch config set` steps
   become app steps).
5. **Installed assets.** Update the skills that name `config.yaml` or `orch config get`:
   `orch-rag-ingest`, `orch-task-status` and `orch-dispatch` under
   `internal/install/assets/claude/skills/`. Run install drift checks with a temporary `--home`.
6. *(Optional, later)* Drop Viper and `legacy.go` once the import has shipped in a release or two.

Phases 3 and 4 can run in parallel after 2. Phase 3 should ship in the same release as phase 2,
or before it, so no released build combines "no file" with "orchd dies before listening".

## Security notes

- This doesn't make config safer from a queued task: `orch.db` is user-writable, like
  `config.yaml` was (`daemon.md` "Limits"). The `daemon.md` text should say so plainly rather than
  imply an improvement.
- Logs keep naming keys only, never values: import, `SetConfig`, `ResetConfig` and CLI writes.
- `ResetConfig` on a `Sensitive` key (permission mode, allowed tools, verify repos, storage) can
  widen or move what agents run, so it needs the same confirm and operator-only gate as `SetConfig`.

## Open decisions

1. **CLI.** Recommended: remove `set`, keep read-only `get`/`list` (agents and skills use
   `orch config get queue.worker.enabled` for diagnosis), and keep `reset` as the recovery hatch.
   The alternative is to remove `orch config` entirely and accept that a crash before listen
   needs `sqlite3` to fix.
2. **Import vs. clean break.** Recommended: the one-time import above, because the file holds real
   choices (`verify.repos`, `allowed_tools`, routing). The alternative ignores the file and
   starts from defaults.
3. **Operational state in the same table.** `rag.qdrant.collection`, `backup_collection` and
   `storage` are written by migrations, not people. Recommended: keep them in `config_values`
   (minimal change) and mark them with `updated_by`. The alternative moves them to a
   `store_state` table and hides them from Settings.
4. **Spaces.** The spaces plan (`docs/spaces/spaces-features.json`) may want per-space settings
   later. `config_values` keyed by `key` alone doesn't block adding a `space` column. Nothing to
   decide now unless spaces needs it soon.

## Follow-ups

- 2026-11-08, from the phase 1 security review (Low): `ApplyStore` and `ImportLegacy` open
  deferred transactions that read before writing, so a write racing another connection's commit
  can fail with `SQLITE_BUSY_SNAPSHOT` (an error, never a lost update). Take the write lock up
  front (`BEGIN IMMEDIATE`) or retry, when phase 2 wires orchd and the CLI to the store.
- At cutover, `ORCH_<KEY>` pins stop applying. Note that in the phase 2 release notes, since
  someone may rely on one to force a safe value.
