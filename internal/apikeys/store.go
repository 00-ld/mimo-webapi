// Package apikeys manages the client credentials that third parties use to
// reach this relay.
//
// Unlike the upstream session pool (which holds the MiMo account cookies), these
// are the keys handed out to other people. Each one carries its own quota and
// usage counters so one caller cannot exhaust the relay for everyone else.
package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors returned by the store.
var (
	ErrNotFound   = errors.New("api key not found")
	ErrQuota      = errors.New("api key quota exhausted")
	ErrDisabled   = errors.New("api key is disabled")
	ErrRateLimit  = errors.New("api key rate limit exceeded")
	ErrNoPassword = errors.New("admin password not configured")
)

// Key is one issued credential.
//
// Only the SHA-256 of the secret is stored, so a leaked keys.json does not
// hand over working credentials. The plaintext is shown exactly once, at
// creation time.
type Key struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"` // first chars, for identification in the UI
	Hash      string `json:"hash"`   // sha256(secret), hex
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"` // 0 = never

	// Quota. Zero means "unlimited" for the token fields.
	QuotaTokens  int64 `json:"quota_tokens"`
	QuotaReqs    int64 `json:"quota_requests"`
	RateLimitRPM int   `json:"rate_limit_rpm"` // requests per minute, 0 = unlimited

	// Usage counters.
	UsedTokens   int64 `json:"used_tokens"`
	UsedRequests int64 `json:"used_requests"`

	// Window holds the rolling minute used for rate limiting. It is persisted
	// so a restart cannot be used to reset a rate limit.
	WindowStart int64 `json:"window_start"`
	WindowCount int   `json:"window_count"`

	LastUsedAt int64  `json:"last_used_at"`
	Note       string `json:"note,omitempty"`
}

// Public is the redacted view safe to send to a browser.
type Public struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Prefix       string `json:"prefix"`
	Enabled      bool   `json:"enabled"`
	CreatedAt    int64  `json:"created_at"`
	ExpiresAt    int64  `json:"expires_at"`
	QuotaTokens  int64  `json:"quota_tokens"`
	QuotaReqs    int64  `json:"quota_requests"`
	RateLimitRPM int    `json:"rate_limit_rpm"`
	UsedTokens   int64  `json:"used_tokens"`
	UsedRequests int64  `json:"used_requests"`
	LastUsedAt   int64  `json:"last_used_at"`
	Note         string `json:"note,omitempty"`
	Exhausted    bool   `json:"exhausted"`
	Expired      bool   `json:"expired"`
}

func (k *Key) public() Public {
	return Public{
		ID: k.ID, Name: k.Name, Prefix: k.Prefix, Enabled: k.Enabled,
		CreatedAt: k.CreatedAt, ExpiresAt: k.ExpiresAt,
		QuotaTokens: k.QuotaTokens, QuotaReqs: k.QuotaReqs,
		RateLimitRPM: k.RateLimitRPM,
		UsedTokens:   k.UsedTokens, UsedRequests: k.UsedRequests,
		LastUsedAt: k.LastUsedAt, Note: k.Note,
		Exhausted: k.exhausted(time.Now()),
		Expired:   k.expired(time.Now()),
	}
}

func (k *Key) expired(now time.Time) bool {
	return k.ExpiresAt > 0 && now.Unix() > k.ExpiresAt
}

func (k *Key) exhausted(now time.Time) bool {
	if k.expired(now) {
		return true
	}
	if k.QuotaTokens > 0 && k.UsedTokens >= k.QuotaTokens {
		return true
	}
	if k.QuotaReqs > 0 && k.UsedRequests >= k.QuotaReqs {
		return true
	}
	return false
}

// storeFile is the on-disk shape.
type storeFile struct {
	Version int    `json:"version"`
	Keys    []*Key `json:"keys"`
}

// Store is a concurrency-safe, persisted key store.
type Store struct {
	mu   sync.RWMutex
	path string
	keys map[string]*Key // by id
	// byHash maps sha256(secret) -> id for O(1) lookup without scanning.
	byHash map[string]string
	now    func() time.Time

	// dirty marks state that needs flushing; writes are batched by Flush so a
	// busy relay does not rewrite the file on every request.
	dirty bool
}

// Open loads (or creates) the store at path.
func Open(path string) (*Store, error) {
	s := &Store{
		path:   path,
		keys:   map[string]*Key{},
		byHash: map[string]string{},
		now:    time.Now,
	}
	if path == "" {
		return s, nil // in-memory only
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read key store: %w", err)
	}
	if len(raw) == 0 {
		return s, nil
	}
	var f storeFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse key store %s: %w", path, err)
	}
	for _, k := range f.Keys {
		if k == nil || k.ID == "" || k.Hash == "" {
			continue
		}
		s.keys[k.ID] = k
		s.byHash[k.Hash] = k.ID
	}
	return s, nil
}

// Count reports how many keys exist.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

