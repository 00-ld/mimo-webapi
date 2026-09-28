// Package auth implements the "open the box and log in" onboarding flow.
//
// The relay needs a logged-in MiMo Studio *browser* session. Asking a user to
// copy cookies out of DevTools is a terrible first-run experience, so this
// package drives a dedicated Chrome profile instead:
//
//  1. Launch Chrome with a private --user-data-dir that belongs to the relay.
//  2. Point it at the MiMo Studio login page.
//  3. The user logs in however they like — password, QR code, SMS. The relay
//     never sees the credentials.
//  4. Poll the browser's own cookie store over the DevTools protocol until the
//     session cookie appears, then hand the cookies to the session pool.
//
// Only the DevTools protocol is used to read cookies, never the on-disk
// database: Chrome encrypts that with a Keychain key on macOS, and asking for
// it would produce a scary system prompt for no benefit.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mimowebapi/internal/config"
)

// DefaultLoginURL is where the wizard sends the browser.
const DefaultLoginURL = "https://aistudio.xiaomimimo.com/"

// ChatPageURL is the page whose load issues the anti-fraud cookie.
const ChatPageURL = "https://aistudio.xiaomimimo.com/#/c"

// Chrome binary locations, most specific first.
var chromeCandidates = []string{
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
	"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
	"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	"/usr/bin/google-chrome",
	"/usr/bin/chromium",
	"/usr/bin/chromium-browser",
	"/snap/bin/chromium",
}

// FindChrome locates a Chromium-family browser.
func FindChrome() (string, error) {
	if p := os.Getenv("MIMO_CHROME"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("MIMO_CHROME points at %q, which does not exist", p)
	}
	for _, p := range chromeCandidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	// Last resort: whatever is on PATH.
	for _, name := range []string{"google-chrome", "chromium", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no Chrome or Chromium found; set MIMO_CHROME to its path")
}

// cookiesForDomain reports the cookie names the backend needs. The session
// cookie carries an environment prefix, so it is matched by suffix.
func isSessionCookie(name string) bool {
	return name == "serviceToken" || strings.HasSuffix(name, "_serviceToken")
}

// Wanted decides which browser cookies are worth keeping. Sending the whole
// jar is unnecessary and clutters the config.
func Wanted(name string) bool {
	if isSessionCookie(name) {
		return true
	}
	for _, s := range []string{"userId", "cUserId", "passToken", "deviceId",
		"muid", "uLocale", "locale", "ph"} {
		if name == s || strings.HasSuffix(name, "_"+s) || strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// Flow drives one login attempt.
type Flow struct {
	chromePath string
	profileDir string
	port       int
	loginURL   string

	mu      sync.Mutex
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	started time.Time
	// openedChat records that the chat tab was already opened for this flow,
	// so the poll loop does not spawn a new tab every second.
	openedChat bool

	// status is the last thing the wizard should tell the user.
	status string
}

// Options configures a Flow.
type Options struct {
	// ProfileDir is the dedicated Chrome profile. It holds the live session
	// cookie, so it must stay private.
	ProfileDir string
	// Port is the DevTools port.
	Port int
	// LoginURL defaults to the MiMo Studio home page.
	LoginURL string
}

// New prepares a login flow. Nothing is launched until Start.
func New(opts Options) (*Flow, error) {
	chrome, err := FindChrome()
	if err != nil {
		return nil, err
	}
	if opts.ProfileDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		opts.ProfileDir = filepath.Join(home, ".mimowebapi", "chrome-profile")
	}
	if opts.Port == 0 {
		opts.Port = 19222
	}
	if opts.LoginURL == "" {
		opts.LoginURL = DefaultLoginURL
	}
	return &Flow{
		chromePath: chrome,
		profileDir: opts.ProfileDir,
		port:       opts.Port,
		loginURL:   opts.LoginURL,
		status:     "idle",
	}, nil
}

// ProfileDir reports where the browser profile lives.
func (f *Flow) ProfileDir() string { return f.profileDir }

// Status returns a human-readable state for the wizard UI.
func (f *Flow) Status() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *Flow) setStatus(s string) {
	f.mu.Lock()
	f.status = s
	f.mu.Unlock()
}

// Start launches Chrome against the profile and returns immediately.
//
// The browser is deliberately visible: the user has to interact with it to log
// in, and a headless window would be indistinguishable from nothing happening.
func (f *Flow) Start(ctx context.Context) error {
	f.mu.Lock()
	if f.cmd != nil {
		f.mu.Unlock()
		return errors.New("a login session is already running")
	}
	f.mu.Unlock()

	if err := os.MkdirAll(f.profileDir, 0o700); err != nil {
		return fmt.Errorf("create browser profile: %w", err)
	}

	childCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(childCtx, f.chromePath,
		"--user-data-dir="+f.profileDir,
		fmt.Sprintf("--remote-debugging-port=%d", f.port),
		// A dedicated profile should behave like a fresh browser, not inherit
		// whatever state a previous run left behind.
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=Translate,MediaRouter",
		// The relay only needs this one site; keeping the window small makes
		// it obvious that it is a setup step and not the user's real browser.
		"--window-size=1100,820",
		f.loginURL,
	)
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("launch browser: %w", err)
	}

	f.mu.Lock()
	f.cmd = cmd
	f.cancel = cancel
	f.started = time.Now()
	f.status = "waiting_for_login"
	f.mu.Unlock()

	// Reap the process so it does not become a zombie when the user closes it.
	go func() {
		_ = cmd.Wait()
		f.mu.Lock()
		if f.cmd == cmd {
			f.cmd = nil
			f.status = "browser_closed"
		}
		f.mu.Unlock()
	}()

	return nil
}

