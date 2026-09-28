#!/usr/bin/env bash
#
# Reuse the login you already have in your everyday Chrome.
#
# The wizard can drive its own browser, but that means logging in a second
# time for an account you are already using. This script borrows the session
# from the Chrome you are already signed into, so there is nothing to log in
# to and no second profile to babysit.
#
# It talks to Chrome through the DevTools port rather than AppleScript: the
# script-only path needs "Allow JavaScript from Apple Events" enabled by hand,
# and this works without touching any menu.
#
#   ./use-my-chrome.sh              attach to Chrome, capture the session
#   ./use-my-chrome.sh --restart    relaunch Chrome with the debug port first

set -euo pipefail

cd "$(dirname "$0")"

BOLD=$'\033[1m'; DIM=$'\033[2m'; GREEN=$'\033[32m'
YELLOW=$'\033[33m'; RED=$'\033[31m'; RESET=$'\033[0m'

ok()   { printf '%s✓%s %s\n' "$GREEN" "$RESET" "$*"; }
warn() { printf '%s!%s %s\n' "$YELLOW" "$RESET" "$*"; }
die()  { printf '%s✗%s %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }

PORT=9333
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
PROFILE="$HOME/Library/Application Support/Google/Chrome"
READY=0

probe() {
  curl -s -m 2 "http://127.0.0.1:$PORT/json/version" >/dev/null 2>&1
}

# ---- restart Chrome with the debug port ------------------------------------

if [ "${1:-}" = "--restart" ]; then
  say() { printf '%s\n' "$*"; }
  say ""
  say "${BOLD}重启 Chrome 以启用调试端口${RESET}"
  say "${DIM}标签页会被恢复，登录态不受影响。${RESET}"
  say ""

  if ! [ -d "$PROFILE" ]; then
    die "找不到 Chrome 配置目录：$PROFILE"
  fi

  warn "关闭 Chrome…"
  osascript -e 'tell application "Google Chrome" to quit' 2>/dev/null || true
  for _ in $(seq 1 20); do
    pgrep -f "Google Chrome$" >/dev/null 2>&1 || break
    sleep 0.3
  done
  if pgrep -f "Google Chrome$" >/dev/null 2>&1; then
    warn "还在运行，强制结束"
    pkill -f "Google Chrome$" 2>/dev/null || true
    sleep 1
  fi
  ok "已关闭"

  # The debug port must be bound on loopback only: this endpoint can read
  # every cookie in the profile, so it must not be reachable from the network.
  nohup "$CHROME" \
    --remote-debugging-port=$PORT \
    --remote-debugging-address=127.0.0.1 \
    --restore-last-session \
    >/dev/null 2>&1 &

  warn "等待 Chrome 起来（它可能问你是否恢复标签页，选恢复就好）…"
  for _ in $(seq 1 60); do
    probe && { READY=1; break; }
    sleep 0.5
  done
  [ "$READY" = "1" ] || die "调试端口没起来。手动启动也行：见 README 的说明。"
  ok "Chrome 已在调试模式运行"
  say ""
fi

# ---- attach ----------------------------------------------------------------

if ! probe; then
  cat <<EOF

${BOLD}你的 Chrome 没有开调试端口。${RESET}

Chrome 只能在使用 ${DIM}--remote-debugging-port${RESET} 启动时被读取，
而正在运行的实例无法中途加上这个开关。

${BOLD}两条路，任选：${RESET}

  ${BOLD}1. 让脚本帮你重启（推荐）${RESET}
     ${DIM}./use-my-chrome.sh --restart${RESET}
     会关掉 Chrome 再带调试端口打开，标签页自动恢复，登录态不变。

  ${BOLD}2. 自己重启 Chrome${RESET}
     完全退出 Chrome，然后在终端里跑：
     ${DIM}"$CHROME" --remote-debugging-port=$PORT --restore-last-session${RESET}

然后重新执行：${DIM}./use-my-chrome.sh${RESET}

${DIM}调试端口只绑在 127.0.0.1，外部网络访问不到。用完关掉 Chrome
再正常打开即可，不留痕迹。${RESET}

EOF
  exit 1
fi

ok "已连上 Chrome 调试端口"

# ---- find a logged-in MiMo tab ---------------------------------------------

TARGETS=$(curl -s -m 5 "http://127.0.0.1:$PORT/json/list" || echo "[]")

PAGE=$(python3 - "$TARGETS" <<'PY'
import json, sys
raw = sys.argv[1] if len(sys.argv) > 1 else "[]"
try:
    targets = json.loads(raw)
except Exception:
    print("")
    raise SystemExit
pages = [t for t in targets if t.get("type") == "page"
         and "xiaomimimo.com" in t.get("url", "")]
print(pages[0]["webSocketDebuggerUrl"] if pages else "")
PY
)

if [ -z "$PAGE" ]; then
  warn "没有找到打开着 MiMo 的标签页"
  echo
  echo "  在 Chrome 里打开 https://aistudio.xiaomimimo.com/ 并确认已登录，"
  echo "  然后重新执行本脚本。"
  echo
  exit 1
fi

ok "找到已登录的 MiMo 标签页"

# ---- capture ---------------------------------------------------------------

COOKIES=$(python3 - "$PORT" <<'PY'
import base64, json, os, socket, struct, sys, urllib.request

port = int(sys.argv[1])

def http_get(path, timeout=8):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=timeout) as r:
        return json.loads(r.read().decode())

targets = http_get("/json/list")
pages = [t for t in targets if t.get("type") == "page"
         and "xiaomimimo.com" in t.get("url", "")]
if not pages:
    raise SystemExit("no MiMo page")

ws = pages[0]["webSocketDebuggerUrl"]
hostport = ws.split("//", 1)[1].split("/", 1)[0]
host, port_s = hostport.rsplit(":", 1)
path = "/" + ws.split("//", 1)[1].split("/", 1)[1]

sock = socket.create_connection((host, int(port_s)), timeout=15)
key = base64.b64encode(os.urandom(16)).decode()
sock.sendall((
    f"GET {path} HTTP/1.1\r\nHost: {hostport}\r\n"
    "Upgrade: websocket\r\nConnection: Upgrade\r\n"
    f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n"
).encode())
if b"101" not in sock.recv(4096):
    raise SystemExit("websocket handshake failed")

def send(obj):
    data = json.dumps(obj).encode()
    mask = os.urandom(4)
    header = bytearray([0x81])
    n = len(data)
    if n < 126:
        header.append(0x80 | n)
    elif n < 65536:
        header.append(0x80 | 126)
        header += struct.pack(">H", n)
    else:
        header.append(0x80 | 127)
        header += struct.pack(">Q", n)
    header += mask
    sock.sendall(bytes(header) + bytes(b ^ mask[i % 4] for i, b in enumerate(data)))

def recv():
    head = sock.recv(2)
    if not head:
        return None
    n = head[1] & 0x7F
    if n == 126:
        n = struct.unpack(">H", sock.recv(2))[0]
    elif n == 127:
        n = struct.unpack(">Q", sock.recv(8))[0]
    buf = b""
    while len(buf) < n:
        buf += sock.recv(n - len(buf))
    return json.loads(buf)

send({"id": 1, "method": "Network.getAllCookies"})
while True:
    msg = recv()
    if not msg:
        raise SystemExit("connection closed")
    if msg.get("id") == 1:
        cookies = msg["result"]["cookies"]
        break

def wanted(name):
    if name in ("serviceToken",) or name.endswith("_serviceToken"):
        return True
    for suffix in ("ph", "userId", "cUserId", "passToken", "deviceId",
                   "uLocale", "locale"):
        if name == suffix or name.endswith("_" + suffix):
            return True
    return False

def unwrap(v):
    if len(v) >= 2 and v[0] == '"' and v[-1] == '"':
        return v[1:-1]
    return v

out, seen = [], set()
for c in cookies:
    dom = c.get("domain", "")
    if "xiaomimimo.com" not in dom and "xiaomi.com" not in dom:
        continue
    name, value = c["name"], unwrap(c.get("value", ""))
    if not value or name in seen or not wanted(name):
        continue
    seen.add(name)
    out.append({"name": name, "value": value})

print(json.dumps(out))
PY
)

