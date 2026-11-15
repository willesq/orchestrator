#!/usr/bin/env bash
# Checks that two features built in parallel, each adding an orchd RPC's
# registration and an Orch.app model, merge without a textual conflict and
# still build (SP-081). It clones the committed tree at REF (default HEAD)
# into a temporary folder, adds the same shape of change on two branches
# from it, merges them and builds the result; the checkout it runs from is
# not changed. Set SWIFT=1 to also build the app (slow on a cold cache).
#
# The orchd.proto service and its generated code are outside this check:
# two new RPCs there conflict by design, and the merge rule is to keep both
# sides and run `make proto`.
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
ref="${1:-HEAD}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

git clone -q --no-hardlinks "$repo" "$work/r"
cd "$work/r"
git checkout -q --detach "$(git -C "$repo" rev-parse "$ref")"
# Reason: the fixture commits live only in this throwaway clone.
g() { git -c user.name=fixture -c user.email=fixture@example.invalid -c commit.gpgsign=false "$@"; }

# feature NAME adds, in new files only, what a feature with one new RPC and
# one app model adds: the RPC's caller class and space decision, a test
# that they registered, an API protocol with the client's conformance, the
# stand-in's conformance, and the model held by the app.
feature() {
  local name="$1" lower
  lower="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
  cat >"internal/daemon/rpcs_fixture_${lower}.go" <<EOF
package daemon

const fixture${name}Method = "/orchd.v1.Orchd/Fixture${name}"

var _ = registerRPCs(operatorRPC(fixture${name}Method, "run fixture ${lower}", spaceDecision{spaceNone, "a fixture"}))
EOF
  cat >"internal/daemon/rpcs_fixture_${lower}_test.go" <<EOF
package daemon

import "testing"

func TestFixture${name}Registered(t *testing.T) {
	if operatorActions[fixture${name}Method] == "" || readSpaceDecisions[fixture${name}Method].kind != spaceNone {
		t.Fatal("fixture ${name} is not registered")
	}
}
EOF
  cat >"macos/Sources/OrchKit/Fixture${name}.swift" <<EOF
public protocol Fixture${name}API: Sendable {
    func fixture${name}() async throws -> Int
}

extension DaemonClient: Fixture${name}API {
    public func fixture${name}() async throws -> Int { 0 }
}

@MainActor
public final class Fixture${name}Model {
    public let api: any Fixture${name}API
    public init(api: any Fixture${name}API) { self.api = api }
}
EOF
  cat >"macos/Sources/OrchApp/Fixture${name}Feature.swift" <<EOF
import OrchKit

extension AppClient {
    var fixture${name}: any Fixture${name}API {
        switch self {
        case .live(let client): client
        case .unreachable(let standIn): standIn
        }
    }
}

extension UnreachableDaemon: Fixture${name}API {
    func fixture${name}() async throws -> Int { throw failure }
}

extension AppModel {
    var fixture${name}: Fixture${name}Model { features.get { Fixture${name}Model(api: client.fixture${name}) } }
}
EOF
  g add -A
  g commit -q -m "fixture: feature ${name}"
}

base="$(git rev-parse HEAD)"
g checkout -q -b fixture-a "$base"
feature Alpha
g checkout -q -b fixture-b "$base"
feature Beta
g checkout -q fixture-a
if ! g merge -q --no-edit fixture-b; then
  echo "FAIL: the two fixture features conflict:" >&2
  git diff --name-only --diff-filter=U >&2
  exit 1
fi
echo "ok: merged without conflict ($(git diff --name-only "$base" HEAD | wc -l | tr -d ' ') files added)"
if [ -n "$(git diff --name-only --diff-filter=M "$base" HEAD)" ]; then
  echo "FAIL: the fixtures had to edit shared files:" >&2
  git diff --name-only --diff-filter=M "$base" HEAD >&2
  exit 1
fi

go vet ./internal/daemon ./internal/cli ./internal/hook ./internal/mcpserver
go test ./internal/daemon -run 'TestFixture(Alpha|Beta)Registered' -count=1
# The fixtures' RPCs are not in orchd.proto, so the registry checks must
# reject them: registered entries are checked like the shared maps' ones.
if out="$(go test ./internal/daemon -run 'TestEveryRPCIsClassified|TestEveryRPCHasASpaceDecision' -count=1 2>&1)"; then
  echo "FAIL: the registry checks accepted RPCs Orchd does not serve" >&2
  exit 1
fi
for name in FixtureAlpha FixtureBeta; do
  if [ "$(grep -c "$name, which Orchd does not serve" <<<"$out")" -ne 2 ]; then
    echo "FAIL: the registry checks did not reject $name in both tests:" >&2
    echo "$out" >&2
    exit 1
  fi
done
echo "ok: the registry checks reject the fixtures' unserved RPCs"
if [ "${SWIFT:-0}" = 1 ]; then
  swift build --package-path macos
fi
echo "ok: the merged fixtures build and register"
