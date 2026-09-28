package httpapi

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"mimowebapi/internal/apikeys"
	"mimowebapi/internal/config"
	"mimowebapi/internal/session"
	"mimowebapi/internal/upstream"
)

// testToken is the client credential used across the handler tests.
const testToken = "test-token-0123456789abcdef"

// testConfig builds a config pointing at the given upstream, with one session
// and one client token.
//
// Every on-disk path is redirected into a fresh temp directory. The defaults
// are relative, so a test that authorises an account would otherwise rewrite
// internal/httpapi/sessions.json — the checked-in fixture — and leave the tree
// dirty after a plain `go test ./...`.
func testConfig(upstreamURL string) *config.Config {
	cfg := config.Default()
	dir, err := os.MkdirTemp("", "mimowebapi-test-")
	if err != nil {
		panic(err) // a tempdir we cannot create means the environment is broken
	}
	cfg.Admin.KeyStorePath = filepath.Join(dir, "keys.json")
	cfg.Admin.SessionDir = dir
	cfg.Admin.BrowserProfileDir = filepath.Join(dir, "chrome-profile")
	cfg.Listen = "127.0.0.1:0"
	cfg.ClientTokens = []string{testToken}
	cfg.Upstream.BaseURL = upstreamURL
	cfg.Upstream.Sessions = []config.Session{{
		Label: "test",
		Cookies: []config.Cookie{
			{Name: "serviceToken", Value: "fake-service-token-value"},
			{Name: "userId", Value: "1234567890"},
		},
	}}
	cfg.Upstream.ConnectTimeout = 5
	cfg.Upstream.ResponseTimeout = 30
	cfg.Upstream.MaxBodyBytes = 1 << 20
	cfg.Upstream.CooldownSeconds = 1
	cfg.Models.Default = "mimo-v2.6-flash"
	cfg.Models.List = []string{"mimo-v2.6-flash", "mimo-v2.6-pro"}
	return cfg
}

func buildServer(cfg *config.Config) (*Server, error) {
	pool := session.New(cfg.Upstream.Sessions, time.Second)
	client, err := upstream.New(cfg, pool)
	if err != nil {
		return nil, err
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := apikeys.Open("") // in-memory for tests
	if err != nil {
		return nil, err
	}
	return NewServer(cfg, client, log, store), nil
}

// buildServerNoSessions builds a relay with no upstream credentials, which is
// the state a fresh install starts in.
func buildServerNoSessions(cfg *config.Config) (*Server, error) {
	cfg.Upstream.Sessions = nil
	return buildServer(cfg)
}
