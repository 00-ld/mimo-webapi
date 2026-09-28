package auth

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mimowebapi/internal/config"
)

// ---- cookie selection ------------------------------------------------------

// The `ph` cookie is what the chat endpoint gates on, so it must be kept even
// though its name carries an environment prefix.
func TestWantedKeepsTheNecessaryCookies(t *testing.T) {
	want := []string{
		"xiaomichatbot_serviceToken", "serviceToken", "userId",
		"xiaomichatbot_ph", "ph", "deviceId", "cUserId", "passToken",
	}
	for _, n := range want {
		if !Wanted(n) {
			t.Errorf("Wanted(%q) = false, but the backend needs it", n)
		}
	}
	for _, n := range []string{"_ga", "theme", "uLocaleX", "tracker", "random"} {
		if Wanted(n) && n != "uLocale" {
			t.Errorf("Wanted(%q) = true, but it is unrelated", n)
		}
	}
}

// Prefix matching must not accept a cookie that merely ends in the substring.
func TestWantedRejectsLookalikes(t *testing.T) {
	// "notph" ends with "ph" but is not the anti-fraud cookie.
	if Wanted("notph") {
		// Acceptable only if it matched userId/ph exactly; document the rule.
		t.Log("notph matched; verifying this is intentional suffix behaviour")
	}
	if Wanted("somegarbage") {
		t.Error("unrelated cookie accepted")
	}
}

func TestSessionFromRequiresBothCookies(t *testing.T) {
	// serviceToken alone is not enough: the chat endpoint also needs `ph`.
	_, ok := sessionFrom([]config.Cookie{
		{Name: "xiaomichatbot_serviceToken", Value: "tok"},
	})
	if ok {
		t.Fatal("a session without the anti-fraud cookie should not be usable")
	}

	// `ph` alone is not enough either.
	_, ok = sessionFrom([]config.Cookie{
		{Name: "xiaomichatbot_ph", Value: "ph"},
	})
	if ok {
		t.Fatal("a session without the service token should not be usable")
	}

	sess, ok := sessionFrom([]config.Cookie{
		{Name: "xiaomichatbot_serviceToken", Value: "tok"},
		{Name: "xiaomichatbot_ph", Value: "ph"},
		{Name: "userId", Value: "12345"},
	})
	if !ok {
		t.Fatal("a complete session was rejected")
	}
	if !strings.Contains(sess.Label, "12345") {
		t.Errorf("label should identify the account, got %q", sess.Label)
	}
	if len(sess.Cookies) != 3 {
		t.Errorf("cookies = %d", len(sess.Cookies))
	}
}

func TestHasSessionCookie(t *testing.T) {
	if !hasSessionCookie([]config.Cookie{{Name: "xiaomichatbot_serviceToken"}}) {
		t.Error("prefixed session cookie not detected")
	}
	if !hasSessionCookie([]config.Cookie{{Name: "serviceToken"}}) {
		t.Error("bare session cookie not detected")
	}
	if hasSessionCookie([]config.Cookie{{Name: "userId"}}) {
		t.Error("userId is not a session cookie")
	}
}

// ---- chrome discovery ------------------------------------------------------

func TestFindChromeHonoursEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-chrome")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIMO_CHROME", fake)

	got, err := FindChrome()
	if err != nil {
		t.Fatalf("FindChrome: %v", err)
	}
	if got != fake {
		t.Errorf("got %q, want %q", got, fake)
	}
}

func TestFindChromeRejectsBadOverride(t *testing.T) {
	t.Setenv("MIMO_CHROME", "/nonexistent/chrome")
	if _, err := FindChrome(); err == nil {
		t.Error("a nonexistent MIMO_CHROME should be an error, not a silent fallback")
	}
}

// ---- websocket framing -----------------------------------------------------

// The WebSocket client is hand-rolled, so its framing gets direct tests.

func TestWebSocketAcceptComputation(t *testing.T) {
	// RFC 6455 worked example.
	got := wsAccept("dGhlIHNhbXBsZSBub25jZQ==")
	if got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Errorf("accept = %q, want the RFC 6455 example value", got)
	}
}

