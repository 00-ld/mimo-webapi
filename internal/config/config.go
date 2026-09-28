// Package config loads and validates the relay configuration.
//
// Validation is deliberately strict: a misconfigured relay that starts and
// then fails per-request is worse than one that refuses to boot.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// Placeholder markers that must never survive into a live config. A cookie or
// token left as a template value is a working credential that anyone reading
// the file can use.
var placeholderMarkers = []string{
	"REPLACE", "PLACEHOLDER", "CHANGEME", "CHANGE_ME", "YOUR_", "EXAMPLE",
	"XXX", "TODO", "<", ">",
}

// weakPasswordMarkers are substrings that appear in passwords people copy from
// tutorials and never change. Length alone is a poor proxy for strength: this
// list is checked in addition to the length floor, not instead of it.
var weakPasswordMarkers = []string{
	"password", "passwd", "admin", "123456", "qwerty", "letmein",
	"changeme", "secret", "test", "demo", "sample", "default",
}

// Cookie is one browser cookie scraped from a logged-in MiMo Studio session.
type Cookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Session is one logged-in MiMo Studio account.
//
// Cookies are the entire credential: the web backend authenticates purely by
// session cookie, so whoever holds them holds the account.
type Session struct {
	Label   string   `json:"label"`
	Cookies []Cookie `json:"cookies"`
}

