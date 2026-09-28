package apikeys

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateAndAuthenticate(t *testing.T) {
	s := newStore(t)
	secret, key, err := s.Create("alice", 0, 0, 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "sk-mimo-") {
		t.Errorf("secret lacks its prefix: %q", secret)
	}
	if key.Name != "alice" || !key.Enabled {
		t.Errorf("key = %+v", key)
	}

	got, err := s.Authenticate(secret)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != key.ID {
		t.Errorf("resolved the wrong key")
	}
	if got.UsedRequests != 1 {
		t.Errorf("request not counted: %d", got.UsedRequests)
	}
}

// A store file is not a credential dump: only hashes may be written.
func TestSecretIsNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := s.Create("bob", 0, 0, 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("the plaintext secret was written to disk")
	}
	if !strings.Contains(string(raw), "hash") {
		t.Error("expected hashes in the store file")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("store mode = %o, want 600", perm)
	}
}

func TestAuthenticateRejectsWrongSecret(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Create("alice", 0, 0, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate("sk-mimo-nope"); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, err := s.Authenticate(""); err != ErrNotFound {
		t.Errorf("empty secret: %v", err)
	}
}

func TestDisabledKeyIsRejected(t *testing.T) {
	s := newStore(t)
	secret, key, _ := s.Create("alice", 0, 0, 0, 0, "")

	off := false
	if _, err := s.Update(key.ID, nil, &off, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(secret); err != ErrDisabled {
		t.Errorf("err = %v, want ErrDisabled", err)
	}

	on := true
	if _, err := s.Update(key.ID, nil, &on, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(secret); err != nil {
		t.Errorf("re-enabled key rejected: %v", err)
	}
}

func TestRequestQuota(t *testing.T) {
	s := newStore(t)
	secret, _, _ := s.Create("alice", 0, 2, 0, 0, "")

	for i := 0; i < 2; i++ {
		if _, err := s.Authenticate(secret); err != nil {
			t.Fatalf("request %d rejected early: %v", i+1, err)
		}
	}
	if _, err := s.Authenticate(secret); err != ErrQuota {
		t.Errorf("err = %v, want ErrQuota", err)
	}
}

func TestTokenQuota(t *testing.T) {
	s := newStore(t)
	secret, key, _ := s.Create("alice", 100, 0, 0, 0, "")

	if _, err := s.Authenticate(secret); err != nil {
		t.Fatal(err)
	}
	s.RecordUsage(key.ID, 100)
	if _, err := s.Authenticate(secret); err != ErrQuota {
		t.Errorf("err = %v, want ErrQuota", err)
	}
}

func TestRateLimit(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newStore(t)
	s.now = func() time.Time { return now }
	secret, _, _ := s.Create("alice", 0, 0, 3, 0, "")

	for i := 0; i < 3; i++ {
		if _, err := s.Authenticate(secret); err != nil {
			t.Fatalf("request %d should pass: %v", i+1, err)
		}
	}
	if _, err := s.Authenticate(secret); err != ErrRateLimit {
		t.Errorf("err = %v, want ErrRateLimit", err)
	}

	// The window must roll over rather than stay exhausted forever.
	now = now.Add(61 * time.Second)
	if _, err := s.Authenticate(secret); err != nil {
		t.Errorf("window did not roll over: %v", err)
	}
}

func TestExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newStore(t)
	s.now = func() time.Time { return now }
	secret, _, _ := s.Create("alice", 0, 0, 0, time.Hour, "")

	if _, err := s.Authenticate(secret); err != nil {
		t.Fatalf("fresh key rejected: %v", err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := s.Authenticate(secret); err != ErrDisabled {
		t.Errorf("err = %v, want ErrDisabled after expiry", err)
	}
}

// Rotating must invalidate the old secret immediately.
func TestRotateInvalidatesOldSecret(t *testing.T) {
	s := newStore(t)
	old, key, _ := s.Create("alice", 0, 0, 0, 0, "")

	fresh, _, err := s.Rotate(key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old {
		t.Fatal("rotate returned the same secret")
	}
	if _, err := s.Authenticate(old); err != ErrNotFound {
		t.Errorf("old secret still works: %v", err)
	}
	if _, err := s.Authenticate(fresh); err != nil {
		t.Errorf("new secret rejected: %v", err)
	}
}

func TestDeleteRemovesKey(t *testing.T) {
	s := newStore(t)
	secret, key, _ := s.Create("alice", 0, 0, 0, 0, "")

	if err := s.Delete(key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(secret); err != ErrNotFound {
		t.Errorf("deleted key still authenticates: %v", err)
	}
	if s.Count() != 0 {
		t.Errorf("count = %d", s.Count())
	}
	if err := s.Delete("missing"); err != ErrNotFound {
		t.Errorf("err = %v", err)
	}
}

// The store must survive a restart with quotas and counters intact.
func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")

	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	secret, key, _ := s1.Create("alice", 500, 10, 5, 0, "note")
	if _, err := s1.Authenticate(secret); err != nil {
		t.Fatal(err)
	}
	s1.RecordUsage(key.ID, 42)
	if err := s1.Flush(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Authenticate(secret)
	if err != nil {
		t.Fatalf("secret did not survive restart: %v", err)
	}
	if got.UsedTokens != 42 {
		t.Errorf("tokens = %d, want 42", got.UsedTokens)
	}
	if got.UsedRequests != 2 {
		t.Errorf("requests = %d, want 2 (1 before flush + 1 after reload)", got.UsedRequests)
	}
	if got.QuotaTokens != 500 || got.RateLimitRPM != 5 || got.Note != "note" {
		t.Errorf("settings lost: %+v", got)
	}
}

func TestListIsSortedAndRedacted(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newStore(t)
	s.now = func() time.Time { return now }
	s.Create("first", 0, 0, 0, 0, "")
	now = now.Add(time.Second)
	s.Create("second", 0, 0, 0, 0, "")

	list := s.List()
	if len(list) != 2 {
		t.Fatalf("len = %d", len(list))
	}
	if list[0].Name != "second" {
		t.Errorf("newest first expected, got %q", list[0].Name)
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "hash") {
		t.Error("the public view leaked the hash field")
	}
}

func TestResetUsageKeepsQuota(t *testing.T) {
	s := newStore(t)
	secret, key, _ := s.Create("alice", 100, 2, 0, 0, "")
	s.RecordUsage(key.ID, 100)

	if _, err := s.Authenticate(secret); err != ErrQuota {
		t.Fatalf("expected quota exhaustion, got %v", err)
	}
	if err := s.ResetUsage(key.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Authenticate(secret)
	if err != nil {
		t.Fatalf("key unusable after reset: %v", err)
	}
	if got.QuotaTokens != 100 {
		t.Errorf("reset changed the quota: %d", got.QuotaTokens)
	}
}

// Concurrent authentication must not corrupt counters.
func TestConcurrentAuthenticate(t *testing.T) {
	s := newStore(t)
	secret, key, _ := s.Create("alice", 0, 0, 0, 0, "")

	var wg sync.WaitGroup
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Authenticate(secret); err != nil {
				t.Errorf("authenticate: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := s.Get(key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsedRequests != n {
		t.Errorf("requests = %d, want %d", got.UsedRequests, n)
	}
}

func TestUnlimitedByDefault(t *testing.T) {
	s := newStore(t)
	secret, _, _ := s.Create("alice", 0, 0, 0, 0, "")
	for i := 0; i < 200; i++ {
		if _, err := s.Authenticate(secret); err != nil {
			t.Fatalf("unlimited key hit a limit at %d: %v", i, err)
		}
	}
}
