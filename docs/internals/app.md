# macOS app

`Orch.app` is a menu bar app for orchd. It is a client like `orch`: it dials the daemon's Unix
socket over gRPC and never opens `orch.db`, `workflows.db` or Qdrant itself, so every rule the
daemon enforces (redaction, delete confirmation, migration locks) applies to it too. The source
lives in [`macos/`](../../macos/), a Swift package.

## Layout

| Target | Contents |
|---|---|
| `OrchKit` | Everything testable: the generated orchd client (`Generated/`), the connection (`DaemonClient`), health classification (`Health`), polling (`DaemonMonitor`, `PRMonitor`), PRs, attention and the tray icon (`PullRequest`), memory search and sources (`Memory`, `MemoryModels`), tasks and dispatch (`Tasks`, `TaskModels`), GitHub scopes and activity (`GitHub`, `GitHubModel`), system metrics, logs and the status report (`System`, `SystemModel`), the login item (`LoginItem`), daemon control (`DaemonControl`), and socket path resolution (`OrchPaths`) |
| `OrchApp` | The SwiftUI `MenuBarExtra`, the main window (`MainWindow`, `DashboardView`, `SystemView`, `LogsView`, `MemoryView`, `TasksView`, `GitHubView`), and notifications (`Notifier`) |
| `OrchKitTests` | Swift Testing suites, including a round trip against a fake orchd on a temporary socket |

## Connection

- **Socket path:** the same as `internal/paths`: `ORCH_SOCKET`, else `ORCH_DATA_DIR/orchd.sock`,
  else `$XDG_DATA_HOME/orch/orchd.sock`, else `~/.local/share/orch/orchd.sock`.
- **Connection:** the app keeps one HTTP/2 connection (grpc-swift 2, POSIX transport, plaintext).
  The socket's 0600 mode is the trust boundary, as for every other client. The transport
  reconnects when orchd restarts and recreates the socket.
- **Deadlines:** each call has a 3 s deadline, matching `orch status`.
- **Codegen:** `make proto` generates both the Go and the Swift client code. The Swift protoc
  plugins are built from the versions pinned in `macos/Package.resolved`, so generated code
  always matches the runtime the app links. CI fails if the committed Swift no longer matches
  `orchd.proto`.

## Health

`DaemonMonitor` calls `Status` every 10 s. The tray icon shows one of four states:

| State | When |
|---|---|
| unknown | before the first check |
| healthy | reachable, and no unit is `failed` or carries an error |
| degraded | reachable, with at least one `failed` unit or one recording an error; `stopped` does not count, because stopping a unit is deliberate |
| down | the call failed (no socket, refused, or past the deadline) |

## Needs you

`PRMonitor` calls `ListPRs` every 60 s. The babysitter refreshes its cache every 10 minutes, so
polling the cache costs nothing. The menu shows up to eight PRs that need the user, most urgent
first, each with its reason (see the PR babysitter in [tasks.md](tasks.md)). Clicking one opens it.

- **Opening PRs:** the app only opens URLs of exactly the form
  `https://github.com/<owner>/<repo>/pull/<n>`: no port, credentials, query or fragment. It hands
  them to the system to open, so nothing else should get through.
- **Badge:** the icon becomes a bell with a count while orchd is healthy and something needs the
  user. A degraded daemon keeps its warning icon with the count. A down or unknown daemon hides
  the count, because the list would be stale.
- **Notifications:** one per PR that newly needs the user, or now needs them for a different
  reason. Nothing is posted for what is already waiting at launch. More than three at once become a
  single "N PRs need you" summary. Clicking a PR's notification opens it. A failed poll keeps the
  last good list, so a briefly unreadable cache cannot make every PR look new. Notifications need the app bundle, so a bare `swift run` binary skips them.

## Launching

Orch.app lives in the menu bar (`LSUIElement`). Without help, opening it would show nothing, and on
a crowded menu bar its icon can sit behind the notch. `AppDelegate` handles this:

