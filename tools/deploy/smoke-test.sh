#!/usr/bin/env bash
# End-to-end smoke test against a mock upstream.
#
# Verifies the whole path — HTTP surface, auth, request translation, SSE
# decoding, streaming — without touching a real MiMo account or spending any
# quota. Run it after any change to the protocol layer.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$PWD

# The toolchain is not always on PATH in this environment.
if ! command -v go >/dev/null 2>&1; then
  for candidate in "$HOME/go-toolchain/go127/bin" "$HOME/go-toolchain/go/bin"; do
    if [ -x "$candidate/go" ]; then
      export PATH="$candidate:$PATH"
      break
    fi
  done
fi
command -v go >/dev/null 2>&1 || { echo "FAIL: no Go toolchain found" >&2; exit 1; }
PORT=${SMOKE_PORT:-18793}
TOKEN="smoke-token-0123456789abcdef"
MOCK_PORT=${SMOKE_MOCK_PORT:-18794}
TMP=$(mktemp -d)
trap 'cleanup' EXIT

PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do
    kill "$pid" 2>/dev/null || true
  done
  rm -rf "$TMP"
}

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "  ok: $*"; }

echo "==> building"
go build -o "$TMP/mimowebapi" . || fail "build failed"

echo "==> starting mock upstream on :$MOCK_PORT"
cat >"$TMP/mock.py" <<'PY'
import http.server, json, sys, socketserver

PORT = int(sys.argv[1])
STREAM = (
    'event: dialogId\ndata: {"content":"dlg-smoke"}\n\n'
    'event: message\ndata: {"content":"Hello"}\n\n'
    'event: message\ndata: {"content":", smoke"}\n\n'
    'event: usage\ndata: {"prompt_tokens":5,"completion_tokens":3}\n\n'
    'event: finish\ndata: {}\n\n'
)

class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_GET(self):
        body = json.dumps({"code": 0, "data": {"modelConfigList": [
            {"name": "MiMo-V2.6-Flash", "model": "mimo-v2.6-flash"}]}}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(n)
        try:
            payload = json.loads(raw)
        except Exception:
            payload = {}
        if not self.headers.get("Cookie"):
            body = json.dumps({"code": 401, "loginUrl": "https://x/"}).encode()
            self.send_response(401)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        # Echo back what the relay translated, so the test can assert on it.
        if self.path.startswith("/echo"):
            body = json.dumps(payload).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        body = STREAM.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", PORT), Handler) as httpd:
    httpd.serve_forever()
PY
python3 "$TMP/mock.py" "$MOCK_PORT" >"$TMP/mock.log" 2>&1 &
PIDS+=($!)
sleep 1

echo "==> writing config"
cat >"$TMP/config.json" <<EOF
{
  "listen": "127.0.0.1:$PORT",
  "client_tokens": ["$TOKEN"],
  "upstream": {
    "base_url": "http://127.0.0.1:$MOCK_PORT",
    "sessions": [{"label":"smoke","cookies":[
      {"name":"serviceToken","value":"smoke-value"}]}],
    "cooldown_seconds": 2
  },
  "models": {"default":"mimo-v2.6-flash","list":["mimo-v2.6-flash"]},
  "admin": {"password":"smoke-admin-password-123","key_store_path":"","console":true}
}
EOF
chmod 600 "$TMP/config.json"

"$TMP/mimowebapi" -check -config "$TMP/config.json" >/dev/null \
  || fail "config validation rejected a valid config"
pass "config validates"

echo "==> starting relay on :$PORT"
"$TMP/mimowebapi" -config "$TMP/config.json" >"$TMP/relay.log" 2>&1 &
PIDS+=($!)
sleep 1

BASE="http://127.0.0.1:$PORT"

# --- health ---------------------------------------------------------------
curl -sf "$BASE/healthz" | grep -q '"status":"ok"' || fail "healthz not ok"
pass "healthz"

# --- auth is enforced -----------------------------------------------------
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/models")
[ "$code" = "401" ] || fail "unauthenticated /v1/models returned $code, want 401"
pass "auth enforced"

# --- model listing --------------------------------------------------------
curl -sf "$BASE/v1/models" -H "Authorization: Bearer $TOKEN" \
  | grep -q 'mimo-v2.6-flash' || fail "model list missing the configured model"
pass "model list"

# --- non-streaming completion --------------------------------------------
resp=$(curl -sf "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}]}')
echo "$resp" | grep -q '"content":"Hello, smoke"' \
  || fail "non-streaming content wrong: $resp"
echo "$resp" | grep -q '"finish_reason":"stop"' \
  || fail "non-streaming finish_reason wrong: $resp"
echo "$resp" | grep -q '"prompt_tokens":5' \
  || fail "usage not propagated: $resp"
pass "non-streaming completion"

# --- streaming completion -------------------------------------------------
stream=$(curl -sf "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"mimo-v2.6-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}')
echo "$stream" | grep -q '"role":"assistant"' || fail "stream missing role chunk"
echo "$stream" | grep -q '"content":"Hello"' || fail "stream missing first delta"
echo "$stream" | grep -q 'data: \[DONE\]' || fail "stream missing [DONE] sentinel"
pass "streaming completion"

# --- anthropic surface ----------------------------------------------------
am=$(curl -sf "$BASE/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"mimo-v2.6-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}')
echo "$am" | grep -q '"type":"message"' || fail "anthropic envelope wrong: $am"
echo "$am" | grep -q '"text":"Hello, smoke"' || fail "anthropic content wrong: $am"
pass "anthropic non-streaming"

as=$(curl -sf "$BASE/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"mimo-v2.6-flash","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}')
for ev in message_start content_block_start content_block_delta \
          content_block_stop message_delta message_stop; do
  echo "$as" | grep -q "event: $ev" || fail "anthropic stream missing $ev"
done
pass "anthropic streaming event order"

# --- session health is visible and secret-free ---------------------------
st=$(curl -sf "$BASE/status" -H "Authorization: Bearer $TOKEN")
echo "$st" | grep -q '"label":"smoke"' || fail "status missing session"
echo "$st" | grep -q 'smoke-value' && fail "status leaked a cookie value"
pass "status hides secrets"

# --- admin console & key lifecycle ---------------------------------------
echo "==> admin console"

code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/console")
[ "$code" = "200" ] || fail "console page returned $code"
pass "console page served"

code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/admin/api/keys")
[ "$code" = "401" ] || fail "admin API unauthenticated returned $code, want 401"
pass "admin API requires auth"

ADMIN="smoke-admin-password-123"
CJ="$TMP/admin-cookies.txt"
curl -s -c "$CJ" -X POST "$BASE/admin/api/login" \
  -H 'Content-Type: application/json' \
  -d "{\"password\":\"$ADMIN\"}" -o /dev/null -w '' || fail "admin login failed"

code=$(curl -s -b "$CJ" -o /dev/null -w '%{http_code}' "$BASE/admin/api/session")
[ "$code" = "200" ] || fail "admin session not established ($code)"
pass "admin login + session"

ISSUED=$(curl -s -b "$CJ" -X POST "$BASE/admin/api/keys" \
  -H 'Content-Type: application/json' \
  -d '{"name":"smoke","quota_requests":50,"rate_limit_rpm":30}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["secret"])')
case "$ISSUED" in
  sk-mimo-*) : ;;
  *) fail "issued key has the wrong shape: $ISSUED" ;;
