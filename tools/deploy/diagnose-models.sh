#!/usr/bin/env bash
# Why can't sub2api see my models?
#
# "Sync failed" from an aggregator can mean two very different things: the
# request never arrived, or it arrived and was answered in a shape the caller
# did not accept. This script tells the two apart in about five seconds, which
# is the difference between fixing a firewall and fixing a payload.
#
# Usage:  ./deploy/diagnose-models.sh [base_url] [api_key]
set -uo pipefail

cd "$(dirname "$0")/.."

CONFIG=${CONFIG:-config.json}
BASE=${1:-}
KEY=${2:-}

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }

if [ ! -f "$CONFIG" ]; then
  bad "找不到 $CONFIG"
  exit 1
fi

# Read the parts of the config this script needs. Values are not echoed.
read_cfg() {
  python3 - "$CONFIG" "$1" <<'PY'
import json, sys
try:
    c = json.load(open(sys.argv[1]))
except Exception:
    print(""); raise SystemExit
key = sys.argv[2]
if key == "listen": print(c.get("listen", ""))
elif key == "token":
    t = c.get("client_tokens") or [""]
    print(t[0])
elif key == "models":
    print(" ".join(c.get("models", {}).get("list", [])))
PY
}

LISTEN=$(read_cfg listen)
TOKEN_FROM_CFG=$(read_cfg token)
MODELS=$(read_cfg models)

[ -n "$KEY" ] || KEY="$TOKEN_FROM_CFG"

# Default the probe target to whatever the listener actually is. Probing
# 127.0.0.1 when the service is bound to a LAN address would report a false
# failure, and vice versa.
if [ -z "$BASE" ]; then
  HOST=${LISTEN%:*}
  PORT=${LISTEN##*:}
  [ "$HOST" = "0.0.0.0" ] && HOST=127.0.0.1
  [ "$HOST" = "::" ] && HOST=127.0.0.1
  BASE="http://$HOST:$PORT"
fi

bold "1. 服务状态"
info "配置文件: $CONFIG"
info "监听地址: $LISTEN"
info "探测目标: $BASE"
info "配置模型: $MODELS"

if [ "$LISTEN" = "127.0.0.1:${LISTEN##*:}" ] || [[ "$LISTEN" == 127.0.0.1:* ]]; then
  warn "只绑定了本机回环地址"
  info "只有运行 sub2api 的同一台机器能连上。"
  info "如果 sub2api 在别的机器或容器里，这一条就是全部原因。"
  info "改法: 把 listen 改为 0.0.0.0:${LISTEN##*:}，"
  info "      并设 allow_non_loopback_listen=true，再用防火墙限制来源。"
fi

if ! curl -sf -m 5 "$BASE/healthz" >/dev/null 2>&1; then
  bad "服务无响应: $BASE/healthz"
  info "服务没在跑，或者这个地址不对。先解决这一步。"
  exit 1
fi
ok "服务在线"

bold "2. 鉴权"
if [ -z "$KEY" ]; then
  bad "没有可用的 key"
  info "在 config.json 的 client_tokens 里放一个，或用控制台新建一个。"
  exit 1
fi

NOAUTH=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$BASE/v1/models")
if [ "$NOAUTH" = "401" ]; then
  ok "不带 key 时正确返回 401"
else
  warn "不带 key 返回 $NOAUTH，期望 401"
fi

AUTH=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$BASE/v1/models" \
  -H "Authorization: Bearer $KEY")
if [ "$AUTH" = "200" ]; then
  ok "带 key 可以读取模型列表"
else
  bad "带 key 返回 $AUTH"
  info "key 不对，或者用了 sub2api 的 key 而不是本服务的 key。"
  exit 1
fi

bold "3. 各家聚合器探测的路径"
# Aggregators disagree about where the listing lives, and a 404 on any spelling
# they try reads as "this upstream has no models".
for path in /v1/models /models; do
  code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$BASE$path" \
    -H "Authorization: Bearer $KEY")
  if [ "$code" = "200" ]; then ok "GET $path → 200"; else bad "GET $path → $code"; fi
done

FIRST_MODEL=$(echo "$MODELS" | awk '{print $1}')
if [ -n "$FIRST_MODEL" ]; then
  for path in "/v1/models/$FIRST_MODEL" "/models/$FIRST_MODEL"; do
    code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$BASE$path" \
      -H "Authorization: Bearer $KEY")
    if [ "$code" = "200" ]; then ok "GET $path → 200"; else bad "GET $path → $code"; fi
  done
fi

bold "4. 返回内容是否够聚合器渲染"
BODY=$(curl -s -m 5 "$BASE/v1/models" -H "Authorization: Bearer $KEY")
python3 - "$BODY" <<'PY'
import json, sys
try:
    d = json.loads(sys.argv[1])
except Exception as e:
    print(f"  \033[31m✗\033[0m 返回不是合法 JSON: {e}")
    raise SystemExit(1)
data = d.get("data")
if not isinstance(data, list):
    print("  \033[31m✗\033[0m 缺少 data 数组")
    raise SystemExit(1)
if not data:
    print("  \033[31m✗\033[0m data 是空的，聚合器会认为没有可用模型")
    raise SystemExit(1)
print(f"  \033[32m✓\033[0m object={d.get('object')}  模型数={len(data)}")
missing = [m.get("id") for m in data if not m.get("context_length")]
for m in data:
    ctx = m.get("context_length", "—")
    print(f"    {m.get('id','?'):<34} ctx={ctx}")
if missing:
    print(f"  \033[33m!\033[0m 这些模型缺少 context_length: {', '.join(missing)}")
    print("    聚合器的模型选择器可能显示为空行。在 config.json 的 models.meta 里补上。")
PY

bold "5. 对话接口（同步通过后真正要用的）"
CHAT=$(curl -s -o /dev/null -w '%{http_code}' -m 60 \
  -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d "{\"model\":\"${FIRST_MODEL:-mimo-v2.6-flash}\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":8}")
if [ "$CHAT" = "200" ]; then ok "POST /v1/chat/completions → 200"; else bad "POST /v1/chat/completions → $CHAT"; fi

echo
bold "结论"
if [ "$LISTEN" = "127.0.0.1:${LISTEN##*:}" ] || [[ "$LISTEN" == 127.0.0.1:* ]]; then
  echo "  接口本身正常，但只监听本机。"
  echo "  如果 sub2api 不在这台机器上，请先解决网络可达性 —— 格式再对也连不上。"
  echo
fi
echo "  base_url 填:  $BASE/v1"
echo "  api key  填:  config.json 里的 client_tokens[0]，或控制台新建的 sk-mimo-..."