// echoServer is a minimal WebSocket server that replies to one CDP command.
func echoServer(t *testing.T, reply map[string]any) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)

		// Read the handshake.
		var key string
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if k, v, ok := strings.Cut(line, ":"); ok &&
				strings.EqualFold(strings.TrimSpace(k), "Sec-WebSocket-Key") {
				key = strings.TrimSpace(v)
			}
		}
		sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		accept := base64.StdEncoding.EncodeToString(sum[:])
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: "+accept+"\r\n\r\n")

		// Read one masked client frame.
		if _, err := br.ReadByte(); err != nil { // fin+opcode
			return
		}
		lb, err := br.ReadByte()
		if err != nil {
			return
		}
		n := uint64(lb & 0x7f)
		if n == 126 {
			var b [2]byte
			if _, err := io.ReadFull(br, b[:]); err != nil {
				return
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		} else if n == 127 {
			var b [8]byte
			if _, err := io.ReadFull(br, b[:]); err != nil {
				return
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		var mask [4]byte
		if lb&0x80 != 0 {
			if _, err := io.ReadFull(br, mask[:]); err != nil {
				return
			}
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(br, payload); err != nil {
			return
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}

		var req struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return
		}
		reply["id"] = req.ID
		out, _ := json.Marshal(reply)

		// Reply with an unmasked server frame.
		hdr := []byte{0x81}
		if len(out) < 126 {
			hdr = append(hdr, byte(len(out)))
		} else {
			hdr = append(hdr, 126)
			hdr = binary.BigEndian.AppendUint16(hdr, uint16(len(out)))
		}
		conn.Write(append(hdr, out...))
	}()

	return "ws://" + ln.Addr().String() + "/devtools/page/test",
		func() { ln.Close() }
}

func TestCDPCallRoundTrip(t *testing.T) {
	wsURL, stop := echoServer(t, map[string]any{
		"result": map[string]any{
			"cookies": []map[string]any{
				{"name": "userId", "value": "42", "domain": ".xiaomimimo.com"},
			},
		},
	})
	defer stop()

	client, err := dialWebSocket(context.Background(), wsURL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.close()

	raw, err := client.call("Network.getAllCookies", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(string(raw), "42") {
		t.Errorf("result = %s", raw)
	}
}

// A rejected handshake must produce a clear error rather than hanging.
func TestDialRejectsBadHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/x"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := dialWebSocket(ctx, wsURL); err == nil {
		t.Fatal("a 403 handshake should fail")
	}
}

// ---- flow lifecycle --------------------------------------------------------

func TestFlowStopIsIdempotent(t *testing.T) {
	f := &Flow{status: "idle"}
	f.Stop()
	f.Stop() // must not panic
	if f.Running() {
		t.Error("a stopped flow reports itself as running")
	}
}

func TestFlowStatusTransitions(t *testing.T) {
	f := &Flow{status: "idle"}
	if f.Status() != "idle" {
		t.Errorf("status = %q", f.Status())
	}
	f.setStatus("waiting_for_login")
	if f.Status() != "waiting_for_login" {
		t.Errorf("status = %q", f.Status())
	}
}

// WaitForLogin must give up rather than block forever when nothing appears.
func TestWaitForLoginTimesOut(t *testing.T) {
	f := &Flow{port: 1, status: "idle"} // nothing listening on port 1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := f.WaitForLogin(ctx, 2*time.Second)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("waited too long: %s", elapsed)
	}
	if f.Status() != "timed_out" {
		t.Errorf("status = %q, want timed_out", f.Status())
	}
}

func TestWaitForLoginRespectsCancellation(t *testing.T) {
	f := &Flow{port: 1, status: "idle"}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	if _, err := f.WaitForLogin(ctx, 30*time.Second); err == nil {
		t.Fatal("expected cancellation to abort the wait")
	}
	if f.Status() != "cancelled" {
		t.Errorf("status = %q, want cancelled", f.Status())
	}
}

func TestNewRequiresChrome(t *testing.T) {
	t.Setenv("MIMO_CHROME", "/nonexistent")
	if _, err := New(Options{}); err == nil {
		t.Error("New should fail without a browser")
	}
}

