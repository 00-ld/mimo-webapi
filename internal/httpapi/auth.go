package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mimowebapi/internal/auth"
	"mimowebapi/internal/config"
)

// authManager owns the single in-flight login flow.
//
// Only one login can run at a time: each one opens a browser window, and two
// windows fighting over the same profile would corrupt it.
type authManager struct {
	mu   sync.Mutex
	flow *auth.Flow
	// lastResult holds the session produced by the most recent successful
	// login, so the wizard can confirm it before committing.
	lastResult *config.Session
	lastErr    string
}

func newAuthManager() *authManager { return &authManager{} }

// authState is the JSON view the wizard polls.
type authState struct {
	Running   bool     `json:"running"`
	Status    string   `json:"status"`
	Detail    string   `json:"detail,omitempty"`
	Profile   string   `json:"profile_dir,omitempty"`
	DevTools  string   `json:"devtools_url,omitempty"`
	Chrome    string   `json:"chrome_path,omitempty"`
	Available bool     `json:"available"`
	Cookies   []string `json:"cookies,omitempty"`
	Session   string   `json:"session_label,omitempty"`
}

// handleAuth routes /admin/api/auth/*.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/api/auth")
	path = strings.TrimSuffix(path, "/")

	// The label-bearing route is handled before the switch: a switch on a
	// string cannot express a prefix-with-method condition.
	if r.Method == http.MethodDelete && strings.HasPrefix(path, "/sessions/") {
		s.authRemoveSession(w, r, strings.TrimPrefix(path, "/sessions/"))
		return
	}

	switch path {
	case "/status":
		s.authStatus(w, r)
	case "/start":
		s.authStart(w, r)
	case "/cancel":
		s.authCancel(w, r)
	case "/sessions":
		s.authSessions(w, r)
	case "/manual":
		s.authPasteCookies(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such auth route"})
	}
}

// authStatus reports the current state of the login flow.
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	s.account.mu.Lock()
	flow := s.account.flow
	lastErr := s.account.lastErr
	var last *config.Session
	if s.account.lastResult != nil {
		cp := *s.account.lastResult
		last = &cp
	}
	s.account.mu.Unlock()

	state := authState{Available: true, Status: "idle"}
	if _, err := auth.FindChrome(); err != nil {
		state.Available = false
		state.Detail = err.Error()
	}
	if flow != nil {
		state.Running = flow.Running()
		state.Status = flow.Status()
		state.Profile = flow.ProfileDir()
		state.DevTools = flow.DevToolsURL()
	}
	if lastErr != "" {
		state.Detail = lastErr
	}
	if last != nil {
		state.Session = last.Label
		for _, c := range last.Cookies {
			state.Cookies = append(state.Cookies, c.Name)
		}
	}
	writeJSON(w, http.StatusOK, state)
}

