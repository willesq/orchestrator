#!/usr/bin/env bash
# Generate/update Formula/orch.rb in willesq/homebrew-tap to point at a release.
# Usage: bump_formula.sh <version> <arm64-sha256> <asset-id>
# Requires: HOMEBREW_TAP_TOKEN (PAT with write to willesq/homebrew-tap).
#
# <asset-id> is the numeric id of the darwin_arm64 tarball release asset. The
# formula points at the api.github.com asset endpoint (not the public
# releases/download URL) so GitHubPrivateReleaseAssetDownloadStrategy can fetch
# it from the private repo using a gh-CLI-resolved token.
set -euo pipefail

VERSION="${1:?version required}"
SHA_ARM64="${2:?arm64 sha256 required}"
ASSET_ID="${3:?asset id required}"
TAP_REPO="willesq/homebrew-tap"
URL="https://api.github.com/repos/willesq/orchestrator/releases/assets/${ASSET_ID}"

WORK="$(mktemp -d)"
git clone "https://x-access-token:${HOMEBREW_TAP_TOKEN}@github.com/${TAP_REPO}.git" "$WORK"
mkdir -p "$WORK/Formula"

cat > "$WORK/Formula/orch.rb" <<RUBY
# The orchestrator repo is private. GitHubPrivateReleaseAssetDownloadStrategy
# (defined in the tap's custom_download_strategy.rb) resolves a token from
# \`gh auth token\` at formula-load time, so an authenticated gh CLI is all
# that's required — no HOMEBREW_GITHUB_API_TOKEN export. The url is the
# api.github.com asset endpoint; the numeric asset id is injected at publish.
require_relative "../custom_download_strategy.rb"
class Orch < Formula
  desc "Local-first agent orchestrator (CLI + daemon + hooks)"
  homepage "https://github.com/willesq/orchestrator"
  version "${VERSION}"
  license "MIT"

  # Prebuilt binaries; arm64 only for now.
  on_macos do
    on_arm do
      url "${URL}", using: GitHubPrivateReleaseAssetDownloadStrategy
      sha256 "${SHA_ARM64}"
    end
  end

  depends_on "gh"          # used to authenticate the private-asset download
  depends_on "onnxruntime" # runtime dylib, resolved via \`brew --prefix onnxruntime\`
  depends_on :macos

  def install
    bin.install "orch", "orchd", "orch-hook"
  end

  service do
    run [opt_bin/"orchd"]
    keep_alive true
    log_path var/"log/orch/orchd.log"
    error_log_path var/"log/orch/orchd.log"
  end

  def caveats
    <<~EOS
      This tap and the orchestrator repo are private. Install and authenticate
      the gh CLI once (the formula downloads the release asset with its token):
        brew install gh
        gh auth login

      First-time setup:
        orch model fetch          # download the embedding model (~90MB)
        orch install --apply      # wire hooks + MCP + alias (dry-run without --apply)
        brew services start orch  # start orchd (native qdrant + workflows + scheduler)
        orch check                # verify everything is green

      Note: \`brew upgrade\` does NOT auto-restart the service. After upgrading:
        brew services restart orch
    EOS
  end

  test do
    system "#{bin}/orch", "version"
  end
end
RUBY

cd "$WORK"
git config user.name "orch-release-bot"
git config user.email "orch-release-bot@users.noreply.github.com"
git add Formula/orch.rb
if git diff --cached --quiet; then
  echo "formula already up to date"
else
  git commit -m "orch ${VERSION}"
  git push origin HEAD
fi
