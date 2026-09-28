package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mimowebapi/internal/config"
	"mimowebapi/internal/session"
)

func testSession() config.Session {
	return config.Session{
		Label: "test",
		Cookies: []config.Cookie{
			{Name: "xiaomichatbot_serviceToken", Value: "tok"},
			{Name: "userId", Value: "42"},
			{Name: "xiaomichatbot_ph", Value: "PH-VALUE=="},
		},
	}
}

func testClient(t *testing.T, base string) (*Client, config.Session) {
	t.Helper()
	cfg := config.Default()
	cfg.Upstream.BaseURL = base
	cfg.Upstream.Sessions = []config.Session{testSession()}
	cfg.Upstream.ConnectTimeout = 5
	cfg.Upstream.ResponseTimeout = 10
	pool := session.New(cfg.Upstream.Sessions, time.Minute)
	c, err := New(cfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	return c, cfg.Upstream.Sessions[0]
}

// The `ph` value must ride in the query string. The backend returns the very
// same 401 for a missing cookie and for a missing query parameter, so this is
// the difference between a relay that works and one that never authenticates.
func TestRequestURLCarriesPHQueryParam(t *testing.T) {
	c, s := testClient(t, "https://example.test")
	got := c.requestURL(PathChat, s)

	if !strings.Contains(got, "xiaomichatbot_ph=PH-VALUE%3D%3D") {
		t.Fatalf("ph query parameter missing or mis-encoded: %s", got)
	}
	if !strings.HasPrefix(got, "https://example.test/open-apis/bot/chat?") {
		t.Errorf("path wrong: %s", got)
	}
}

// A session without a ph cookie must not emit a dangling parameter.
func TestRequestURLWithoutPH(t *testing.T) {
	c, s := testClient(t, "https://example.test")
	s.Cookies = []config.Cookie{{Name: "xiaomichatbot_serviceToken", Value: "tok"}}

	got := c.requestURL(PathChat, s)
	if strings.Contains(got, "xiaomichatbot_ph") {
		t.Errorf("unexpected ph parameter: %s", got)
	}
}

// The ph cookie is matched by suffix because the name is prefixed per
// environment (production uses `xiaomichatbot_ph`).
func TestPHFromSessionMatchesBySuffix(t *testing.T) {
	cases := map[string]string{
		"xiaomichatbot_ph":  "a",
		"ph":                "a",
		"preoverseas-ph_ph": "a",
		"unrelated":         "",
	}
	for name, want := range cases {
		s := config.Session{Cookies: []config.Cookie{{Name: name, Value: "a"}}}
		got := phFromSession(s)
		if (got == "") != (want == "") {
			t.Errorf("cookie %q: got %q, want found=%v", name, got, want != "")
		}
	}
}

// The live usage frame is camelCase with a nested nativeUsage block.
func TestParseUsageLiveFormat(t *testing.T) {
	raw := map[string]any{
		"promptTokens":     float64(2370),
		"completionTokens": float64(149),
		"totalTokens":      float64(2519),
		"nativeUsage": map[string]any{
			"prompt_tokens":     float64(2370),
			"completion_tokens": float64(149),
			"total_tokens":      float64(2519),
		},
	}
	got := parseUsage(raw)
	if got.PromptTokens != 2370 || got.CompletionTokens != 149 || got.TotalTokens != 2519 {
		t.Errorf("usage = %+v", got)
	}
}

// The older snake_case shape must still parse, since model revisions differ.
func TestParseUsageLegacyFormat(t *testing.T) {
	got := parseUsage(map[string]any{
		"prompt_tokens":     float64(10),
		"completion_tokens": float64(5),
	})
	if got.PromptTokens != 10 || got.CompletionTokens != 5 || got.TotalTokens != 15 {
		t.Errorf("usage = %+v", got)
	}
}

// End-to-end through the real client: the chat request must carry both the
// cookie and the query parameter, and the frames must decode.
func TestChatSendsPHAndDecodesFrames(t *testing.T) {
	var gotQuery, gotCookie string
	mux := http.NewServeMux()
	mux.HandleFunc(PathChat, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"id:x\nevent:dialogId\ndata:{\"content\":\"1\"}\n\n" +
				"id:x\nevent:message\ndata:{\"type\":\"text\",\"content\":\"hi\"}\n\n" +
				"id:x\nevent:usage\ndata:{\"promptTokens\":3,\"completionTokens\":1}\n\n" +
				"id:x\nevent:finish\ndata:{\"content\":\"[DONE]\"}\n\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, _ := testClient(t, srv.URL)
	stream, err := c.Chat(t.Context(), NewChatRequest("", "mimo-v2.6-pro",
		ModelConfig{Model: "mimo-v2.6-pro"}, "hello", nil))
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var usage *Usage
	for f := range stream.Frames {
		if f.Event == "message" {
			text += f.Content
		}
		if f.Event == "usage" {
			usage = f.Usage
		}
	}
	stream.Close(nil)

	if !strings.Contains(gotQuery, "xiaomichatbot_ph=") {
		t.Errorf("chat request missing ph query parameter: %q", gotQuery)
	}
	if !strings.Contains(gotCookie, "xiaomichatbot_serviceToken=") {
		t.Errorf("chat request missing session cookie: %q", gotCookie)
	}
	if text != "hi" {
		t.Errorf("text = %q", text)
	}
	if usage == nil || usage.PromptTokens != 3 {
		t.Errorf("usage = %+v", usage)
	}
}