// Create issues a new key and returns the plaintext secret.
//
// The secret is not recoverable afterwards: only its hash is kept.
func (s *Store) Create(name string, quotaTokens, quotaReqs int64, rateRPM int,
	ttl time.Duration, note string) (string, *Key, error) {

	name = strings.TrimSpace(name)
	if name == "" {
		name = "unnamed"
	}

	secret, err := newSecret()
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])

	now := s.now()
	k := &Key{
		ID:           newID(),
		Name:         name,
		Prefix:       secret[:min(len(secret), 12)],
		Hash:         hash,
		Enabled:      true,
		CreatedAt:    now.Unix(),
		QuotaTokens:  quotaTokens,
		QuotaReqs:    quotaReqs,
		RateLimitRPM: rateRPM,
		Note:         note,
	}
	if ttl > 0 {
		k.ExpiresAt = now.Add(ttl).Unix()
	}

	s.mu.Lock()
	s.keys[k.ID] = k
	s.byHash[hash] = k.ID
	s.dirty = true
	s.mu.Unlock()

	if err := s.Flush(); err != nil {
		return "", nil, err
	}
	return secret, k, nil
}

// Authenticate resolves a presented secret to a usable key.
//
// It returns the key on success; otherwise a sentinel error describing why the
// key was rejected, so the caller can pick the right HTTP status.
func (s *Store) Authenticate(secret string) (*Key, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, ErrNotFound
	}
	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])

	s.mu.Lock()
	defer s.mu.Unlock()

	id, ok := s.byHash[hash]
	if !ok {
		// Constant-time fallback scan. The map lookup above already leaks
		// nothing useful (the attacker cannot see it), but this keeps the
		// work proportional to the key count rather than bailing early.
		for _, k := range s.keys {
			if subtle.ConstantTimeCompare([]byte(k.Hash), []byte(hash)) == 1 {
				id, ok = k.ID, true
				break
			}
		}
		if !ok {
			return nil, ErrNotFound
		}
	}
	k := s.keys[id]
	if k == nil {
		return nil, ErrNotFound
	}
	if !k.Enabled {
		return nil, ErrDisabled
	}
	now := s.now()
	if k.expired(now) {
		return nil, ErrDisabled
	}
	if k.exhausted(now) {
		return nil, ErrQuota
	}

	// Rolling one-minute rate limit.
	if k.RateLimitRPM > 0 {
		if now.Unix()-k.WindowStart >= 60 {
			k.WindowStart = now.Unix()
			k.WindowCount = 0
		}
		if k.WindowCount >= k.RateLimitRPM {
			return nil, ErrRateLimit
		}
		k.WindowCount++
	}

	k.UsedRequests++
	k.LastUsedAt = now.Unix()
	s.dirty = true
	return k, nil
}

// RecordUsage adds token usage to a key after a successful completion.
func (s *Store) RecordUsage(id string, tokens int64) {
	if tokens <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if k := s.keys[id]; k != nil {
		k.UsedTokens += tokens
		s.dirty = true
	}
}

// List returns every key, redacted, newest first.
func (s *Store) List() []Public {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Public, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, k.public())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// Get returns one key by id.
func (s *Store) Get(id string) (*Key, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k := s.keys[id]
	if k == nil {
		return nil, ErrNotFound
	}
	cp := *k
	return &cp, nil
}

// Update applies a partial change to a key.
//
// Pointer fields distinguish "leave alone" (nil) from "set to zero".
func (s *Store) Update(id string, name *string, enabled *bool,
	quotaTokens, quotaReqs *int64, rateRPM *int, note *string) (*Key, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.keys[id]
	if k == nil {
		return nil, ErrNotFound
	}
	if name != nil {
		k.Name = strings.TrimSpace(*name)
	}
	if enabled != nil {
		k.Enabled = *enabled
	}
	if quotaTokens != nil {
		k.QuotaTokens = *quotaTokens
	}
	if quotaReqs != nil {
		k.QuotaReqs = *quotaReqs
	}
	if rateRPM != nil {
		k.RateLimitRPM = *rateRPM
	}
	if note != nil {
		k.Note = *note
	}
	s.dirty = true
	cp := *k
	return &cp, nil
}

// ResetUsage zeroes a key's counters without touching its quota.
func (s *Store) ResetUsage(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.keys[id]
	if k == nil {
		return ErrNotFound
	}
	k.UsedTokens = 0
	k.UsedRequests = 0
	k.WindowCount = 0
	k.WindowStart = 0
	s.dirty = true
	return nil
}

// Delete removes a key permanently.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.keys[id]
	if k == nil {
		return ErrNotFound
	}
	delete(s.byHash, k.Hash)
	delete(s.keys, id)
	s.dirty = true
	return s.flushLocked()
}

// Rotate issues a fresh secret for an existing key and returns it.
func (s *Store) Rotate(id string) (string, *Key, error) {
	secret, err := newSecret()
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.keys[id]
	if k == nil {
		return "", nil, ErrNotFound
	}
	delete(s.byHash, k.Hash)
	k.Hash = hash
	k.Prefix = secret[:min(len(secret), 12)]
	s.byHash[hash] = id
	s.dirty = true
	cp := *k
	if err := s.flushLocked(); err != nil {
		return "", nil, err
	}
	return secret, &cp, nil
}

// Flush writes the store to disk if anything changed.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *Store) flushLocked() error {
	if !s.dirty || s.path == "" {
		return nil
	}
	f := storeFile{Version: 1, Keys: make([]*Key, 0, len(s.keys))}
	for _, k := range s.keys {
		f.Keys = append(f.Keys, k)
	}
	sort.Slice(f.Keys, func(i, j int) bool { return f.Keys[i].CreatedAt < f.Keys[j].CreatedAt })

	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	// Write to a sibling temp file and rename, so a crash mid-write cannot
	// leave a truncated store behind.
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".keys-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// newSecret returns a URL-safe key with a recognisable prefix.
func newSecret() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return "sk-mimo-" + hex.EncodeToString(b[:]), nil
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("k%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