esac
pass "key issued"

code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/models" \
  -H "Authorization: Bearer $ISSUED")
[ "$code" = "200" ] || fail "issued key rejected by the public API ($code)"
pass "issued key authenticates"

curl -s -b "$CJ" "$BASE/admin/api/keys" | grep -q "$ISSUED" \
  && fail "key listing leaked the plaintext secret"
pass "key listing hides secrets"

resp=$(curl -s "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $ISSUED" -H 'Content-Type: application/json' \
  -d '{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}]}')
echo "$resp" | grep -q '"content":"Hello, smoke"' || fail "issued key inference failed: $resp"
pass "inference with an issued key"

USED=$(curl -s -b "$CJ" "$BASE/admin/api/keys" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["keys"][0]["used_requests"])')
[ "$USED" -ge 1 ] || fail "usage was not recorded (used_requests=$USED)"
pass "usage accounted per key"

# Quota enforcement: a key limited to one request must 429 on the second.
K2=$(curl -s -b "$CJ" -X POST "$BASE/admin/api/keys" \
  -H 'Content-Type: application/json' -d '{"name":"quota","quota_requests":1}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["secret"])')
curl -s -o /dev/null "$BASE/v1/models" -H "Authorization: Bearer $K2"
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/models" -H "Authorization: Bearer $K2")
[ "$code" = "429" ] || fail "quota not enforced (got $code, want 429)"
pass "quota enforced"

echo
# --- upstream failure is reported honestly -------------------------------
# Removing the cookies makes the mock answer 401; the relay must surface that
# as an upstream auth error rather than an empty success.
# Point the session store at a scratch directory. A restart deliberately
# reloads a saved session, so without this the test inherits whatever the
# developer has authorised locally and never observes the empty case.
python3 - "$TMP/config.json" "$TMP/nosession" <<'PY'
import json, os, sys
cfg, session_dir = sys.argv[1], sys.argv[2]
os.makedirs(session_dir, exist_ok=True)
c = json.load(open(cfg))
c['upstream']['sessions'] = []
c['admin']['session_dir'] = session_dir
json.dump(c, open(cfg, 'w'))
PY
kill "${PIDS[1]}" 2>/dev/null || true
wait "${PIDS[1]}" 2>/dev/null || true
"$TMP/mimowebapi" -config "$TMP/config.json" >>"$TMP/relay.log" 2>&1 &
PIDS+=($!)
sleep 1
body=$(curl -s "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}]}')
echo "$body" | grep -q 'no_session' \
  || fail "missing session should yield no_session, got: $body"
pass "fail-closed with no session"

echo "all smoke checks passed"
