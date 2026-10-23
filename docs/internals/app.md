# macOS app

`Orch.app` is a menu bar app for orchd. It is a client like `orch`: it dials the daemon's Unix
socket over gRPC and never opens `orch.db`, `workflows.db` or Qdrant itself, so every rule the
daemon enforces (redaction, delete confirmation, migration locks) applies to it too. The source
lives in [`macos/`](../../macos/), a Swift package.

## Layout

| Target | Contents |
|---|---|
| `OrchKit` | Everything testable: the generated orchd client (`Generated/`), the connection (`DaemonClient`), health classification (`Health`), polling (`DaemonMonitor`), daemon control (`DaemonControl`), and socket path resolution (`OrchPaths`) |
| `OrchApp` | The SwiftUI `MenuBarExtra` and its views |
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

## Daemon control

**Start** and **Restart** run `orch daemon start|restart`, which already chooses between brew
services and launchd. An app launched from Finder gets launchd's minimal `PATH`, so it looks for
`orch` in `/opt/homebrew/bin`, `/usr/local/bin`, `~/.local/bin` and `~/go/bin`. The command runs
from an argument array, never through a shell.

## Build, sign, release

- **`make app`:** assembles `build/app/Orch.app`, ad-hoc signed by default. Set
  `SIGN_IDENTITY` to a Developer ID Application identity to sign with the hardened runtime and a
  secure timestamp, which notarization requires.
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
- **Runners:** grpc-swift needs Swift 6.1, so the app jobs run on `macos-15`.
