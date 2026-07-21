#!/usr/bin/env bash
# Generate registry.json pointing at a GitHub release's shared-library assets.
# Usage: gen-registry.sh <tag> <repo> <dist-dir>
set -euo pipefail
TAG="${1:?tag required}"
REPO="${2:?repo required (owner/name)}"
DIST="${3:-dist}"
VER="${TAG#v}"
BASE="https://github.com/${REPO}/releases/download/${TAG}"

artifact() { # goos goarch file
  local goos="$1" goarch="$2" file="$3"
  local path="${DIST}/${file}"
  [ -f "$path" ] || return 0
  local sha size
  sha="$(sha256sum "$path" | awk '{print $1}')"
  size="$(wc -c < "$path" | tr -d ' ')"
  cat <<JSON
      { "goos": "${goos}", "goarch": "${goarch}", "url": "${BASE}/${file}", "sha256": "${sha}", "size": ${size} }
JSON
}

ARTS=""
add() { local a; a="$(artifact "$@")"; [ -n "$a" ] && ARTS="${ARTS:+$ARTS,$'\n'}$a"; }
add linux   amd64 "cliproxy-codeium-linux-amd64.so"
add darwin  arm64 "cliproxy-codeium-darwin-arm64.dylib"
add windows amd64 "cliproxy-codeium-windows-amd64.dll"

cat > registry.json <<JSON
{
  "schema_version": 1,
  "plugins": [
    {
      "id": "codeium",
      "name": "Codeium / Devin (Windsurf) Provider",
      "description": "Serve Devin/Windsurf models (SWE, Claude, GPT, Gemini) via your own account. OpenAI/Anthropic/Responses compatible with tool calling.",
      "author": "senran-N",
      "version": "${VER}",
      "repository": "https://github.com/${REPO}",
      "homepage": "https://github.com/${REPO}",
      "license": "MIT",
      "tags": ["codeium", "windsurf", "devin", "openai-compatible"],
      "auth_required": true,
      "install": {
        "type": "github-release",
        "artifacts": [
${ARTS}
        ]
      }
    }
  ]
}
JSON
echo "wrote registry.json for ${TAG}"
