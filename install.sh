#!/usr/bin/env bash
#
# One-shot installer for mimowebapi.
#
# It builds the binary, generates a config with a random admin password, and
# prints exactly what to do next. Running it twice is safe: an existing config
# is left alone and its password is reused.

set -euo pipefail

cd "$(dirname "$0")"

BOLD=$'\033[1m'; DIM=$'\033[2m'; GREEN=$'\033[32m'
YELLOW=$'\033[33m'; RED=$'\033[31m'; RESET=$'\033[0m'

say()  { printf '%s\n' "$*"; }
ok()   { printf '%s✓%s %s\n' "$GREEN" "$RESET" "$*"; }
warn() { printf '%s!%s %s\n' "$YELLOW" "$RESET" "$*"; }
die()  { printf '%s✗%s %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }

say ""
say "${BOLD}MiMo API 中转 · 安装${RESET}"
say "${DIM}把小米 MiMo 变成 OpenAI / Anthropic 兼容接口${RESET}"
say ""

# ---- prerequisites ---------------------------------------------------------

if ! command -v go >/dev/null 2>&1; then
  for candidate in "$HOME/go-toolchain/go127/bin" "$HOME/go-toolchain/go/bin" \
                   /usr/local/go/bin /opt/homebrew/bin; do
    if [ -x "$candidate/go" ]; then
      export PATH="$candidate:$PATH"
      break
    fi
  done
fi
command -v go >/dev/null 2>&1 || die "没找到 Go。装一个：https://go.dev/dl/"
ok "Go $(go version | awk '{print $3}' | sed 's/go//')"

if ! command -v python3 >/dev/null 2>&1; then
  die "没找到 python3，需要它来生成配置。"
fi
ok "python3"

# ---- build -----------------------------------------------------------------

say ""
say "编译中…"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o mimowebapi . \
  || die "编译失败"
ok "mimowebapi 已生成（$(du -h mimowebapi | cut -f1)）"

# ---- config ----------------------------------------------------------------

NEW_CONFIG=0
if [ -f config.json ]; then
  ok "沿用已有的 config.json"
else
  NEW_CONFIG=1
  ADMIN_PW=$(python3 -c "import secrets;print(secrets.token_urlsafe(18))")
  CLIENT_TOKEN=$(python3 -c "import secrets;print(secrets.token_hex(32))")

  python3 - "$ADMIN_PW" "$CLIENT_TOKEN" <<'PY'
import json, sys

admin_pw, client_token = sys.argv[1], sys.argv[2]

config = {
    "listen": "127.0.0.1:8793",
    "allow_non_loopback_listen": False,
    "client_tokens": [client_token],
    "upstream": {
        "base_url": "https://aistudio.xiaomimimo.com",
        "sessions": [],
        "connect_timeout_seconds": 15,
        "response_header_timeout_seconds": 120,
        "cooldown_seconds": 300,
    },
    "models": {
        "default": "mimo-v2.6-flash",
        "list": ["mimo-v2.6-pro", "mimo-v2.6-flash",
                 "mimo-v2.6-pro-ultraspeed-studio"],
    },
    "behavior": {
        "enable_thinking_default": True,
        "web_search_default": "auto",
        "system_prompt_mode": "prepend",
    },
    "admin": {
        "password": admin_pw,
        "key_store_path": "keys.json",
        "console": True,
        "default_rate_limit_rpm": 60,
        "default_quota_requests": 0,
        "default_quota_tokens": 0,
        "session_label": "mimo",
    },
    "log": {"level": "info", "usage": True},
}

with open("config.json", "w") as f:
    json.dump(config, f, indent=2, ensure_ascii=False)
    f.write("\n")

with open(".installed-credentials", "w") as f:
    f.write(f"admin_password={admin_pw}\n")
    f.write(f"client_token={client_token}\n")
PY

  chmod 600 config.json .installed-credentials 2>/dev/null || true

  # Fail here rather than at first launch: the config parser rejects unknown
  # fields, so a typo in the template above would otherwise only surface as a
  # confusing error minutes later.
  if ! CHECK_OUT=$(./mimowebapi -config config.json -check 2>&1); then
    say ""
    warn "生成的 config.json 没通过校验："
    say "$CHECK_OUT"
    die "安装模板有误，请把上面的错误报给维护者"
  fi
  ok "config.json 已生成并通过校验"
fi

# ---- read back what matters ------------------------------------------------

ADMIN_PW=$(python3 -c "
import json
c=json.load(open('config.json'))
print(c.get('admin',{}).get('password',''))")
PORT=$(python3 -c "
import json
c=json.load(open('config.json'))
print(c.get('listen','127.0.0.1:8793').split(':')[-1])")

# ---- summary ---------------------------------------------------------------

say ""
say "${BOLD}装好了。${RESET}"
say ""
say "  启动服务："
say "    ${BOLD}./mimowebapi -config config.json${RESET}"
say ""
say "  然后打开控制台，用这个密码登录："
say "    ${BOLD}http://127.0.0.1:${PORT}/console${RESET}"
if [ "$NEW_CONFIG" = "1" ]; then
  say "    密码 ${BOLD}${ADMIN_PW}${RESET}"
  say "    ${DIM}（存在 config.json 的 admin.password，可随时改）${RESET}"
fi
say ""
say "  第一次打开会引导你登录小米账号，登录完就能用了。"
say ""

if [ "$NEW_CONFIG" = "1" ]; then
  warn "config.json 里有你的管理密码，别提交到 git（.gitignore 已处理）"
fi
say ""