// Header returns the Cookie header value for this session.
func (s Session) Header() string {
	parts := make([]string, 0, len(s.Cookies))
	for _, c := range s.Cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// Config is the full relay configuration.
type Config struct {
	// Path is where this config was loaded from. It is not a file field; it
	// lets the server place generated files (keys, sessions) beside the
	// config instead of in whatever directory the process was started from.
	Path string `json:"-"`

	Listen        string   `json:"listen"`
	ClientTokens  []string `json:"client_tokens"`
	AllowNonLocal bool     `json:"allow_non_loopback_listen"`

	Upstream Upstream `json:"upstream"`
	Models   Models   `json:"models"`
	Behavior Behavior `json:"behavior"`
	Admin    Admin    `json:"admin"`
	Log      Log      `json:"log"`
}

// Admin configures the multi-tenant surface: the key store and the console.
type Admin struct {
	// Password guards the /admin API and the console UI. Leave empty to run
	// single-tenant (client_tokens only) with the console disabled.
	Password string `json:"password"`
	// KeyStorePath is where issued API keys are persisted.
	KeyStorePath string `json:"key_store_path"`
	// SessionDir holds sessions.json. Empty means "next to the config file",
	// which keeps a restart from depending on the process working directory.
	SessionDir string `json:"session_dir"`
	// AllowOpenRegistration lets anyone who knows the admin password mint a
	// key. With it false, only a pre-existing client_token can do so.
	AllowOpenRegistration bool `json:"allow_open_registration"`
	// DefaultQuotaTokens / DefaultQuotaReqs seed newly issued keys.
	DefaultQuotaTokens int64 `json:"default_quota_tokens"`
	DefaultQuotaReqs   int64 `json:"default_quota_requests"`
	DefaultRateRPM     int   `json:"default_rate_limit_rpm"`
	// Console enables the built-in web UI at /console.
	Console bool `json:"console"`
	// BrowserProfileDir is the dedicated Chrome profile used by the login
	// wizard. Empty means "<key_store_dir>/chrome-profile".
	BrowserProfileDir string `json:"browser_profile_dir"`
	// SessionLabel names the account added by the wizard.
	SessionLabel string `json:"session_label"`
}

// Upstream describes the MiMo Studio web backend.
type Upstream struct {
	BaseURL         string    `json:"base_url"`
	Sessions        []Session `json:"sessions"`
	ConnectTimeout  int       `json:"connect_timeout_seconds"`
	ResponseTimeout int       `json:"response_header_timeout_seconds"`
	MaxBodyBytes    int64     `json:"max_body_bytes"`
	// CooldownSeconds is how long a session is parked after the backend
	// reports it as banned or its cookies stop working.
	CooldownSeconds int `json:"cooldown_seconds"`
	// ClientID mimics the browser-side tracing parameter appended to every
	// request. Empty disables it.
	ClientID string `json:"client_id"`
	// UserAgent and Locale mirror what the real web client sends. They are
	// configurable because the backend occasionally gates on them.
	UserAgent string `json:"user_agent"`
	Locale    string `json:"locale"`
	TimeZone  string `json:"time_zone"`
}

// Models maps client-facing model names onto MiMo web model codes.
type Models struct {
	Default string            `json:"default"`
	List    []string          `json:"list"`
	Alias   map[string]string `json:"alias"`
	// Meta describes each advertised model. Aggregators such as sub2api show
	// context limits and capabilities in their model picker, and a listing
	// without them renders as an entry with no useful information.
	Meta map[string]ModelMeta `json:"meta,omitempty"`
}

// ModelMeta is the descriptive half of a model listing.
type ModelMeta struct {
	ContextLength int      `json:"context_length,omitempty"`
	MaxOutput     int      `json:"max_output,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	DisplayName   string   `json:"display_name,omitempty"`
}

// Behavior tunes how OpenAI requests are translated onto the web protocol.
type Behavior struct {
	// EnableThinkingDefault is used when the client does not ask for
	// reasoning explicitly.
	EnableThinkingDefault bool `json:"enable_thinking_default"`
	// WebSearchDefault is "auto", "enabled" or "disabled".
	WebSearchDefault string `json:"web_search_default"`
	// SystemPromptMode controls how OpenAI system messages are handled:
	//   "prepend" — folded into the first user turn (default, always works)
	//   "drop"    — discarded
	SystemPromptMode string `json:"system_prompt_mode"`
}

// Log controls verbosity.
type Log struct {
	Level string `json:"level"`
	Usage bool   `json:"usage"`
}

// Default returns a config with every field populated to a working default.
func Default() *Config {
	return &Config{
		Listen: "127.0.0.1:8793",
		Upstream: Upstream{
			BaseURL:         "https://aistudio.xiaomimimo.com",
			ConnectTimeout:  15,
			ResponseTimeout: 120,
			MaxBodyBytes:    16 << 20,
			CooldownSeconds: 300,
			UserAgent:       DefaultUserAgent,
			Locale:          "zh-CN",
			TimeZone:        "Asia/Shanghai",
		},
		Models: Models{
			Default: "mimo-v2.6-flash",
			List: []string{
				"mimo-v2.6-pro",
				"mimo-v2.6-flash",
				"mimo-v2.6-pro-ultraspeed-studio",
			},
			Alias: map[string]string{},
		},
		Behavior: Behavior{
			EnableThinkingDefault: true,
			WebSearchDefault:      "auto",
			SystemPromptMode:      "prepend",
		},
		Admin: Admin{
			KeyStorePath:     "keys.json",
			SessionLabel:     "mimo",
			Console:          true,
			DefaultRateRPM:   60,
			DefaultQuotaReqs: 0,
		},
		Log: Log{Level: "info", Usage: true},
	}
}

// DefaultUserAgent mimics the Chrome build the web client targets.
const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// Load reads a config file, applying environment overrides on top.
// SessionFile is where authorised accounts are persisted.
//
// This lives next to the config file rather than in the process working
// directory: a server started from elsewhere must still find the session it
// saved. It was previously computed in two places with the same collapse-to-"."
// bug, which let a test read a real user's credentials out of the cwd.
func (c *Config) SessionFile() string {
	if dir := c.Admin.SessionDir; dir != "" {
		return filepath.Join(dir, "sessions.json")
	}
	base := filepath.Dir(c.Admin.KeyStorePath)
	if base == "" || base == "." {
		if c.Path != "" {
			base = filepath.Dir(c.Path)
		} else {
			base = "."
		}
	}
	return filepath.Join(base, "sessions.json")
}

func Load(path string) (*Config, error) {
	cfg := Default()
	cfg.Path = path
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		// Unknown keys are a config mistake worth reporting, not something
		// to swallow: a typo'd key would otherwise silently keep its default.
		dec.DisallowUnknownFields()
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	applyEnv(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("MIMO_RELAY_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("MIMO_RELAY_CLIENT_TOKENS"); v != "" {
		cfg.ClientTokens = splitList(v)
	}
	if v := os.Getenv("MIMO_RELAY_BASE_URL"); v != "" {
		cfg.Upstream.BaseURL = v
	}
	if v := os.Getenv("MIMO_RELAY_COOKIES"); v != "" {
		cfg.Upstream.Sessions = parseSessionEnv(v)
	}
	if v := os.Getenv("MIMO_RELAY_DEFAULT_MODEL"); v != "" {
		cfg.Models.Default = v
	}
	if v := os.Getenv("MIMO_RELAY_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	if v := os.Getenv("MIMO_RELAY_ADMIN_PASSWORD"); v != "" {
		cfg.Admin.Password = v
	}
	if v := os.Getenv("MIMO_RELAY_KEY_STORE"); v != "" {
		cfg.Admin.KeyStorePath = v
	}
	if v := os.Getenv("MIMO_RELAY_SESSION_DIR"); v != "" {
		cfg.Admin.SessionDir = v
	}
	if v := os.Getenv("MIMO_RELAY_BROWSER_PROFILE_DIR"); v != "" {
		cfg.Admin.BrowserProfileDir = v
	}
	if v := os.Getenv("MIMO_RELAY_ALLOW_NON_LOOPBACK_LISTEN"); v != "" {
		cfg.AllowNonLocal = truthy(v)
	}
}

// truthy reads the spellings people actually put in a compose file.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseSessionEnv reads "label=name=value;name=value,label2=..." — the cookie
// string itself contains ';' and '=', so the label is split off before the
// first '=' only.
func parseSessionEnv(s string) []Session {
	var out []Session
	for _, chunk := range strings.Split(s, ",") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		label, rest, found := strings.Cut(chunk, "=")
		if !found {
			label, rest = "session", chunk
		}
		out = append(out, Session{Label: label, Cookies: ParseCookieHeader(rest)})
	}
	return out
}

// ParseCookieHeader turns "a=1; b=2" into structured cookies.
func ParseCookieHeader(raw string) []Cookie {
	var out []Cookie
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, found := strings.Cut(part, "=")
		if !found || name == "" {
			continue
		}
		out = append(out, Cookie{Name: strings.TrimSpace(name), Value: strings.TrimSpace(value)})
	}
	return out
}

// Validate checks every field that could produce a silently broken relay.
func (c *Config) Validate() error {
	if err := c.validateListen(); err != nil {
		return err
	}
	// A relay needs at least one way to authenticate: either static client
	// tokens, or the multi-tenant key store behind an admin password. With
	// neither, it is an open proxy.
	if len(c.ClientTokens) == 0 && c.Admin.Password == "" {
		return fmt.Errorf(
			"neither client_tokens nor admin.password is set: the relay would be an open proxy")
	}
	for i, t := range c.ClientTokens {
		if containsPlaceholder(t) {
			return fmt.Errorf(
				"client_tokens[%d] is still the example value. Replace it with your own "+
					"secret, for example: openssl rand -hex 32", i)
		}
		if len(t) < 16 {
			return fmt.Errorf("client_tokens[%d] is shorter than 16 chars", i)
		}
	}
	// Reachable from the network means every credential is the only thing
	// between a stranger and the account. A short or placeholder password is
	// fine on loopback and not fine here, so the bar moves with the exposure.
	if c.exposed() && c.AllowNonLocal {
		if p := c.Admin.Password; p != "" {
			if len(p) < 12 {
				return fmt.Errorf(
					"admin.password is shorter than 12 characters and the listener is " +
						"reachable from the network, where it is the only thing protecting " +
						"the account; use a longer password or bind to 127.0.0.1")
			}
			if containsPlaceholder(p) || containsWeakMarker(p) {
				return fmt.Errorf(
					"admin.password looks like a default or example value and the listener " +
						"is reachable from the network; choose one unique to this deployment")
			}
		}
	}

	if c.Upstream.BaseURL == "" {
		return fmt.Errorf("upstream.base_url is empty")
	}
	if !strings.HasPrefix(c.Upstream.BaseURL, "http://") &&
		!strings.HasPrefix(c.Upstream.BaseURL, "https://") {
		return fmt.Errorf("upstream.base_url must start with http:// or https://")
	}
	for i, s := range c.Upstream.Sessions {
		if len(s.Cookies) == 0 {
			return fmt.Errorf("upstream.sessions[%d] (%s) has no cookies", i, s.Label)
		}
		for _, ck := range s.Cookies {
			if containsPlaceholder(ck.Value) {
				return fmt.Errorf("upstream.sessions[%d] cookie %q is still the example "+
					"value; open /console and use the browser wizard, or paste a real "+
					"cookie", i, ck.Name)
			}
		}
	}
	switch c.Behavior.WebSearchDefault {
	case "", "auto", "enabled", "disabled":
	default:
		return fmt.Errorf("behavior.web_search_default must be auto|enabled|disabled, got %q",
			c.Behavior.WebSearchDefault)
	}
	switch c.Behavior.SystemPromptMode {
	case "", "prepend", "drop":
	default:
		return fmt.Errorf("behavior.system_prompt_mode must be prepend|drop, got %q",
			c.Behavior.SystemPromptMode)
	}
	if c.Upstream.MaxBodyBytes <= 0 {
		return fmt.Errorf("upstream.max_body_bytes must be positive")
	}
	if c.Upstream.ConnectTimeout <= 0 {
		return fmt.Errorf("upstream.connect_timeout_seconds must be positive")
	}
	return nil
}

// exposed reports whether the listener is reachable from outside this host.
func (c *Config) exposed() bool {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	// An empty host means "all interfaces", which is never loopback.
	if ip == nil {
		return true
	}
	return !ip.IsLoopback()
}

// validateListen keeps the listener safe without asking the operator to reason
// about network topology.
//
// Binding beyond loopback hands every caller the account cookies this process
// holds, so it is gated behind an explicit opt-in rather than an IP allowlist:
// an allowlist has to be edited whenever the caller moves, which in practice
// means people disable it. The gate that survives contact with real users is
// one that names a single clear decision.
func (c *Config) validateListen() error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q is not host:port: %w", c.Listen, err)
	}
	if c.exposed() && !c.AllowNonLocal {
		return fmt.Errorf(
			"listen %q is reachable from outside this machine, and this process "+
				"holds live MiMo account cookies: anyone who can reach the port can "+
				"use your account. If that is intended (Docker, a trusted LAN, a "+
				"tunnel), set allow_non_loopback_listen=true. Otherwise leave listen "+
				"as 127.0.0.1:%s and point your client at it from this machine.",
			c.Listen, portOf(c.Listen))
	}
	return nil
}

func portOf(hostport string) string {
	if _, p, err := net.SplitHostPort(hostport); err == nil {
		return p
	}
	return "8793"
}

// containsWeakMarker reports whether a secret looks like one of the defaults
// people copy instead of replacing.
func containsWeakMarker(s string) bool {
	low := strings.ToLower(s)
	for _, m := range weakPasswordMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func containsPlaceholder(s string) bool {
	up := strings.ToUpper(s)
	for _, m := range placeholderMarkers {
		if strings.Contains(up, m) {
			return true
		}
	}
	return false
}

// ResolveModel maps a client-supplied model name to the upstream model code.
func (c *Config) ResolveModel(name string) string {
	if name == "" {
		name = c.Models.Default
	}
	if v, ok := c.Models.Alias[name]; ok {
		return v
	}
	return name
}
