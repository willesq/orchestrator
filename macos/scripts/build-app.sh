#!/usr/bin/env bash
# Assembles Orch.app from the SwiftPM build and signs it.
#
#   VERSION        bundle version, X.Y.Z (default 0.0.0)
#   SIGN_IDENTITY  codesign identity; a Developer ID Application identity gets
#                  the hardened runtime and a secure timestamp, as notarization
#                  requires. Default "-" (ad-hoc), for local builds.
#   OUT_DIR        where Orch.app lands (default <repo>/build/app.noindex: the
#                  .noindex suffix keeps Spotlight, and so "open Orch", from
#                  finding dev builds instead of the installed app)
set -euo pipefail

pkg="$(cd "$(dirname "$0")/.." && pwd)"
repo="$(cd "$pkg/.." && pwd)"
version="${VERSION:-0.0.0}"
identity="${SIGN_IDENTITY:--}"
out="${OUT_DIR:-$repo/build/app.noindex}"

if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "build-app: VERSION must be X.Y.Z, got '$version'" >&2
  exit 2
fi

# Only the versions pinned in Package.resolved, so a build never pulls in
# dependency code that was not reviewed in a PR.
swift build -c release --arch arm64 --package-path "$pkg" --only-use-versions-from-resolved-file --product Orch
bin="$(swift build -c release --arch arm64 --package-path "$pkg" --show-bin-path)"

app="$out/Orch.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$bin/Orch" "$app/Contents/MacOS/Orch"
# Dependencies that ship resources build them as bundles beside the binary.
for bundle in "$bin"/*.bundle; do
  [ -e "$bundle" ] && cp -R "$bundle" "$app/Contents/Resources/"
done
sed "s/__VERSION__/$version/g" "$pkg/Resources/Info.plist" > "$app/Contents/Info.plist"
plutil -lint "$app/Contents/Info.plist" >/dev/null

if [ "$identity" = "-" ]; then
  codesign --force --sign - "$app"
else
  codesign --force --options runtime --timestamp --sign "$identity" "$app"
fi
codesign --verify --strict --verbose=2 "$app"
echo "$app"