COUNT=$(printf '%s' "$COOKIES" | python3 -c "import json,sys;print(len(json.load(sys.stdin)))")
ok "抓到 $COUNT 个 cookie"

# ---- hand it to the running server -----------------------------------------

ADMIN_PW=$(python3 -c "
import json
print(json.load(open('config.json'))['admin']['password'])" 2>/dev/null || echo "")
LISTEN=$(python3 -c "
import json
c=json.load(open('config.json'))
print(c['listen'])" 2>/dev/null || echo "127.0.0.1:8793")
BASE="http://$LISTEN"

if ! curl -s -m 3 "$BASE/healthz" >/dev/null 2>&1; then
  warn "服务没在跑（$BASE）。先启动：./mimowebapi -config config.json"
  printf '%s' "$COOKIES" > .chrome-session.json
  chmod 600 .chrome-session.json
  echo "  cookie 已暂存到 .chrome-session.json，启动服务后执行："
  echo "    ./use-my-chrome.sh"
  exit 0
fi

LOGIN=$(curl -s -X POST "$BASE/admin/api/login" \
  -H "Content-Type: application/json" \
  -d "$(python3 -c "import json,sys;print(json.dumps({'password':sys.argv[1]}))" "$ADMIN_PW")" \
  -c /tmp/mimo-attach-cookies.txt 2>/dev/null || echo "")

PAYLOAD=$(python3 -c "
import json,sys
cookies = json.loads(sys.argv[1])
print(json.dumps({'cookie': '; '.join(f\"{c['name']}={c['value']}\" for c in cookies),
                  'label': 'from-chrome'}))" "$COOKIES")

RESP=$(curl -s -b /tmp/mimo-attach-cookies.txt -X POST "$BASE/admin/api/auth/manual" \
  -H "Content-Type: application/json" \
  -d "$PAYLOAD")
rm -f /tmp/mimo-attach-cookies.txt

echo "$RESP" | grep -q '"ok":true' || die "服务拒绝了这组 cookie：$RESP"

# A cookie set that the backend will not accept is worse than none: it looks
# authorised and then fails every request. Say so instead of reporting success.
echo "$RESP" | python3 -c "
import json, sys
d = json.load(sys.stdin)
msg = d.get('warn')
sys.exit(1 if msg else 0)
" 2>/dev/null || {
  warn "这组 cookie 看起来不完整，上游可能仍会拒绝"
}

echo
ok "已接管你 Chrome 的登录态"
echo
echo "  ${DIM}账号名：from-chrome${RESET}"
echo "  ${DIM}验证：$BASE/admin/api/auth/status${RESET}"
echo
echo "  现在可以直接用了。要撤销：控制台「账号」页删掉即可。"
echo

# ---- verify ----------------------------------------------------------------

MODEL=$(python3 -c "
import json
print(json.load(open('config.json'))['models']['default'])" 2>/dev/null || echo "mimo-v2.6-flash")
TOKEN=$(python3 -c "
import json
print(json.load(open('config.json'))['client_tokens'][0])" 2>/dev/null || echo "")

if [ -n "$TOKEN" ]; then
  echo "  ${DIM}正在发一条真请求验证…${RESET}"
  CODE=$(curl -s -o /tmp/mimo-verify-out.txt -w "%{http_code}" -m 60 \
    -X POST "$BASE/v1/chat/completions" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":16}" \
    2>/dev/null || echo "000")

  if [ "$CODE" = "200" ]; then
    ok "验证通过 —— 接口真的能用了"
  else
    warn "接口返回 $CODE："
    head -c 300 /tmp/mimo-verify-out.txt 2>/dev/null | sed 's/^/    /'
    echo
    echo "  ${DIM}cookie 已接管，但上游仍拒绝。把上面的内容发我。${RESET}"
  fi
  rm -f /tmp/mimo-verify-out.txt
fi
echo
