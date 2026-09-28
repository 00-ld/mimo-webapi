package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mimowebapi/internal/apikeys"
)

// Session cookie name for the admin console.
const adminCookie = "mimo_admin"

// handleAdmin routes the management API.
//
// Every route below /admin/api requires a valid admin session. The session is
// a signed token rather than the raw password, so the password itself is never
// stored in a browser cookie.
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		path = "/"
	}

	switch path {
	case "/login":
		s.adminLogin(w, r)
		return
	case "/logout":
		s.adminLogout(w, r)
		return
	case "/session":
		// Reports whether the caller already holds a valid session.
		if !s.adminAuthorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if !s.adminAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "admin authentication required",
		})
		return
	}

	if strings.HasPrefix(path, "/auth") {
		s.handleAuth(w, r)
		return
	}

	switch {
	case path == "/keys" && r.Method == http.MethodGet:
		s.adminListKeys(w, r)
	case path == "/keys" && r.Method == http.MethodPost:
		s.adminCreateKey(w, r)
	case strings.HasPrefix(path, "/keys/") && r.Method == http.MethodPatch:
		s.adminUpdateKey(w, r, strings.TrimPrefix(path, "/keys/"))
	case strings.HasPrefix(path, "/keys/") && r.Method == http.MethodDelete:
		s.adminDeleteKey(w, r, strings.TrimPrefix(path, "/keys/"))
	case strings.HasSuffix(path, "/rotate") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/keys/"), "/rotate")
		s.adminRotateKey(w, r, id)
	case strings.HasSuffix(path, "/reset-usage") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/keys/"), "/reset-usage")
		s.adminResetUsage(w, r, id)
	case path == "/status" && r.Method == http.MethodGet:
		s.adminStatus(w, r)
	case path == "/models" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"models": s.cfg.Models.List})
	case path == "/agents" && r.Method == http.MethodGet:
		s.handleAgents(w, r)
	case strings.HasPrefix(path, "/agents/") && strings.HasSuffix(path, "/configure"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/agents/"), "/configure")
		s.handleAgentConfigure(w, r, id)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such admin route"})
	}
}

// ---- session handling ------------------------------------------------------

// adminLogin exchanges the admin password for a signed session cookie.
func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use POST"})
		return
	}
	if s.cfg.Admin.Password == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "admin.password is not configured",
		})
		return
	}
	body, err := readBody(w, r, 64<<10)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if !constTimeEqual(s.cfg.Admin.Password, in.Password) {
		// A small fixed delay blunts online guessing without making the
		// endpoint a denial-of-service lever.
		time.Sleep(300 * time.Millisecond)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "wrong password"})
		return
	}

	exp := time.Now().Add(12 * time.Hour)
	token := s.signSession(exp)
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Value:    token,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Secure is intentionally not set: the console is normally reached
		// over plain http on localhost. Put it behind TLS and add Secure if
		// you expose it.
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": exp.Unix()})
}

func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// signSession builds "expiry.hmac" so the cookie cannot be forged or extended.
func (s *Server) signSession(exp time.Time) string {
	payload := strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, s.sessionKey())
	mac.Write([]byte("admin-session:" + payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload + "." + sig))
}

func (s *Server) verifySession(raw string) bool {
	dec, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return false
	}
	payload, sig, found := strings.Cut(string(dec), ".")
	if !found {
		return false
	}
	mac := hmac.New(sha256.New, s.sessionKey())
	mac.Write([]byte("admin-session:" + payload))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return false
	}
	exp, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < exp
}

// sessionKey derives the signing key from the admin password, so rotating the
// password invalidates every existing console session.
func (s *Server) sessionKey() []byte {
	sum := sha256.Sum256([]byte("mimowebapi-session-v1|" + s.cfg.Admin.Password))
	return sum[:]
}

func (s *Server) adminAuthorized(r *http.Request) bool {
	if s.cfg.Admin.Password == "" {
		return false
	}
	// A bearer admin password is accepted for scripting the API.
	if tok := extractToken(r); tok != "" && constTimeEqual(tok, s.cfg.Admin.Password) {
		return true
	}
	ck, err := r.Cookie(adminCookie)
	if err != nil {
		return false
	}
	return s.verifySession(ck.Value)
}

// ---- key management --------------------------------------------------------

func (s *Server) adminListKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"keys": s.keys.List()})
}

