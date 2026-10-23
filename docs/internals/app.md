# macOS app

`Orch.app` is a menu bar app for orchd. It is a client like `orch`: it dials the daemon's Unix
socket over gRPC and never opens `orch.db`, `workflows.db` or Qdrant itself, so every rule the
daemon enforces (redaction, delete confirmation, migration locks) applies to it too. The source
lives in [`macos/`](../../macos/), a Swift package.

## Layout

| Target | Contents |
|---|---|
| `OrchKit` | Everything testable: the generated orchd client (`Generated/`), the connection (`DaemonClient`), health classification (`Health`), polling (`DaemonMonitor`, `PRMonitor`), PRs, attention and the tray icon (`PullRequest`), memory search and sources (`Memory`, `MemoryModels`), tasks and dispatch (`Tasks`, `TaskModels`), the login item (`LoginItem`), daemon control (`DaemonControl`), and socket path resolution (`OrchPaths`) |
| `OrchApp` | The SwiftUI `MenuBarExtra`, the main window (`MainWindow`, `DashboardView`, `MemoryView`, `TasksView`), and notifications (`Notifier`) |
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

## Main window

"Open Orchestrator…" in the menu opens a window with a sidebar. The window never opens by itself at
launch.

- **Dashboard:** orchd's health, every subsystem with its detail or last error and restart count,
  a **Restart** button for each unit, **Restart all** (the `Restart` RPC, one at a time), and the
  Needs you list.
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
from an argument array, never through a shell.

## Build, sign, release

- **`make app`:** assembles `build/app/Orch.app`, ad-hoc signed by default. Set
  `SIGN_IDENTITY` to a Developer ID Application identity to sign with the hardened runtime and a
  secure timestamp, which notarization requires.
- **`make app-install`:** builds the app and installs it as `~/Applications/Orch.app` (or
  `$INSTALL_DIR/Orch.app`). It first asks a running copy to quit, then replaces the bundle and
  launches the new one; `NO_LAUNCH=1` skips the launch. This is the way to run the app from source
  until the cask ships.
- **Bundle settings:** `LSUIElement` keeps the app out of the Dock, and `LSMinimumSystemVersion`
  is 15.2.
- **Tests:** `make app-test` runs the tests, and `make app-cover` applies the 80%-per-file gate
  to `OrchKit` (generated code excluded).
- **CI:** the `app` job in `ci.yml` checks codegen drift, runs the coverage gate and assembles the
  app.
- **Release:** the `app` job in `release.yml` runs after the binaries are released. It builds
  the app before any signing material is on the runner, then imports the Developer ID
  certificate into a throwaway keychain, re-signs, notarizes with an App Store Connect API key,
  staples, uploads `Orch_<version>_darwin_arm64.zip`, and dispatches the tap's `bump-cask.yml`.
  Without the Apple secrets it still builds the app but uploads nothing, because Gatekeeper would
  block an unsigned app. Builds use only the dependency versions in `Package.resolved`.
- **Installing:** the `orch-app` cask in `willesq/tap` depends on the `orch` formula, which
  stays the owner of the binaries and of `brew services`.
- **Runners:** the Swift dependencies (swift-crypto, swift-log, swift-service-lifecycle) need
  Swift 6.2, so the app jobs run on `macos-26` and select its newest Xcode 26.
