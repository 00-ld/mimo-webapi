package httpapi

import (
	"io"
	"log/slog"
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
func testConfig(upstreamURL string) *config.Config {
	cfg := config.Default()
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
