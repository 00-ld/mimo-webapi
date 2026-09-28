// Package session manages the pool of logged-in MiMo Studio accounts used to
// serve upstream traffic.
//
// The web backend authenticates by cookie only, so a "key" here is a whole
// browser session. The pool rotates across sessions and parks any session the
// backend rejects so a dead cookie does not keep absorbing requests.
package session

import (
	"errors"
	"sort"
	"sync"
	"time"

	"mimowebapi/internal/config"
)

// ErrNoSession means every configured session is cooling down.
var ErrNoSession = errors.New("no upstream session available")

// entry is one pooled session plus its health bookkeeping.
type entry struct {
	session    config.Session
	cooldownAt time.Time
	reason     string
	failures   int
	successes  int
}

// Pool rotates across sessions and tracks cooldowns.
type Pool struct {
	mu       sync.Mutex
	entries  []*entry
	next     int
	cooldown time.Duration
	now      func() time.Time
}

// New builds a pool from configuration.
func New(sessions []config.Session, cooldown time.Duration) *Pool {
	p := &Pool{cooldown: cooldown, now: time.Now}
	for _, s := range sessions {
		p.entries = append(p.entries, &entry{session: s})
	}
	return p
}

// Size reports how many sessions are configured.
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// Lease hands out the next healthy session. The returned release function
// reports the outcome so the pool can cool the session down on failure.
//
// A nil release is never returned alongside a nil error.
func (p *Pool) Lease() (config.Session, func(err error), error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.entries) == 0 {
		return config.Session{}, nil, ErrNoSession
	}

	now := p.now()
	var chosen *entry
	// Two passes: prefer a session that is outside its cooldown; if every
	// session is cooling, fall back to the one whose cooldown expires first
	// rather than failing outright — a stale cooldown is better than a 503.
	var soonest *entry
	for i := 0; i < len(p.entries); i++ {
		e := p.entries[(p.next+i)%len(p.entries)]
		if !e.cooling(now) {
			chosen = e
			p.next = (p.next + i + 1) % len(p.entries)
			break
		}
		if soonest == nil || e.cooldownAt.Before(soonest.cooldownAt) {
			soonest = e
		}
	}
	if chosen == nil {
		if soonest == nil {
			return config.Session{}, nil, ErrNoSession
		}
		chosen = soonest
		p.next = 0
	}

	sess := chosen.session
	released := false
	release := func(err error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if released {
			return
		}
		released = true
		if err == nil {
			chosen.successes++
			chosen.failures = 0
			return
		}
		var fatal *FatalError
		chosen.failures++
		if errors.As(err, &fatal) {
			chosen.cool(fatal.Reason, p.cooldown, p.now())
		}
	}
	return sess, release, nil
}

func (e *entry) cooling(now time.Time) bool {
	return !e.cooldownAt.IsZero() && now.Before(e.cooldownAt)
}

func (e *entry) cool(reason string, d time.Duration, now time.Time) {
	e.cooldownAt = now.Add(d)
	e.reason = reason
}

// Upsert adds or replaces a session by label.
//
// It exists so a freshly authorised account can be put to work without
// restarting the process, which would drop in-flight streams. An existing
// session with the same label keeps its health counters, since it is the same
// account being re-authorised rather than a new one.
func (p *Pool) Upsert(s config.Session) {
	if s.Label == "" {
		s.Label = "default"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.session.Label == s.Label {
			e.session = s
			// A re-authorised session gets a clean slate: the old cookies were
			// the problem, so any cooldown they earned must not carry over.
			e.cooldownAt = time.Time{}
			e.reason = ""
			e.failures = 0
			return
		}
	}
	p.entries = append(p.entries, &entry{session: s})
}

// Remove drops a session by label.
func (p *Pool) Remove(label string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.entries {
		if e.session.Label == label {
			p.entries = append(p.entries[:i], p.entries[i+1:]...)
			if p.next >= len(p.entries) {
				p.next = 0
			}
			return true
		}
	}
	return false
}

// Sessions returns the configured sessions without their health state. It
// never exposes cookie values.
func (p *Pool) Sessions() []Status { return p.Statuses() }

// FatalError marks a session as unusable rather than merely unlucky: bad
// cookies, a banned account, or an authorization failure. Retrying the same
// session would just burn the caller's latency.
type FatalError struct {
	Reason string
	Err    error
}

func (e *FatalError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *FatalError) Unwrap() error { return e.Err }

// Status is a snapshot of one session's health, safe to expose over HTTP.
// It never contains cookie values.
type Status struct {
	Label        string `json:"label"`
	CoolingDown  bool   `json:"cooling_down"`
	CooldownSecs int    `json:"cooldown_seconds_remaining"`
	Reason       string `json:"reason,omitempty"`
	Failures     int    `json:"consecutive_failures"`
	Successes    int    `json:"successes"`
}

// Statuses reports every session's health, ordered by label for stable output.
func (p *Pool) Statuses() []Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]Status, 0, len(p.entries))
	for _, e := range p.entries {
		st := Status{
			Label:       e.session.Label,
			Failures:    e.failures,
			Successes:   e.successes,
			Reason:      e.reason,
			CoolingDown: e.cooling(now),
		}
		if st.CoolingDown {
			st.CooldownSecs = int(e.cooldownAt.Sub(now).Seconds())
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}