// Stop closes the browser and clears the session.
func (f *Flow) Stop() {
	f.mu.Lock()
	cancel, cmd := f.cancel, f.cmd
	f.cmd, f.cancel = nil, nil
	f.status = "idle"
	f.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// Running reports whether the browser is still open.
func (f *Flow) Running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cmd != nil
}

// DevToolsURL is where the wizard can inspect the session.
func (f *Flow) DevToolsURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", f.port)
}

// Verifier checks whether a candidate session actually works. It is injected
// so the auth package does not depend on the upstream client, and so tests can
// substitute a fake.
type Verifier func(ctx context.Context, s config.Session) error

// WaitForLogin polls the browser's cookie jar until a session that really
// works appears.
//
// Presence of the right cookie names is not proof of a usable session: Xiaomi
// writes `serviceToken` while the login is still settling, and a token that
// looks complete can still be rejected by the chat endpoint. When a Verifier
// is provided the session is exercised against the real backend before being
// handed over, so the wizard reports success only when inference would work.
func (f *Flow) WaitForLogin(ctx context.Context, timeout time.Duration) (config.Session, error) {
	return f.waitForLogin(ctx, timeout, nil)
}

// WaitForVerifiedLogin waits until the captured session passes verify.
func (f *Flow) WaitForVerifiedLogin(ctx context.Context, timeout time.Duration, verify Verifier) (config.Session, error) {
	return f.waitForLogin(ctx, timeout, verify)
}

func (f *Flow) waitForLogin(ctx context.Context, timeout time.Duration, verify Verifier) (config.Session, error) {
	deadline := time.Now().Add(timeout)
	f.setStatus("waiting_for_login")

	var lastProblem string
	for {
		if time.Now().After(deadline) {
			f.setStatus("timed_out")
			return config.Session{}, fmt.Errorf(
				"timed out after %s waiting for login%s", timeout, lastProblem)
		}
		select {
		case <-ctx.Done():
			f.setStatus("cancelled")
			return config.Session{}, ctx.Err()
		case <-time.After(time.Second):
		}

		cookies, err := f.ReadCookies(ctx)
		if err != nil {
			lastProblem = ": " + err.Error()
			continue
		}
		if sess, ok := sessionFrom(cookies); ok {
			if verify == nil {
				f.setStatus("logged_in")
				return sess, nil
			}
			// A token that is present but not yet accepted would produce a
			// relay that fails every request, so confirm it end to end first.
			f.setStatus("verifying")
			if err := verify(ctx, sess); err == nil {
				f.setStatus("logged_in")
				return sess, nil
			} else {
				lastProblem = ": the captured login is not accepted by MiMo yet (" +
					err.Error() + ")"
			}
		}
		if hasSessionCookie(cookies) {
			// The login landed but the anti-fraud cookie is only issued on
			// the chat page. Open it once rather than making the user work
			// out which page to visit.
			if !f.openedChat {
				f.openedChat = true
				_ = f.OpenTab(ctx, ChatPageURL)
			}
			lastProblem = ": found the session cookie but not the anti-fraud " +
				"cookie (missing " + strings.Join(missingCookies(cookies), ", ") +
				"); send one message in the chat tab that just opened"
			f.setStatus("waiting_for_chat_visit")
		} else {
			lastProblem = ""
		}
	}
}

