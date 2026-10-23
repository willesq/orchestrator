#!/usr/bin/env bash
# Builds Orch.app and installs it for this user, replacing an earlier copy:
# quits a running Orch, swaps the bundle, and launches the new one.
#
#   INSTALL_DIR  where Orch.app goes (default ~/Applications); use a folder
#                only you can write to, since the script launches what it finds there
#   VERSION, SIGN_IDENTITY  passed through to build-app.sh
set -euo pipefail

pkg="$(cd "$(dirname "$0")/.." && pwd)"
dest_dir="${INSTALL_DIR:-$HOME/Applications}"
staging="$(mktemp -d)"
trap 'rm -rf "$staging"' EXIT

OUT_DIR="$staging" bash "$pkg/scripts/build-app.sh" >/dev/null

running() { pgrep -qx -u "$(id -u)" Orch; }

# Ask a running copy to quit, so it is not replaced underneath itself.
osascript -e 'if application id "com.willesq.orch" is running then tell application id "com.willesq.orch" to quit' >/dev/null
for _ in 1 2 3 4 5 6 7 8 9 10; do
  running || break
  sleep 0.5
done
if running; then
  echo "install-app: Orch is still running; quit it and retry" >&2
  exit 1
fi

# Copy beside the old bundle first, so a failed copy leaves the old app intact.
mkdir -p "$dest_dir"
next="$dest_dir/.Orch.app.new"
rm -rf "$next"
ditto "$staging/Orch.app" "$next"
rm -rf "$dest_dir/Orch.app"
mv "$next" "$dest_dir/Orch.app"
echo "installed $dest_dir/Orch.app"
if [ "${NO_LAUNCH:-}" != "1" ]; then
  open "$dest_dir/Orch.app"
fi