- **When the user opens it** (Finder, Spotlight, `open`), the main window appears. A launch at login
  (the launch Apple event's `keyAELaunchedAsLogInItem`) shows only the tray icon.
- **Opening it again** while it runs brings the window back.
- **One copy only:** the earliest-launched live copy stays, with the lowest PID breaking a tie
  (`LaunchPolicy.shouldHandOff`), so copies started together leave exactly one. Any later copy posts
  the `com.willesq.orch.show-main-window` distributed notification, which makes the running copy
  show its window, and quits without opening a connection to orchd.
- **The notification:** it carries nothing, so whoever posts it can only bring the window forward.
  The running copy honours at most one request a second.
- **Plain AppKit window:** the main window is an `NSWindow` hosting the SwiftUI `MainWindow`. It
  remembers its frame, and the selected pane is kept in user defaults. The hosting controller's
  `sizingOptions` is empty, so the window keeps the size the user gives it instead of following each
  pane's ideal size. Every pane has toolbar content, so the toolbar band never comes and goes, which
  would otherwise change the window's height on a pane switch. A SwiftUI `Window` scene can
  only be opened from a live view, and the menu bar label is rendered as an image, so it cannot do
  that for AppKit.
- **Dev builds:** `make app` writes to `build/app.noindex/`. The `.noindex` suffix keeps Spotlight,
  and so "open Orch", from launching a dev build in place of the installed app.

## Main window

"Open Orchestrator…" in the menu, or opening the app, shows a window with a sidebar.

- **Dashboard:** orchd's health, every subsystem with its detail or last error and restart count,
  a **Restart** button for each unit, **Restart all** (the `Restart` RPC, one at a time), and the
  Needs you list.
- **System:** an Overview/Logs switch in the toolbar. Both poll only while the tab is shown.
  - **Overview:**
    - **Process tiles:** CPU and memory for orchd and qdrant (`GetSystemMetrics` every 2 s), each
      with a two-minute sparkline. A process that stops or comes back under a new PID starts a
      new line.
    - **Runtime:** orchd's goroutines and Go heap.
    - **Subsystems:** each unit's heartbeat age, ticking every second, with **Restart** for each.
    - **Storage and tasks:** on-disk store sizes and task counts by status.
    - **Status report:** the full `orch status --json` report every 15 s. The app runs `orch`
      from an argument array, as for daemon control, so the report covers the checks that work
      with orchd down.
  - **Logs:** `TailLogs` every second.
    - **Buffer:** keeps the latest 5000 lines from every source.
    - **Controls:** a source picker shows one source or all of them. A text filter, **Pause**
      (resuming catches up) and **Clear** sit beside it.
    - **Gaps:** if lines rolled out of orchd's memory before the app read them, a note says how
      many.
    - **Restarts:** when orchd restarts, its numbering starts over, so the view starts over too.
    - **Text:** log text is shown verbatim and never rendered.
- **Memory:** a Search/Sources switch in the toolbar.
- **Memory → Search:** the `Search` RPC with mode (default, hybrid, meaning only), a source-type
  filter and a result count of 1–100. Each hit shows its score, source, type and date.
  - Indexed text is untrusted, so it is shown verbatim and never rendered as Markdown.
  - A newer search cancels an older one, and a cancelled search that finishes late cannot replace
    newer results.
  - Searches get a 15 s deadline instead of 3 s, because the first search after a restart loads
    the embedding model.
- **Memory → Sources:** `ListSources`, up to 200 at a time with totals, filtered by type.
- **Tasks:** `ListTasks`, up to 100 at a time, filtered by status.
  - **Detail:** the selected task's model, run models, duration, turns, cost, verification and PR
    outcome, plus its body and result, shown verbatim.
  - **PR links:** opened only through the same `github.com/<owner>/<repo>/pull/<n>` check.
  - **Rating:** a finished task can be rated good, needed rework or bad (`RateTask`), which feeds
    model routing.
- **GitHub:** what orch monitors on GitHub, and what it found.
  - **Watching:** your account ("You") and each org you belong to ("Org"), each with an on/off
    switch. An enabled scope also shows:
    - a picker between "Involving me" and "Everything open";
    - a summary of what it covers, and **Repos…**, a sheet to choose all repos, only some, or all
      but some, from the repos you can see.

    Changes go to `SetGitHubScope`. orchd then sweeps within about a minute, and the activity list
    reloads every 30 s while the tab is open.
  - **Your teams:** from `GetGitHubIdentity`.
  - **Activity:** `ListGitHubItems`, filtered by All, PRs or Issues in the toolbar. Items that need
    you come first, then the newest. Each row shows its title, `repo#number`, author, age, and its
    reasons as chips. Clicking opens the item, but only exact
    `https://github.com/<owner>/<repo>/pull|issues/<n>` URLs. Titles and authors are other people's
    text, shown verbatim.
  - **Refresh** asks GitHub for your orgs and teams again.
- **Dispatch:** a sheet that sends `Dispatch` with an agent, an optional task kind (showing what
  `RecommendModel` would route it to), an optional pinned model, an optional working directory
  (chosen with a folder picker) and the task body.
  - **Validation:** it checks orchd's own rules before sending: the agent name pattern, a non-empty
    body up to 64 KiB, and an absolute `cwd`. orchd still validates on its side.
  - **Sending:** ⌘↩, not plain Return, so pressing Return in a text field never starts an agent.
  - **After sending:** the form clears the body, so a second click cannot queue the task twice.
    It shows the route orchd actually chose, and warns when no worker is enabled, because the
    task then waits.
  - **Lost answers:** dispatch gets a 30 s deadline, because routing may build a live proposal
    first. If the answer is lost (deadline, dropped connection), the task may already be queued,
    so the form says so, keeps the body, and refreshes the task list instead of inviting a blind
    retry.
  - **Late results:** a send that finishes after the form closes is dropped, so it can't bring
    back a stale result.
  - **Task kinds:** the form's list copies `routing.Kinds`, and a Go test fails if they drift.

`SearchModel`, `SourcesModel`, `RestartModel`, `TasksModel` and `DispatchModel` in OrchKit hold this
logic, behind small `MemoryAPI`, `ControlAPI` and `TasksAPI` protocols so views and tests depend only
on what they use.

## Open at login

`LoginItem` wraps `SMAppService.mainApp`, and the menu's "Open at login" checkbox turns it on or
off. If macOS registers the app but wants the user's approval, the menu links to System Settings →
Login Items. The state is re-read every time the menu opens, because it can also change in System
Settings. A binary running outside an app bundle cannot register, so the checkbox is disabled there.

## Daemon control

**Start** and **Restart** run `orch daemon start|restart`, which already chooses between brew
services and launchd. An app launched from Finder gets launchd's minimal `PATH`, so it looks for
`orch` in `/opt/homebrew/bin`, `/usr/local/bin`, `~/.local/bin` and `~/go/bin`. The command runs
from an argument array, never through a shell. `orch` itself adds the same Homebrew and `~/.local/bin` directories to its
`PATH` at startup, as orchd does, so the `gh` and `brew` it runs are found even with launchd's
minimal `PATH`.

## Brand

The logo is two SVG files in `macos/Resources/Brand/`:

- **`icon.svg`:** the app icon. Three rings, one per coding agent (Claude Code, Codex, Gemini),
  overlap on the memory they share. The art fills Apple's 824 pt icon grid inside a 1024 canvas.
- **`glyph.svg`:** the 18 pt menu bar glyph. The rings sit further apart than in the icon so they
  stay distinct at that size.

`build-app.sh` renders `icon.svg` with AppKit's own SVG support (`scripts/render-icon.swift`) into
every size `iconutil` needs. It packs them into `Contents/Resources/AppIcon.icns` (named by
`CFBundleIconFile`) and copies the glyph in as `MenuBarGlyph.svg`. No extra tools are needed.

Where the logo appears:

- **Menu bar:** the glyph, as a template image macOS tints, while orchd is healthy, with the
  needs-you count beside it.
- **Tray menu and Dashboard headers:** the app icon while orchd is healthy (`HealthMark`).
- **About Orch:** the menu opens the standard About panel, which takes the icon and version from
  the bundle. A menu-bar-only app has no app menu to reach it from.
- **Finder, Login Items and notifications:** macOS shows the bundle's icon.

Degraded, down and unknown replace the logo with their warning symbol in its color, because those
states must stand out. The README uses `icon.svg` directly. To change the logo, edit the SVGs and
rebuild.

## Build, sign, release

- **`make app`:** assembles `build/app.noindex/Orch.app`, ad-hoc signed by default. Set
  `SIGN_IDENTITY` to a Developer ID Application identity to sign with the hardened runtime and a
  secure timestamp, which notarization requires.
- **`make app-install`:** builds the app and installs it as `/Applications/Orch.app`, where Finder,
  Launchpad and the cask put apps. It uses `~/Applications` when `/Applications` is not writable,
  and `INSTALL_DIR` overrides both.
  - It first asks a running copy to quit, then replaces the bundle and launches the new one;
    `NO_LAUNCH=1` skips the launch.
  - It removes a copy an earlier install left in the other folder, but only one whose bundle id is
    `com.willesq.orch`, so Spotlight and Login Items see a single Orch.
  - This is the way to run the app from source until the cask ships.
- **Bundle settings:** `LSUIElement` keeps the app out of the Dock, and `LSMinimumSystemVersion`
  is 15.2.
- **Tests:** `make app-test` runs the tests, and `make app-cover` applies the 80%-per-file gate
  to `OrchKit` (generated code excluded).
- **CI:** the `app` job in `ci.yml` checks codegen drift, runs the coverage gate, assembles the
  app, and uploads it as the `Orch-app-<sha>` workflow artifact (kept 14 days). Every push therefore
  has an installable, ad-hoc signed build.
- **Release:** the `app` job in `release.yml` runs after the binaries are released and always
  attaches `Orch_<version>_darwin_arm64.zip` to the release. The build happens before any signing
  material is on the runner, and only versions pinned in `Package.resolved` are used.
  - **With the Apple secrets:** it imports the Developer ID certificate into a throwaway keychain,
    re-signs, notarizes with an App Store Connect API key and staples. It then dispatches the tap's
    `bump-cask.yml` with the version and sha256 (the cask URL carries the version).
  - **Without them:** the zip is ad-hoc signed, and the cask is bumped only when the repository
    variable `ORCH_APP_CASK_ADHOC` is `true`, because Gatekeeper blocks an ad-hoc app that Homebrew
    downloaded until the user allows it.
- **Installing:** the `orch-app` cask in `willesq/tap` depends on the `orch` formula, which
  stays the owner of the binaries and of `brew services`.
- **Runners:** the Swift dependencies (swift-crypto, swift-log, swift-service-lifecycle) need
  Swift 6.2, so the app jobs run on `macos-26` and select its newest Xcode 26.
