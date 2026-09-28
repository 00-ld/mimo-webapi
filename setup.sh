#!/usr/bin/env bash
# One-shot setup for mimowebapi.
#
# Handles the two things that trip people up: the project is NOT in $HOME, and
# the Go toolchain is not on PATH by default. Run this from anywhere.
set -euo pipefail

PROJECT="$HOME/Desktop/AI_Lrean/mimo-webapi"

if [ ! -d "$PROJECT" ]; then
  echo "error: project not found at $PROJECT" >&2
  echo "       edit PROJECT= at the top of this script if it moved." >&2
  exit 1
fi

cd "$PROJECT"

# The toolchain lives outside PATH in this environment. go1.27.0 is the
# version the module was built and tested against.
if ! command -v go >/dev/null 2>&1; then
  for candidate in "$HOME/go-toolchain/go127/bin" "$HOME/go-toolchain/go/bin"; do
    if [ -x "$candidate/go" ]; then
      export PATH="$candidate:$PATH"
      echo "using Go from $candidate"
      break
    fi
  done
fi

if ! command -v go >/dev/null 2>&1; then
  echo "error: no Go toolchain found." >&2
  echo "       expected one under ~/go-toolchain/" >&2
  exit 1
fi

echo "==> go version: $(go version)"
echo "==> building"
go build -o mimowebapi .
echo "    built $PROJECT/mimowebapi"

# --- cookies ---------------------------------------------------------------
if [ -f config.json ]; then
  echo "==> config.json already exists, leaving it alone"
  TOKEN=$(python3 -c "import json;print(json.load(open('config.json'))['client_tokens'][0])")
else
  echo
  echo "==> no config.json yet — need MiMo Studio cookies"
  echo
  echo "  Option A: automatic (reads a local browser profile)"
  echo "      python3 tools/export_cookies.py --auto --browser chrome"
  echo
  echo "  Option B: manual — this is the reliable one"
  echo "      1. Open https://aistudio.xiaomimimo.com in your browser and log in"
  echo "      2. Press F12 -> Network tab"
  echo "      3. Send any message, click the request named 'chat'"
  echo "      4. Find the Request Headers section, copy the whole 'Cookie:' line"
  echo "         (everything after 'Cookie: ', on one line)"
  echo "      5. Run:"
  echo "           python3 tools/export_cookies.py --cookie-string 'PASTE_IT_HERE'"
  echo
  echo "  Then re-run this script."
  exit 0
fi

# --- run -------------------------------------------------------------------
echo
echo "==> starting relay on 127.0.0.1:8793"
echo "    client token: $TOKEN"
echo
echo "    OpenAI clients   Base URL: http://127.0.0.1:8793/v1"
echo "                     API Key:  \$TOKEN"
echo "    Claude Code      export ANTHROPIC_BASE_URL=http://127.0.0.1:8793"
echo "                     export ANTHROPIC_API_KEY=\$TOKEN"
echo
echo "    quick test:"
echo "      curl -s http://127.0.0.1:8793/v1/chat/completions \\"
echo "        -H \"Authorization: Bearer \$TOKEN\" \\"
echo "        -H 'Content-Type: application/json' \\"
echo "        -d '{\"model\":\"mimo-v2.6-flash\",\"messages\":[{\"role\":\"user\",\"content\":\"你好\"}]}'"
echo
exec ./mimowebapi -config config.json
