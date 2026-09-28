package auth

import (
	"context"
	"os"
	"testing"
)

// TestLiveCookieRead exercises the real capture path against the running
// browser, so the filter can be checked against reality rather than a fixture.
func TestLiveCookieRead(t *testing.T) {
	if os.Getenv("MIMO_LIVE") == "" {
		t.Skip("set MIMO_LIVE=1 to probe the running browser")
	}
	f := &Flow{port: 19222}
	ck, err := f.ReadCookies(context.Background())
	if err != nil {
		t.Fatalf("ReadCookies: %v", err)
	}
	t.Logf("captured %d cookies:", len(ck))
	for _, c := range ck {
		t.Logf("  %-34s len=%d  %.30s", c.Name, len(c.Value), c.Value)
	}
	sess, ok := sessionFrom(ck)
	t.Logf("sessionFrom => ok=%v label=%s", ok, sess.Label)
	if !ok {
		t.Error("the live browser has both required cookies but no session was formed")
	}
}
