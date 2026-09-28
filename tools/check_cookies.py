#!/usr/bin/env python3
"""Diagnose why MiMo Studio cookies are being rejected.

The web backend scopes its endpoints: `user/*` accepts a bare serviceToken,
but `chat/*` needs a fully-established session. This script probes each layer
so you know which one is failing instead of guessing.

Usage:
    python3 check_cookies.py --config config.json
    python3 check_cookies.py --cookie-string "serviceToken=...; userId=..."
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

BASE = "https://aistudio.xiaomimimo.com"
UA = ("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")


def probe(path: str, cookie: str, *, method: str = "GET", body: dict | None = None,
          timeout: int = 25) -> tuple[int, str]:
    """Issue one request and return (status, body). Never raises for HTTP errors."""
    data = json.dumps(body).encode() if body is not None else None
    req = Request(BASE + path, data=data, method=method)
    req.add_header("Cookie", cookie)
    req.add_header("Accept-Language", "zh-CN")
    req.add_header("x-timeZone", "Asia/Shanghai")
    req.add_header("User-Agent", UA)
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")
    except HTTPError as exc:
        return exc.code, exc.read().decode("utf-8", "replace")
    except URLError as exc:
        return 0, f"connection failed: {exc}"
    except Exception as exc:  # noqa: BLE001 - report anything else verbatim
        return 0, f"error: {exc}"


def load_cookie(args) -> str:
    if args.cookie_string:
        return args.cookie_string
    path = Path(args.config)
    if not path.exists():
        sys.exit(f"error: {path} not found")
    cfg = json.loads(path.read_text())
    sessions = cfg.get("upstream", {}).get("sessions", [])
    if not sessions:
        sys.exit("error: no upstream.sessions in the config")
    cookies = sessions[0].get("cookies", [])
    return "; ".join(f"{c['name']}={c['value']}" for c in cookies)


# Endpoints ordered from least to most protected. The first failure tells you
# exactly how far the credential gets.
CHECKS: list[tuple[str, str, str, dict | None]] = [
    ("GET", "/open-apis/bot/config", "public model catalogue (no auth)", None),
    ("GET", "/open-apis/user/mi/get", "account identity", None),
    ("POST", "/open-apis/chat/conversation/list", "chat history scope",
     {"page": 1, "pageSize": 1}),
    ("POST", "/open-apis/bot/chat", "inference", {
        "msgId": "diag-1",
        "conversationId": "diag-1",
        "query": "hi",
        "isEditedQuery": False,
        "sceneType": None,
        "params": {},
        "modelConfig": {"enableThinking": False,
                        "webSearchStatus": "disabled",
                        "model": "mimo-v2.6-flash"},
        "multiMedias": [],
    }),
]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    src = ap.add_mutually_exclusive_group(required=True)
    src.add_argument("--config", default="config.json",
                     help="config file to read cookies from")
    src.add_argument("--cookie-string", help="raw Cookie header value")
    args = ap.parse_args()

    cookie = load_cookie(args)
    names = [p.split("=", 1)[0].strip() for p in cookie.split(";") if "=" in p]
    print(f"cookies presented: {', '.join(names) or '(none)'}")
    print()

    results = []
    for method, path, label, body in CHECKS:
        status, raw = probe(path, cookie, method=method, body=body)
        ok = status == 200 and '"code":0' in raw.replace(" ", "")
        # A 405 means the route exists but wants another verb; that still proves
        # the credential reached the handler.
        reached = ok or status == 405
        results.append((label, path, status, reached, raw))
        mark = "PASS" if reached else "FAIL"
        print(f"[{mark}] {label}")
        print(f"       {method} {path} -> HTTP {status}")
        if not reached:
            print(f"       {raw[:200].strip()}")
        print()

    # Summarize, then give the specific advice for the failure mode seen.
    public_ok = results[0][3]
    identity_ok = results[1][3]
    chat_ok = results[2][3] and results[3][3]

    print("=" * 62)
    if chat_ok:
        print("RESULT: cookies are fully working. Inference will succeed.")
        return 0

    if not public_ok:
        print("RESULT: cannot reach the backend at all.")
        print("        Check your network / whether the service moved.")
        return 1

    if identity_ok and not chat_ok:
        print("RESULT: cookies identify the account but do NOT authorize chat.")
        print()
        print("  The backend scopes its endpoints. `user/*` accepts a bare")
        print("  serviceToken, but `chat/*` requires a fully-established")
        print("  browser session — the short-lived cookies the site sets when")
        print("  you load the app, not just the long-lived serviceToken.")
        print()
        print("  Fix: copy the COMPLETE Cookie header from a live request.")
        print("       1. Open https://aistudio.xiaomimimo.com and log in")
        print("       2. F12 -> Network -> send a message -> click the `chat`")
        print("          request -> Request Headers -> copy the ENTIRE `Cookie:`")
        print("          line (it is long; grab all of it)")
        print("       3. python3 tools/export_cookies.py --cookie-string '<paste>'")
        print("       4. python3 tools/check_cookies.py --config config.json")
        print()
        print("  If a full header copy still fails, the session likely needs a")
        print("  fresh login: log out of the site, log back in, then copy again.")
        return 1

    print("RESULT: identity lookup failed; cookies are wrong or expired.")
    print("        Re-copy the Cookie header from a logged-in session.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