// authStart launches the browser and begins polling for a login.
func (s *Server) authStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use POST"})
		return
	}

	s.account.mu.Lock()
	if s.account.flow != nil && s.account.flow.Running() {
		s.account.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "a login window is already open",
		})
		return
	}
	s.account.lastErr = ""
	s.account.mu.Unlock()

	flow, err := auth.New(auth.Options{ProfileDir: s.profileDir()})
	if err != nil {
		s.setAuthError(err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}

	ctx, cancel := contextWithTimeout(10 * time.Minute)
	if err := flow.Start(ctx); err != nil {
		cancel()
		s.setAuthError(err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	s.account.mu.Lock()
	s.account.flow = flow
	s.account.mu.Unlock()

	// Wait for the login in the background so the HTTP call returns at once.
	go func() {
		defer cancel()
		// Confirm the captured session really works before reporting success:
		// a token can be present in the browser and still be rejected, and a
		// relay that authorises "successfully" then fails every request is
		// worse than one that keeps waiting.
		verify := func(vctx context.Context, cand config.Session) error {
			return s.client.Probe(vctx, cand)
		}
		sess, err := flow.WaitForVerifiedLogin(ctx, 9*time.Minute, verify)
		s.account.mu.Lock()
		defer s.account.mu.Unlock()
		if err != nil {
			s.account.lastErr = err.Error()
			s.account.flow = nil
			flow.Stop()
			return
		}
		s.account.lastResult = &sess
		s.account.lastErr = ""
		s.account.flow = nil

		// Put the account to work immediately and remember it, so a restart
		// does not throw away a login the user just performed.
		s.client.Pool().Upsert(sess)
		if err := s.persistSession(sess); err != nil {
			s.log.Error("could not persist the authorised session", "error", err)
		}
		s.log.Info("account authorised", "label", sess.Label,
			"cookies", len(sess.Cookies))
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"status": "waiting_for_login",
		"detail": "A browser window has opened. Log in to MiMo there; " +
			"this page will update by itself.",
	})
}

// authPasteCookies accepts a pasted Cookie header.
//
// The wizard is the happy path, but it needs a desktop browser on the same
// machine. On a headless server there is no browser to drive, so this endpoint
// takes the cookie string copied from DevTools instead.
func (s *Server) authPasteCookies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use POST"})
		return
	}
	body, err := readBody(w, r, 256<<10)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	var in struct {
		Cookie string `json:"cookie"`
		Label  string `json:"label"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}

	cookies := ParseCookieString(in.Cookie)
	if len(cookies) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "no cookies found in that string",
			"hint": "paste the whole Cookie header, e.g. " +
				"xiaomichatbot_serviceToken=...; userId=...; xiaomichatbot_ph=...",
		})
		return
	}

	label := strings.TrimSpace(in.Label)
	if label == "" {
		label = s.cfg.Admin.SessionLabel
	}
	sess := config.Session{Label: label, Cookies: cookies}

	complete := false
	for _, c := range cookies {
		if c.Name == "serviceToken" || strings.HasSuffix(c.Name, "_serviceToken") {
			complete = true
		}
	}

	s.client.Pool().Upsert(sess)
	if err := s.persistSession(sess); err != nil {
		s.log.Error("could not persist the pasted session", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "session accepted in memory but could not be saved: " + err.Error(),
		})
		return
	}
	s.log.Info("session added by hand", "label", sess.Label, "cookies", len(cookies))

	resp := map[string]any{"ok": true, "label": sess.Label, "count": len(cookies)}
	if !complete {
		resp["warn"] = "no service token was recognised in that string; " +
			"requests may still fail with 401"
	}
	writeJSON(w, http.StatusOK, resp)
}

// ParseCookieString pulls name=value pairs out of a pasted Cookie header.
//
// It tolerates the shapes people actually paste: a full `Cookie:` header, the
// quoted form a browser devtools panel shows, and stray whitespace or trailing
// semicolons.
func ParseCookieString(raw string) []config.Cookie {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if i := strings.IndexByte(raw, ':'); i > 0 &&
		strings.EqualFold(strings.TrimSpace(raw[:i]), "cookie") {
		raw = raw[i+1:]
	}

	var out []config.Cookie
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		// Strip the surrounding quotes a devtools panel adds for display;
		// they are not part of the value.
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		if name == "" || value == "" || seen[name] {
			continue
		}
		// Xiaomi's own cookies arrive quoted; the upstream rejects the quotes.
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		if value == "" {
			continue
		}
		seen[name] = true
		out = append(out, config.Cookie{Name: name, Value: value})
	}
	return out
}

func (s *Server) authCancel(w http.ResponseWriter, r *http.Request) {
	s.account.mu.Lock()
	flow := s.account.flow
	s.account.flow = nil
	s.account.mu.Unlock()
	if flow != nil {
		flow.Stop()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// authSessions lists the accounts currently in rotation.
func (s *Server) authSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"label":    s.cfg.Admin.SessionLabel,
		"profile":  s.profileDir(),
		"sessions": s.client.SessionStatuses(),
	})
}

// authRemoveSession drops an account from the pool and from disk.
//
// Without this the only way to correct a mistyped cookie was to edit
// sessions.json by hand and restart, which is exactly the kind of step an
// operator gets wrong at the worst moment.
func (s *Server) authRemoveSession(w http.ResponseWriter, r *http.Request, label string) {
	label = strings.TrimSpace(label)
	if label == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing label"})
		return
	}

	// Check that the label exists before reasoning about pool size: a request
	// for an unknown account should say so, not complain about the last one.
	exists := false
	for _, st := range s.client.SessionStatuses() {
		if st.Label == label {
			exists = true
			break
		}
	}
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such account"})
		return
	}

	// Refuse to empty the pool. A relay with no account answers every request
	// with an error, and the operator is usually removing the wrong entry when
	// they get here.
	if s.client.Pool().Size() <= 1 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "this is the only account; add another before removing it",
		})
		return
	}

	if !s.client.Pool().Remove(label) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such account"})
		return
	}
	if err := s.removePersistedSession(label); err != nil {
		// The pool already dropped it, so the in-memory state is what the
		// caller asked for. Report the persistence problem without undoing it.
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "removed": label,
			"warn": "removed from the running pool, but the saved file could not be updated: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": label})
}

// removePersistedSession deletes one label from the session file, leaving the
// others untouched.
func (s *Server) removePersistedSession(label string) error {
	path := s.sessionFile()

	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f struct {
		Version  int              `json:"version"`
		Sessions []config.Session `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}

	kept := make([]config.Session, 0, len(f.Sessions))
	for _, sess := range f.Sessions {
		if sess.Label != label {
			kept = append(kept, sess)
		}
	}
	if len(kept) == len(f.Sessions) {
		return nil
	}
	if f.Version == 0 {
		f.Version = 1
	}
	out, err := json.MarshalIndent(map[string]any{
		"version":  f.Version,
		"sessions": kept,
	}, "", "  ")
	if err != nil {
		return err
	}
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer fh.Close()
	if _, err := fh.Write(out); err != nil {
		return err
	}
	return fh.Sync()
}

