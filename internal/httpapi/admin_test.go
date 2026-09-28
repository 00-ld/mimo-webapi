package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const adminPW = "super-secret-admin-password"

func adminServer(t *testing.T) *Server {
	t.Helper()
	cfg := testConfig("https://example.invalid")
	cfg.Admin.Password = adminPW
	s, err := buildServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// adminCall performs a request with the admin bearer password.
func adminCall(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+adminPW)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestAdminRequiresAuth(t *testing.T) {
	s := adminServer(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/admin/api/keys"},
		{"POST", "/admin/api/keys"},
		{"GET", "/admin/api/status"},
	} {
		rec := doJSON(t, s.Routes(), tc.method, tc.path, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestAdminLoginFlow(t *testing.T) {
	s := adminServer(t)
	routes := s.Routes()

	// Wrong password is rejected.
	rec := doJSON(t, routes, http.MethodPost, "/admin/api/login",
		`{"password":"wrong"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: status = %d", rec.Code)
	}

	// Right password sets a session cookie.
	rec = doJSON(t, routes, http.MethodPost, "/admin/api/login",
		`{"password":"`+adminPW+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status = %d body = %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a session cookie")
	}
	var session *http.Cookie
	for _, c := range cookies {
		if c.Name == adminCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("session cookie missing")
	}
	if !session.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	// The cookie must not contain the password itself.
	if strings.Contains(session.Value, adminPW) {
		t.Error("session cookie leaks the password")
	}

	// The session grants access without the bearer password.
	req := httptest.NewRequest(http.MethodGet, "/admin/api/keys", nil)
	req.AddCookie(session)
	rec2 := httptest.NewRecorder()
	routes.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("session not accepted: %d", rec2.Code)
	}
}

// A forged session value must not be accepted.
func TestAdminSessionCannotBeForged(t *testing.T) {
	s := adminServer(t)
	routes := s.Routes()

	for _, forged := range []string{
		"9999999999.deadbeef",
		"garbage",
		"",
	} {
		req := httptest.NewRequest(http.MethodGet, "/admin/api/keys", nil)
		req.AddCookie(&http.Cookie{Name: adminCookie, Value: forged})
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("forged session %q accepted with status %d", forged, rec.Code)
		}
	}
}

func TestAdminCreateAndListKey(t *testing.T) {
	s := adminServer(t)
	routes := s.Routes()

	rec := adminCall(t, routes, http.MethodPost, "/admin/api/keys",
		`{"name":"alice","quota_requests":10,"rate_limit_rpm":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Secret string `json:"secret"`
		Key    struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Secret, "sk-mimo-") {
		t.Fatalf("secret = %q", created.Secret)
	}

	// The secret works on the public API.
	pub := doJSONWithToken(t, routes, http.MethodGet, "/v1/models", created.Secret)
	if pub.Code != http.StatusOK {
		t.Fatalf("issued key rejected by the public API: %d %s",
			pub.Code, pub.Body.String())
	}

	// Listing must not re-expose the secret.
	rec = adminCall(t, routes, http.MethodGet, "/admin/api/keys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), created.Secret) {
		t.Fatal("the key listing leaked a plaintext secret")
	}
	if !strings.Contains(rec.Body.String(), "alice") {
		t.Error("listing does not include the created key")
	}
}

func TestAdminUpdateDisableAndDelete(t *testing.T) {
	s := adminServer(t)
	routes := s.Routes()

	rec := adminCall(t, routes, http.MethodPost, "/admin/api/keys", `{"name":"bob"}`)
	var created struct {
		Secret string `json:"secret"`
		Key    struct {
			ID string `json:"id"`
		} `json:"key"`
	}
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created.Key.ID

	// Disable: the public API must stop accepting it.
	rec = adminCall(t, routes, http.MethodPatch, "/admin/api/keys/"+id,
		`{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	pub := doJSONWithToken(t, routes, http.MethodGet, "/v1/models", created.Secret)
	if pub.Code != http.StatusForbidden {
		t.Errorf("disabled key returned %d, want 403", pub.Code)
	}

	// Delete.
	rec = adminCall(t, routes, http.MethodDelete, "/admin/api/keys/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	pub = doJSONWithToken(t, routes, http.MethodGet, "/v1/models", created.Secret)
	if pub.Code != http.StatusUnauthorized {
		t.Errorf("deleted key returned %d, want 401", pub.Code)
	}
}

func TestAdminRotateReturnsNewSecret(t *testing.T) {
	s := adminServer(t)
	routes := s.Routes()

	rec := adminCall(t, routes, http.MethodPost, "/admin/api/keys", `{"name":"carol"}`)
	var created struct {
		Secret string `json:"secret"`
		Key    struct {
			ID string `json:"id"`
		} `json:"key"`
	}
	json.Unmarshal(rec.Body.Bytes(), &created)

	rec = adminCall(t, routes, http.MethodPost,
		"/admin/api/keys/"+created.Key.ID+"/rotate", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	var rotated struct {
		Secret string `json:"secret"`
	}
	json.Unmarshal(rec.Body.Bytes(), &rotated)
	if rotated.Secret == created.Secret {
		t.Fatal("rotate returned the same secret")
	}
	if doJSONWithToken(t, routes, http.MethodGet, "/v1/models",
		created.Secret).Code != http.StatusUnauthorized {
		t.Error("old secret still accepted after rotation")
	}
	if doJSONWithToken(t, routes, http.MethodGet, "/v1/models",
		rotated.Secret).Code != http.StatusOK {
		t.Error("new secret rejected")
	}
}

func TestAdminStatusCounts(t *testing.T) {
	s := adminServer(t)
	routes := s.Routes()
	adminCall(t, routes, http.MethodPost, "/admin/api/keys", `{"name":"a"}`)
	adminCall(t, routes, http.MethodPost, "/admin/api/keys", `{"name":"b"}`)

	rec := adminCall(t, routes, http.MethodGet, "/admin/api/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var st struct {
		KeysTotal  int `json:"keys_total"`
		KeysActive int `json:"keys_active"`
	}
	json.Unmarshal(rec.Body.Bytes(), &st)
	if st.KeysTotal != 2 || st.KeysActive != 2 {
		t.Errorf("status = %+v", st)
	}
}

func TestConsolePageIsServed(t *testing.T) {
	s := adminServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("console status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<!doctype html") {
		t.Error("console did not return HTML")
	}
	if !strings.Contains(body, "MiMo") {
		t.Error("console content looks wrong")
	}
	// The page must not embed the admin password.
	if strings.Contains(body, adminPW) {
		t.Fatal("console page embeds the admin password")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp == "" {
		t.Error("console is missing a CSP header")
	}
}

// Without an admin password the console is not mounted at all.
func TestConsoleDisabledWithoutPassword(t *testing.T) {
	cfg := testConfig("https://example.invalid")
	cfg.Admin.Console = true
	cfg.Admin.Password = ""
	s, err := buildServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console", nil))
	// It falls through to the JSON root handler rather than serving the page.
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "<!doctype") {
		t.Error("console served without an admin password")
	}
}

// A static client_token must keep working alongside issued keys.
func TestStaticTokenStillWorks(t *testing.T) {
	s := adminServer(t)
	if doJSON(t, s.Routes(), http.MethodGet, "/v1/models", "").Code != http.StatusOK {
		t.Error("static client token stopped working after the key store was added")
	}
}

// ---- authorisation wizard -------------------------------------------------

func TestAuthStatusIsIdleInitially(t *testing.T) {
	s := adminServer(t)
	rec := adminCall(t, s.Routes(), http.MethodGet, "/admin/api/auth/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var st authState
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Running {
		t.Error("a login flow is running before anything was started")
	}
	if st.Status != "idle" {
		t.Errorf("status = %q", st.Status)
	}
}

func TestAuthRoutesRequireAdmin(t *testing.T) {
	s := adminServer(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/admin/api/auth/status"},
		{"POST", "/admin/api/auth/start"},
		{"POST", "/admin/api/auth/cancel"},
		{"POST", "/admin/api/auth/manual"},
	} {
		rec := doJSON(t, s.Routes(), tc.method, tc.path, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

// Pasting a complete cookie string must register a usable account.
func TestAuthManualAcceptsCookies(t *testing.T) {
	s := adminServer(t)
	routes := s.Routes()

	rec := adminCall(t, routes, http.MethodPost, "/admin/api/auth/manual",
		`{"cookie":"xiaomichatbot_serviceToken=tok; userId=7; xiaomichatbot_ph=abc","label":"acct"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Label string `json:"label"`
		Count int    `json:"count"`
		Warn  string `json:"warn"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.OK || resp.Count != 3 {
		t.Errorf("resp = %+v", resp)
	}
	if resp.Label != "acct" {
		t.Errorf("label = %q", resp.Label)
	}
	if resp.Warn != "" {
		t.Errorf("a complete cookie set should not warn: %s", resp.Warn)
	}

	// The account must land in the pool.
	rec = adminCall(t, routes, http.MethodGet, "/admin/api/auth/sessions", "")
	var ss struct {
		Sessions []struct {
			Label string `json:"label"`
		} `json:"sessions"`
	}
	json.Unmarshal(rec.Body.Bytes(), &ss)
	found := false
	for _, x := range ss.Sessions {
		if x.Label == "acct" {
			found = true
		}
	}
	if !found {
		t.Errorf("the pasted account was not registered: %+v", ss.Sessions)
	}
}

// An incomplete cookie string is still accepted but must warn, rather than
// silently registering an account that will fail every request.
func TestAuthManualWarnsOnIncompleteCookies(t *testing.T) {
	s := adminServer(t)
	rec := adminCall(t, s.Routes(), http.MethodPost, "/admin/api/auth/manual",
		`{"cookie":"userId=7; deviceId=wb_1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Warn string `json:"warn"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Warn == "" {
		t.Error("an incomplete cookie set should warn the caller")
	}
}

func TestAuthManualRejectsEmpty(t *testing.T) {
	s := adminServer(t)
	rec := adminCall(t, s.Routes(), http.MethodPost, "/admin/api/auth/manual",
		`{"cookie":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestAuthManualUsesConfiguredDefaultLabel(t *testing.T) {
	s := adminServer(t)
	rec := adminCall(t, s.Routes(), http.MethodPost, "/admin/api/auth/manual",
		`{"cookie":"xiaomichatbot_serviceToken=t; xiaomichatbot_ph=p"}`)
	var resp struct {
		Label string `json:"label"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Label != s.cfg.Admin.SessionLabel {
		t.Errorf("label = %q, want the configured default %q",
			resp.Label, s.cfg.Admin.SessionLabel)
	}
}

// Authorising twice under the same label must replace, not duplicate.
func TestAuthManualReplacesSameLabel(t *testing.T) {
	s := adminServer(t)
	routes := s.Routes()
	for i := 0; i < 2; i++ {
		adminCall(t, routes, http.MethodPost, "/admin/api/auth/manual",
			`{"cookie":"xiaomichatbot_serviceToken=t; xiaomichatbot_ph=p","label":"same"}`)
	}
	rec := adminCall(t, routes, http.MethodGet, "/admin/api/auth/sessions", "")
	var ss struct {
		Sessions []struct {
			Label string `json:"label"`
		} `json:"sessions"`
	}
	json.Unmarshal(rec.Body.Bytes(), &ss)

	// The default test config already seeds one account, so count ours.
	n := 0
	for _, x := range ss.Sessions {
		if x.Label == "same" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("got %d sessions labelled %q, want 1 (re-authorising should replace)", n, "same")
	}
}

// Sessions survive a simulated restart through the on-disk store.
func TestSessionPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	cfg := testConfig("https://example.invalid")
	cfg.Admin.Password = adminPW
	cfg.Admin.KeyStorePath = filepath.Join(dir, "keys.json")
	// testConfig redirects storage into a tempdir of its own; point this back at
	// the directory the assertions below read from.
	cfg.Admin.SessionDir = dir
	s, err := buildServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	adminCall(t, s.Routes(), http.MethodPost, "/admin/api/auth/manual",
		`{"cookie":"xiaomichatbot_serviceToken=t; xiaomichatbot_ph=p","label":"kept"}`)

	loaded, err := LoadPersistedSessions(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Label != "kept" {
		t.Fatalf("loaded = %+v", loaded)
	}
	if len(loaded[0].Cookies) != 2 {
		t.Errorf("cookies = %d", len(loaded[0].Cookies))
	}

	// The file holds live credentials, so it must not be world-readable.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("sessions.json mode = %o, want 600", perm)
	}
}

func TestLoadPersistedSessionsMissingFile(t *testing.T) {
	got, err := LoadPersistedSessions(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("a missing file is not an error: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v", got)
	}
}

func TestLoadPersistedSessionsSkipsEmptyEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	os.WriteFile(path, []byte(`{"sessions":[
		{"label":"good","cookies":[{"name":"a","value":"b"}]},
		{"label":"empty","cookies":[]}
	]}`), 0o600)

	got, err := LoadPersistedSessions(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Label != "good" {
		t.Errorf("got %+v; entries without cookies should be dropped", got)
	}
}

func doJSONWithToken(t *testing.T, h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// A session arrives as a Cookie header, a curl flag, or a bare string. All
// three are accepted because all three are what people actually paste.
func TestManualAcceptsEveryPasteShape(t *testing.T) {
	body := `xiaomichatbot_serviceToken="/vjQ"; userId=1; xiaomichatbot_ph="n3=="`
	for _, tc := range []struct{ name, cookie string }{
		{"bare header", body},
		{"prefixed", "Cookie: " + body},
		{"with spaces", "  " + body + "  "},
		{"trailing semicolon", body + ";"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := adminServer(t)
			rec := adminCall(t, s.Routes(), http.MethodPost, "/admin/api/auth/manual",
				`{"cookie":`+jsonString(tc.cookie)+`,"label":"x"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var resp struct {
				OK    bool `json:"ok"`
				Count int  `json:"count"`
			}
			json.Unmarshal(rec.Body.Bytes(), &resp)
			if !resp.OK || resp.Count < 3 {
				t.Errorf("got %+v, want 3 cookies accepted", resp)
			}
		})
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