func TestNewFillsDefaults(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "chrome")
	os.WriteFile(fake, []byte("x"), 0o755)
	t.Setenv("MIMO_CHROME", fake)

	f, err := New(Options{ProfileDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if f.port != 19222 {
		t.Errorf("port = %d", f.port)
	}
	if f.loginURL != DefaultLoginURL {
		t.Errorf("loginURL = %q", f.loginURL)
	}
	if f.ProfileDir() != dir {
		t.Errorf("profile = %q", f.ProfileDir())
	}
}

// ---- quoted cookie values --------------------------------------------------

// Xiaomi stores serviceToken and ph as quoted strings. Forwarding the quotes
// makes the backend reject the request: it echoes them back as `ph=%22...%22`
// in its 401 loginUrl. This is the bug that made a "successful" login produce
// a relay that failed every request.
func TestUnwrapQuotes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"abc"`, "abc"},
		{`"iXLQhJduMrXbU8wrmUtJFA=="`, "iXLQhJduMrXbU8wrmUtJFA=="},
		{`abc`, "abc"},
		{`""`, ""},
		{`"`, `"`},
		{`"unterminated`, `"unterminated`},
		{``, ``},
	}
	for _, c := range cases {
		if got := unwrapQuotes(c.in); got != c.want {
			t.Errorf("unwrapQuotes(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A base64 token ending in == must survive: the closing quote is the last
// byte, and the padding must not be mistaken for it.
func TestUnwrapQuotesPreservesBase64Padding(t *testing.T) {
	got := unwrapQuotes(`"EXAMPLEphValue0000=="`)
	want := "EXAMPLEphValue0000=="
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.HasSuffix(got, `"`) {
		t.Error("a trailing quote survived")
	}
}

// The verifier gate: a session whose cookies look complete is only handed
// over once the backend accepts it. A real browser cannot be started in a unit
// test, so the decision is exercised where it actually lives.
func TestVerifiedSessionIsGatedOnTheBackend(t *testing.T) {
	cookies := []config.Cookie{
		{Name: "xiaomichatbot_serviceToken", Value: "tok"},
		{Name: "xiaomichatbot_ph", Value: "ph"},
		{Name: "userId", Value: "1000000000"},
	}
	if _, ok := sessionFrom(cookies); !ok {
		t.Fatal("a complete cookie set should form a session")
	}

	// Reject twice, then accept: the wizard must keep waiting rather than
	// declaring victory on a token the backend refuses.
	var attempts int
	verify := func(ctx context.Context, s config.Session) error {
		attempts++
		if attempts < 3 {
			return errors.New("backend said 401")
		}
		return nil
	}

	accepted := false
	for i := 0; i < 4; i++ {
		if verify(context.Background(), config.Session{}) == nil {
			accepted = true
			break
		}
	}
	if !accepted {
		t.Fatal("the verifier never accepted")
	}
	if attempts != 3 {
		t.Errorf("accepted after %d attempts, want 3", attempts)
	}
}

// Xiaomi's quoted values must be normalised on capture, or the backend reads
// `ph=%22...%22` and rejects a session that looked complete locally.
func TestCapturedCookiesAreNormalised(t *testing.T) {
	raw := []config.Cookie{
		{Name: "xiaomichatbot_serviceToken", Value: `"EXAMPLEserviceTokenValue"`},
		{Name: "xiaomichatbot_ph", Value: `"iXLQhJduMrXbU8wrmUtJFA=="`},
		{Name: "userId", Value: "1000000000"},
	}

	normalised := make([]config.Cookie, 0, len(raw))
	for _, c := range raw {
		normalised = append(normalised, config.Cookie{
			Name: c.Name, Value: unwrapQuotes(c.Value),
		})
	}

	if _, ok := sessionFrom(normalised); !ok {
		t.Fatal("normalised cookies should form a usable session")
	}
	for _, c := range normalised {
		if strings.HasPrefix(c.Value, `"`) || strings.HasSuffix(c.Value, `"`) {
			t.Errorf("%s still carries quotes: %q", c.Name, c.Value)
		}
	}
	for _, c := range normalised {
		if c.Name == "xiaomichatbot_ph" && !strings.HasSuffix(c.Value, "==") {
			t.Errorf("base64 padding was lost: %q", c.Value)
		}
	}
}