func (s *Server) adminCreateKey(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r, 64<<10)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	var in struct {
		Name         string `json:"name"`
		QuotaTokens  *int64 `json:"quota_tokens"`
		QuotaReqs    *int64 `json:"quota_requests"`
		RateLimitRPM *int   `json:"rate_limit_rpm"`
		TTLHours     *int   `json:"ttl_hours"`
		Note         string `json:"note"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}

	quotaTokens := s.cfg.Admin.DefaultQuotaTokens
	if in.QuotaTokens != nil {
		quotaTokens = *in.QuotaTokens
	}
	quotaReqs := s.cfg.Admin.DefaultQuotaReqs
	if in.QuotaReqs != nil {
		quotaReqs = *in.QuotaReqs
	}
	rateRPM := s.cfg.Admin.DefaultRateRPM
	if in.RateLimitRPM != nil {
		rateRPM = *in.RateLimitRPM
	}
	var ttl time.Duration
	if in.TTLHours != nil && *in.TTLHours > 0 {
		ttl = time.Duration(*in.TTLHours) * time.Hour
	}

	secret, key, err := s.keys.Create(in.Name, quotaTokens, quotaReqs, rateRPM, ttl, in.Note)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// The plaintext secret appears here and nowhere else, ever.
	writeJSON(w, http.StatusOK, map[string]any{
		"secret": secret,
		"key":    key,
		"note":   "This is the only time the secret is shown. Copy it now.",
	})
}

func (s *Server) adminUpdateKey(w http.ResponseWriter, r *http.Request, id string) {
	body, err := readBody(w, r, 64<<10)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	var in struct {
		Name         *string `json:"name"`
		Enabled      *bool   `json:"enabled"`
		QuotaTokens  *int64  `json:"quota_tokens"`
		QuotaReqs    *int64  `json:"quota_requests"`
		RateLimitRPM *int    `json:"rate_limit_rpm"`
		Note         *string `json:"note"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	k, err := s.keys.Update(id, in.Name, in.Enabled, in.QuotaTokens, in.QuotaReqs,
		in.RateLimitRPM, in.Note)
	if err != nil {
		s.writeKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": k})
}

func (s *Server) adminDeleteKey(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.keys.Delete(id); err != nil {
		s.writeKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) adminRotateKey(w http.ResponseWriter, r *http.Request, id string) {
	secret, key, err := s.keys.Rotate(id)
	if err != nil {
		s.writeKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"secret": secret, "key": key,
		"note": "The previous secret stopped working immediately.",
	})
}

func (s *Server) adminResetUsage(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.keys.ResetUsage(id); err != nil {
		s.writeKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) adminStatus(w http.ResponseWriter, r *http.Request) {
	keys := s.keys.List()
	var totalReq, totalTokens int64
	active := 0
	for _, k := range keys {
		totalReq += k.UsedRequests
		totalTokens += k.UsedTokens
		if k.Enabled && !k.Exhausted {
			active++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"keys_total":     len(keys),
		"keys_active":    active,
		"total_requests": totalReq,
		"total_tokens":   totalTokens,
		"sessions":       s.client.SessionStatuses(),
		"models":         s.cfg.Models.List,
		"default_model":  s.cfg.Models.Default,
		"upstream":       s.cfg.Upstream.BaseURL,
	})
}

func (s *Server) writeKeyError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, apikeys.ErrNotFound) {
		status = http.StatusNotFound
	}
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

// keyAuthError maps a key store rejection onto an HTTP status.
func keyAuthError(err error) (int, string, string) {
	switch {
	case errors.Is(err, apikeys.ErrQuota):
		return http.StatusTooManyRequests, "insufficient_quota",
			"This API key has exhausted its quota."
	case errors.Is(err, apikeys.ErrRateLimit):
		return http.StatusTooManyRequests, "rate_limit_exceeded",
			"Too many requests for this API key. Slow down."
	case errors.Is(err, apikeys.ErrDisabled):
		return http.StatusForbidden, "key_disabled", "This API key is disabled or expired."
	default:
		return http.StatusUnauthorized, "invalid_api_key", "Incorrect API key provided."
	}
}

// contextWithTimeout is a small helper so the auth flow reads cleanly.
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// describeKey renders a key for a log line without exposing the secret.
func describeKey(k *apikeys.Key) string {
	if k == nil {
		return "anonymous"
	}
	return fmt.Sprintf("%s(%s)", k.Name, k.Prefix)
}
