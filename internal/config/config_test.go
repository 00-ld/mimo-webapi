package config

import (
	"os"
	"path/filepath"
	"testing"
)

func validConfig() *Config {
	c := Default()
	c.ClientTokens = []string{"0123456789abcdef0123456789abcdef"}
	c.Upstream.Sessions = []Session{{
		Label:   "main",
		Cookies: []Cookie{{Name: "serviceToken", Value: "abc123"}},
	}}
	return c
}

func TestValidateAcceptsGoodConfig(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A relay that binds a public interface with live cookies on board is the
// single most damaging misconfiguration, so it must not boot by default.
func TestValidateRejectsNonLoopbackWithoutOptIn(t *testing.T) {
	c := validConfig()
	c.Listen = "0.0.0.0:8793"
	err := c.Validate()
	if err == nil {
		t.Fatal("expected a refusal for a non-loopback listen")
	}

	c.AllowNonLocal = true
	if err := c.Validate(); err != nil {
		t.Fatalf("opt-in should permit it, got %v", err)
	}
}

func TestValidateLoopbackForms(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8793", "[::1]:8793", "localhost:8793"} {
		c := validConfig()
		c.Listen = addr
		if err := c.Validate(); err != nil {
			t.Errorf("listen %q rejected: %v", addr, err)
		}
	}
	c := validConfig()
	c.Listen = "not-an-address"
	if err := c.Validate(); err == nil {
		t.Error("expected a malformed listen address to be rejected")
	}
}

func TestValidateRejectsOpenProxy(t *testing.T) {
	c := validConfig()
	c.ClientTokens = nil
	if err := c.Validate(); err == nil {
		t.Error("expected an error when no client tokens are configured")
	}
}

// Placeholder tokens are working credentials, so they must never validate.
func TestValidateRejectsPlaceholderTokens(t *testing.T) {
	for _, tok := range []string{
		"REPLACE_ME_WITH_A_REAL_TOKEN",
		"PLACEHOLDER-0123456789abcdef",
		"<your-token-here>",
	} {
		c := validConfig()
		c.ClientTokens = []string{tok}
		if err := c.Validate(); err == nil {
			t.Errorf("token %q should have been rejected", tok)
		}
	}
}

func TestValidateRejectsShortTokens(t *testing.T) {
	c := validConfig()
	c.ClientTokens = []string{"short"}
	if err := c.Validate(); err == nil {
		t.Error("expected a short token to be rejected")
	}
}

func TestValidateRequiresCookies(t *testing.T) {
	c := validConfig()
	c.Upstream.Sessions = []Session{{Label: "empty"}}
	if err := c.Validate(); err == nil {
		t.Error("expected a session with no cookies to be rejected")
	}
}

func TestValidateRejectsPlaceholderCookie(t *testing.T) {
	c := validConfig()
	c.Upstream.Sessions[0].Cookies[0].Value = "PASTE_YOUR_COOKIE_HERE"
	if err := c.Validate(); err == nil {
		t.Error("expected a placeholder cookie to be rejected")
	}
}

func TestValidateBehaviorEnums(t *testing.T) {
	c := validConfig()
	c.Behavior.WebSearchDefault = "sometimes"
	if err := c.Validate(); err == nil {
		t.Error("expected an invalid web_search_default to be rejected")
	}
	c = validConfig()
	c.Behavior.SystemPromptMode = "cook"
	if err := c.Validate(); err == nil {
		t.Error("expected an invalid system_prompt_mode to be rejected")
	}
}

func TestParseCookieHeader(t *testing.T) {
	got := ParseCookieHeader("a=1; b=two; serviceToken=xyz; malformed; c=3")
	want := map[string]string{"a": "1", "b": "two", "serviceToken": "xyz", "c": "3"}
	if len(got) != len(want) {
		t.Fatalf("parsed %d cookies, want %d: %+v", len(got), len(want), got)
	}
	for _, c := range got {
		if want[c.Name] != c.Value {
			t.Errorf("cookie %s = %q, want %q", c.Name, c.Value, want[c.Name])
		}
	}
}

func TestSessionHeader(t *testing.T) {
	s := Session{Cookies: []Cookie{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}}}
	if got := s.Header(); got != "a=1; b=2" {
		t.Errorf("header = %q", got)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// "listne" is a typo for "listen"; silently accepting it would leave the
	// relay bound to the default address while the operator believes otherwise.
	body := `{"listen":"127.0.0.1:8793","listne":"127.0.0.1:9999",
	  "client_tokens":["0123456789abcdef0123456789abcdef"]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("expected an unknown field to be rejected")
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"listen":"127.0.0.1:8793",
	  "client_tokens":["0123456789abcdef0123456789abcdef"],
	  "upstream":{"sessions":[{"label":"s","cookies":[{"name":"serviceToken","value":"v"}]}]}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream.BaseURL != "https://aistudio.xiaomimimo.com" {
		t.Errorf("base url default lost: %q", cfg.Upstream.BaseURL)
	}
	if cfg.Models.Default == "" {
		t.Error("default model not populated")
	}
	if cfg.Behavior.SystemPromptMode != "prepend" {
		t.Errorf("system prompt mode = %q", cfg.Behavior.SystemPromptMode)
	}
}

func TestResolveModelAlias(t *testing.T) {
	c := validConfig()
	c.Models.Default = "mimo-v2.6-flash"
	c.Models.Alias = map[string]string{"gpt-4o": "mimo-v2.6-pro"}

	if got := c.ResolveModel("gpt-4o"); got != "mimo-v2.6-pro" {
		t.Errorf("alias not applied: %q", got)
	}
	if got := c.ResolveModel(""); got != "mimo-v2.6-flash" {
		t.Errorf("default not applied: %q", got)
	}
	if got := c.ResolveModel("mimo-v2.6-pro"); got != "mimo-v2.6-pro" {
		t.Errorf("passthrough broken: %q", got)
	}
}

// A negative query ceiling is a configuration mistake, not a way to disable
// the guard: 0 is the documented off switch, so anything below it must be
// rejected at boot rather than silently disabling the protection.
func TestValidateRejectsNegativeMaxQueryChars(t *testing.T) {
	c := validConfig()
	c.Upstream.MaxQueryChars = -1
	if err := c.Validate(); err == nil {
		t.Fatal("negative max_query_chars was accepted")
	}
}

// Zero is the documented way to defer entirely to the backend.
func TestValidateAcceptsZeroMaxQueryChars(t *testing.T) {
	c := validConfig()
	c.Upstream.MaxQueryChars = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("0 must disable the guard, got: %v", err)
	}
}