// missingCookies reports which of the required cookies have not arrived yet.
//
// Without this the wizard can only say "still waiting", which is impossible to
// act on: the user cannot tell whether the login is mid-flight or a cookie was
// never issued. Naming the gap makes the wizard self-diagnosing.
func missingCookies(cookies []config.Cookie) []string {
	have := map[string]bool{}
	for _, c := range cookies {
		have[c.Name] = true
	}
	var missing []string
	for _, want := range []string{"xiaomichatbot_serviceToken", "xiaomichatbot_ph"} {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return missing
}

func hasSessionCookie(cookies []config.Cookie) bool {
	for _, c := range cookies {
		if isSessionCookie(c.Name) {
			return true
		}
	}
	return false
}

// sessionFrom builds a usable session when the cookies are sufficient.
func sessionFrom(cookies []config.Cookie) (config.Session, bool) {
	var hasSession, hasPH bool
	var userID string
	for _, c := range cookies {
		switch {
		case isSessionCookie(c.Name):
			hasSession = true
		case c.Name == "ph" || strings.HasSuffix(c.Name, "_ph"):
			hasPH = true
		case c.Name == "userId" || strings.HasSuffix(c.Name, "_userId"):
			userID = c.Value
		}
	}
	if !hasSession || !hasPH {
		return config.Session{}, false
	}
	label := "mimo"
	if userID != "" {
		label = "mimo-" + userID
	}
	return config.Session{Label: label, Cookies: cookies}, true
}

// unwrapQuotes strips the surrounding double quotes Xiaomi's own cookies carry.
//
// The account service stores `serviceToken` and `ph` as a *quoted* token — the
// quotes are part of the stored string, not a display artefact. Forwarding
// them makes the backend reject the request: it echoes the value back in its
// 401 loginUrl as `ph=%22...%22` and treats it as a different cookie. Every
// other cookie in the same jar is unquoted, which is what makes this easy to
// miss.
func unwrapQuotes(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

// ReadCookies fetches the browser's cookies for the MiMo domains.
func (f *Flow) ReadCookies(ctx context.Context) ([]config.Cookie, error) {
	raw, err := f.cdpCall(ctx, "Network.getAllCookies", nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Cookies []struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Domain string `json:"domain"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode cookie list: %w", err)
	}

	var out []config.Cookie
	seen := map[string]bool{}
	for _, c := range res.Cookies {
		if !strings.Contains(c.Domain, "xiaomimimo.com") &&
			!strings.Contains(c.Domain, "xiaomi.com") {
			continue
		}
		if !Wanted(c.Name) || seen[c.Name] || c.Value == "" {
			continue
		}
		seen[c.Name] = true
		out = append(out, config.Cookie{
			Name:  c.Name,
			Value: unwrapQuotes(c.Value),
		})
	}
	if len(out) == 0 {
		return nil, errors.New("no MiMo cookies in the browser yet")
	}
	return out, nil
}

// OpenTab opens a new tab at the given URL.
//
// When the wizard has captured a login token but not the anti-fraud cookie,
// the user still has to visit the chat page once for the browser to receive
// it. Opening that tab removes the guesswork about where to click.
func (f *Flow) OpenTab(ctx context.Context, rawURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("http://127.0.0.1:%d/json/new?%s", f.port,
			url.QueryEscape(rawURL)), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("the browser refused to open a tab: %s", resp.Status)
	}
	return nil
}

// cdpCall issues one DevTools protocol command over a page target.
func (f *Flow) cdpCall(ctx context.Context, method string, params any) (json.RawMessage, error) {
	client, err := f.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer client.close()
	return client.call(method, params)
}