func (s *Server) setAuthError(err error) {
	s.account.mu.Lock()
	s.account.lastErr = err.Error()
	s.account.mu.Unlock()
}

// profileDir is where the dedicated browser profile lives.
func (s *Server) profileDir() string {
	if s.cfg.Admin.BrowserProfileDir != "" {
		return s.cfg.Admin.BrowserProfileDir
	}
	// Keep it next to the config so a portable install stays self-contained.
	base := filepath.Dir(s.cfg.Admin.KeyStorePath)
	if base == "." || base == "" {
		base = "."
	}
	return filepath.Join(base, "chrome-profile")
}

// sessionFile is where an authorised session is stored for restarts.
func (s *Server) sessionFile() string {
	return s.cfg.SessionFile()
}

// persistSession writes an authorised session to disk so it survives a
// restart without the user logging in again.
//
// The file holds live credentials, so it is written 0600.
func (s *Server) persistSession(sess config.Session) error {
	path := s.sessionFile()

	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()

	existing := map[string]config.Session{}
	if raw, err := os.ReadFile(path); err == nil {
		var f struct {
			Sessions []config.Session `json:"sessions"`
		}
		if json.Unmarshal(raw, &f) == nil {
			for _, s := range f.Sessions {
				existing[s.Label] = s
			}
		}
	}
	existing[sess.Label] = sess

	out := make([]config.Session, 0, len(existing))
	for _, v := range existing {
		out = append(out, v)
	}
	raw, err := json.MarshalIndent(map[string]any{
		"version":  1,
		"sessions": out,
	}, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return err
	}
	return f.Sync()
}

// LoadPersistedSessions reads sessions saved by a previous authorisation.
func LoadPersistedSessions(path string) ([]config.Session, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f struct {
		Sessions []config.Session `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	// Drop anything that would not survive validation, rather than letting a
	// corrupt entry break startup.
	out := make([]config.Session, 0, len(f.Sessions))
	for _, s := range f.Sessions {
		if len(s.Cookies) > 0 {
			out = append(out, s)
		}
	}
	return out, nil
}

// errNoChrome is returned when the wizard cannot find a browser.
var errNoChrome = errors.New("no Chrome or Chromium found")
