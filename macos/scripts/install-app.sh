#!/usr/bin/env bash
# Builds Orch.app and installs it, replacing an earlier copy: quits a running
# Orch, swaps the bundle, and launches the new one.
#
#   INSTALL_DIR  where Orch.app goes. Default /Applications, where Finder,
#                Launchpad and the Homebrew cask put apps; ~/Applications when
#                /Applications is not writable. The script launches what it
#                finds there, so use a folder other users cannot write to.
#   VERSION, SIGN_IDENTITY  passed through to build-app.sh
set -euo pipefail

pkg="$(cd "$(dirname "$0")/.." && pwd)"
if [ -n "${INSTALL_DIR:-}" ]; then
  dest_dir="$INSTALL_DIR"
elif [ -w /Applications ]; then
  dest_dir=/Applications
else
  dest_dir="$HOME/Applications"
fi
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

# Remove a copy an earlier install left in the other standard folder, so
# Spotlight and Login Items see one Orch. Only a bundle that is this app
# (by bundle id) is removed.
for other in /Applications/Orch.app "$HOME/Applications/Orch.app"; do
  [ "$other" -ef "$dest_dir/Orch.app" ] && continue
  [ -d "$other" ] || continue
  id="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$other/Contents/Info.plist" 2>/dev/null || true)"
  if [ "$id" = "com.willesq.orch" ]; then
    rm -rf "$other"
    echo "removed the earlier copy at $other"
  fi
done
if [ "${NO_LAUNCH:-}" != "1" ]; then
  open "$dest_dir/Orch.app"
fi
