#!/usr/bin/env python3
"""Export MiMo Studio cookies into a config.json for mimowebapi.

Two ways to get cookies in:

1. Automatic (Chrome/Edge/Brave, macOS): read the browser's cookie store
   directly. The browser must be closed for Chrome's store to be readable on
   some versions, and macOS will prompt for Keychain access to the encryption
   key.

2. Manual: read cookies from a file or stdin that you produced yourself, e.g.
   from DevTools -> Network -> any /open-apis request -> Request Headers ->
   Cookie, or from the Application -> Cookies panel.

Usage:
    python3 export_cookies.py --auto --browser chrome
    python3 export_cookies.py --cookie-file cookies.txt
    python3 export_cookies.py --cookie-string "xiaomichatbot_serviceToken=...; userId=..."
    cat cookies.txt | python3 export_cookies.py --stdin
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
from pathlib import Path

DOMAIN_SUFFIX = "xiaomimimo.com"
BASE_URL = "https://aistudio.xiaomimimo.com"

CONFIG_TEMPLATE = {
    "listen": "127.0.0.1:8793",
    "client_tokens": [],
    "upstream": {
        "base_url": BASE_URL,
        "sessions": [],
        "cooldown_seconds": 300,
    },
    "models": {
        "default": "mimo-v2.6-flash",
        "list": [
            "mimo-v2.6-pro",
            "mimo-v2.6-flash",
            "mimo-v2.6-pro-ultraspeed-studio",
        ],
        "alias": {},
    },
    "behavior": {
        "enable_thinking_default": True,
        "web_search_default": "auto",
        "system_prompt_mode": "prepend",
    },
    "log": {"level": "info", "usage": True},
}

# Cookie name suffixes the web backend actually uses. serviceToken is the
# session itself; the rest make the request look like a real browser session.
# Matched as suffixes because the site prefixes some of them with the service
# id (xiaomichatbot_serviceToken, xiaomichatbot_ph, ...).
WANTED_SUFFIXES = (
    "serviceToken",
    "userId",
    "cUserId",
    "passToken",
    "deviceId",
    "muid",
    "uLocale",
    "locale",
    "_ph",
)


def is_wanted(name: str) -> bool:
    """True if a cookie name is one the backend cares about."""
    return any(name == s or name.endswith(s) for s in WANTED_SUFFIXES)

def is_session_cookie(name: str) -> bool:
    """True for the cookie that carries the login.

    The name is not always bare `serviceToken`: the site may prefix it with the
    service id, e.g. `xiaomichatbot_serviceToken`. Matching on the suffix keeps
    both spellings working.
    """
    return name == "serviceToken" or name.endswith("_serviceToken")


BROWSER_PATHS = {
    "chrome": "~/Library/Application Support/Google/Chrome",
    "edge": "~/Library/Application Support/Microsoft Edge",
    "brave": "~/Library/Application Support/BraveSoftware/Brave-Browser",
    "chromium": "~/Library/Application Support/Chromium",
}


def log(msg: str) -> None:
    print(msg, file=sys.stderr)


def fail(msg: str) -> "None":
    log(f"error: {msg}")
    sys.exit(1)


def strip_quotes(value: str) -> str:
    """Remove RFC 6265 quoting from a cookie value.

    DevTools' Application panel displays cookie values wrapped in double
    quotes whenever they contain characters outside the cookie-octet set
    (MiMo's serviceToken contains '/', '+', '='). Those quotes are the
    panel's rendering, NOT part of the value -- sending them produces the
    literal value `"..."`, which the backend rejects with a 401.
    """
    if len(value) >= 2 and value[0] == '"' and value[-1] == '"':
        return value[1:-1]
    return value


def parse_cookie_string(raw: str) -> list[dict]:
    """Parse a `Cookie:` header value into structured cookies."""
    out: list[dict] = []
    seen: set[str] = set()
    for part in raw.replace("\n", ";").split(";"):
        part = part.strip()
        if not part or "=" not in part:
            continue
        name, value = part.split("=", 1)
        name, value = name.strip(), strip_quotes(value.strip())
        if not name or name in seen:
            continue
        seen.add(name)
        out.append({"name": name, "value": value})
    return out


def read_cookie_file(path: Path) -> list[dict]:
    if not path.exists():
        fail(f"cookie file not found: {path}")
    return parse_cookie_string(path.read_text(encoding="utf-8", errors="replace"))


def extract_from_browser(browser: str) -> list[dict]:
    """Read cookies out of a Chromium-family browser's SQLite store."""
    root = Path(os.path.expanduser(BROWSER_PATHS.get(browser, "")))
    if not root.exists():
        fail(f"no {browser} profile found at {root}")

    cookie_dbs = sorted(root.glob("*/Cookies"))
    cookie_dbs = [p for p in cookie_dbs if p.is_file()]
    if not cookie_dbs:
        fail(f"no Cookies database under {root}")

    # Prefer the most recently modified profile: that is the one in use.
    cookie_dbs.sort(key=lambda p: p.stat().st_mtime, reverse=True)

    found: dict[str, str] = {}
    for db in cookie_dbs:
        rows = _read_db(db)
        for name, value in rows:
            if is_wanted(name) and name not in found:
                found[name] = value
        if any(is_session_cookie(k) for k in found):
            break

    if not any(is_session_cookie(k) for k in found):
        fail(
            "no serviceToken cookie found. Log in to "
            f"{BASE_URL} in {browser} first, then re-run. "
            "If you use multiple profiles, make sure you logged in with the "
            "one used most recently."
        )
    return [{"name": k, "value": v} for k, v in found.items()]


def _read_db(db: Path) -> list[tuple[str, str]]:
    """Copy the DB aside and read it without needing the decryption key.

    Chromium stores values in an `encrypted_value` BLOB on macOS, but the
    plaintext `value` column is populated for many cookies and is enough on
    its own. If it is empty this returns nothing for that row rather than
    guessing at Keychain access.
    """
    tmp = Path(tempfile.mkdtemp(prefix="mimo-cookies-"))
    target = tmp / "Cookies"
    try:
        shutil.copy2(db, target)
        con = sqlite3.connect(f"file:{target}?mode=ro", uri=True)
        try:
            cur = con.execute(
                "SELECT name, value FROM cookies "
                "WHERE host_key LIKE ? AND value IS NOT NULL AND value != ''",
                (f"%{DOMAIN_SUFFIX}%",),
            )
            return [(n, v) for n, v in cur.fetchall()]
        finally:
            con.close()
    except sqlite3.Error as exc:
        log(f"warning: could not read {db}: {exc}")
        return []
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def load_chrome_plaintext(browser: str) -> list[dict]:
    """Fall back to decrypting Chromium cookies via the Keychain.

    Chromium on macOS encrypts values with a key stored in the login Keychain
    under "<Browser> Safe Storage". This shells out to `security` to fetch it.
    """
    keychain_name = {
        "chrome": "Chrome Safe Storage",
        "edge": "Microsoft Edge Safe Storage",
        "brave": "Brave Safe Storage",
        "chromium": "Chromium Safe Storage",
    }.get(browser)
    if not keychain_name:
        return []

    try:
        pw = subprocess.run(
            ["security", "find-generic-password", "-w", "-s", keychain_name],
            capture_output=True, text=True, check=True, timeout=30,
        ).stdout.strip()
    except (subprocess.CalledProcessError, FileNotFoundError, subprocess.TimeoutExpired) as exc:
        log(f"warning: could not read the Keychain entry {keychain_name!r}: {exc}")
        return []

    if not pw:
        return []

    # Chromium derives the AES key with PBKDF2-HMAC-SHA1, 1003 iterations.
    try:
        from hashlib import pbkdf2_hmac
        from Crypto.Cipher import AES  # type: ignore
    except ImportError:
        log("warning: pycryptodome is not installed; skipping encrypted cookies")
        log("         pip3 install pycryptodome   (or use --cookie-string)")
        return []

    key = pbkdf2_hmac("sha1", pw.encode(), b"saltysalt", 1003, dklen=16)
    iv = b" " * 16

    root = Path(os.path.expanduser(BROWSER_PATHS[browser]))
    cbs = sorted(root.glob("*/Cookies"), key=lambda p: p.stat().st_mtime, reverse=True)

    out: dict[str, str] = {}
    for db in cbs:
        tmp = Path(tempfile.mkdtemp(prefix="mimo-enc-"))
        target = tmp / "Cookies"
        try:
            shutil.copy2(db, target)
            con = sqlite3.connect(f"file:{target}?mode=ro", uri=True)
            try:
                cur = con.execute(
                    "SELECT name, encrypted_value FROM cookies "
                    "WHERE host_key LIKE ? AND length(encrypted_value) > 0",
                    (f"%{DOMAIN_SUFFIX}%",),
                )
                for name, blob in cur.fetchall():
                    if name in out or not is_wanted(name):
                        continue
                    try:
                        plain = AES.new(key, AES.MODE_CBC, iv).decrypt(blob[3:])
                        plain = plain[:-plain[-1]]
                        if plain[:32].count(b"\x00") > 8:
                            # Chrome 80+ prefixes a SHA256 domain hash.
                            plain = plain[32:]
                        out[name] = plain.decode("utf-8", "replace")
                    except Exception:
                        continue
            finally:
                con.close()
        except sqlite3.Error:
            continue
        finally:
            shutil.rmtree(tmp, ignore_errors=True)
        if any(is_session_cookie(k) for k in out):
            break

    return [{"name": k, "value": v} for k, v in out.items()]


def new_client_token() -> str:
    return os.urandom(32).hex()


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    src = ap.add_mutually_exclusive_group(required=True)
    src.add_argument("--auto", action="store_true",
                     help="read cookies from a local browser profile")
    src.add_argument("--cookie-file", type=Path,
                     help="file containing a raw Cookie header value")
    src.add_argument("--cookie-string",
                     help="a raw Cookie header value on the command line")
    src.add_argument("--stdin", action="store_true",
                     help="read a raw Cookie header value from stdin")

    ap.add_argument("--browser", default="chrome", choices=sorted(BROWSER_PATHS),
                    help="which browser to read (default: chrome)")
    ap.add_argument("--label", default="mimo-1",
                    help="label for this session in the config")
    ap.add_argument("--out", type=Path, default=Path("config.json"),
                    help="config file to write (default: ./config.json)")
    ap.add_argument("--token", help="client token to use instead of generating one")
    ap.add_argument("--force", action="store_true",
                    help="overwrite an existing config file")
    args = ap.parse_args()

    if args.out.exists() and not args.force:
        fail(f"{args.out} already exists; pass --force to overwrite")

    if args.auto:
        log(f"reading cookies from {args.browser}...")
        cookies = extract_from_browser(args.browser)
        if not any(is_session_cookie(c["name"]) for c in cookies):
            log("trying encrypted cookie values via Keychain...")
            cookies = load_chrome_plaintext(args.browser) or cookies
    elif args.cookie_file:
        cookies = read_cookie_file(args.cookie_file)
    elif args.stdin:
        cookies = parse_cookie_string(sys.stdin.read())
    else:
        cookies = parse_cookie_string(args.cookie_string or "")

    if not cookies:
        fail("no cookies parsed")
    names = {c["name"] for c in cookies}
    if not any(is_session_cookie(n) for n in names):
        fail(
            "no serviceToken cookie found — that is the one that carries the "
            "login. Re-copy the full Cookie header from a logged-in session."
        )

    cfg = json.loads(json.dumps(CONFIG_TEMPLATE))  # deep copy
    cfg["client_tokens"] = [args.token or new_client_token()]
    cfg["upstream"]["sessions"] = [{"label": args.label, "cookies": cookies}]

    args.out.write_text(json.dumps(cfg, indent=2, ensure_ascii=False) + "\n",
                        encoding="utf-8")
    os.chmod(args.out, 0o600)

    log(f"wrote {args.out} (mode 600) with {len(cookies)} cookies: "
        f"{', '.join(sorted(names))}")
    print(cfg["client_tokens"][0])
    return 0


if __name__ == "__main__":
    sys.exit(main())
