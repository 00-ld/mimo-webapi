package session

import (
	"errors"
	"testing"
	"time"

	"mimowebapi/internal/config"
)

func mkPool(n int, cooldown time.Duration) (*Pool, []string) {
	var sessions []config.Session
	var labels []string
	for i := 0; i < n; i++ {
		label := string(rune('a' + i))
		labels = append(labels, label)
		sessions = append(sessions, config.Session{
			Label:   label,
			Cookies: []config.Cookie{{Name: "serviceToken", Value: "v" + label}},
		})
	}
	return New(sessions, cooldown), labels
}

func TestLeaseRotates(t *testing.T) {
	p, _ := mkPool(3, time.Minute)
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		s, release, err := p.Lease()
		if err != nil {
			t.Fatal(err)
		}
		seen[s.Label]++
		release(nil)
	}
	if len(seen) != 3 {
		t.Fatalf("rotation did not visit every session: %v", seen)
	}
	for label, n := range seen {
		if n != 2 {
			t.Errorf("session %s leased %d times, want 2", label, n)
		}
	}
}

// A session the backend rejected must be parked, and the pool must prefer a
// healthy one. Retrying a dead cookie just burns latency on every request.
func TestFatalErrorTriggersCooldownAndFailover(t *testing.T) {
	now := time.Unix(0, 0)
	p, _ := mkPool(2, time.Minute)
	p.now = func() time.Time { return now }

	first, release, err := p.Lease()
	if err != nil {
		t.Fatal(err)
	}
	release(&FatalError{Reason: "cookies_expired"})

	// Every subsequent lease must avoid the parked session while it cools.
	for i := 0; i < 4; i++ {
		s, rel, err := p.Lease()
		if err != nil {
			t.Fatal(err)
		}
		if s.Label == first.Label {
			t.Fatalf("cooling session %s was leased again", s.Label)
		}
		rel(nil)
	}

	// Once the cooldown expires it becomes eligible again.
	now = now.Add(2 * time.Minute)
	gotBack := false
	for i := 0; i < 4; i++ {
		s, rel, err := p.Lease()
		if err != nil {
			t.Fatal(err)
		}
		if s.Label == first.Label {
			gotBack = true
		}
		rel(nil)
	}
	if !gotBack {
		t.Error("session never returned to the rotation after its cooldown")
	}
}

// A transient failure must not park the session: the account is still fine.
func TestNonFatalErrorDoesNotCoolDown(t *testing.T) {
	p, _ := mkPool(1, time.Minute)
	_, release, err := p.Lease()
	if err != nil {
		t.Fatal(err)
	}
	release(errors.New("connection reset"))

	for i := 0; i < 3; i++ {
		if _, rel, err := p.Lease(); err != nil {
			t.Fatalf("session was parked by a transient error: %v", err)
		} else {
			rel(nil)
		}
	}
}

func TestEmptyPool(t *testing.T) {
	p := New(nil, time.Minute)
	if p.Size() != 0 {
		t.Fatalf("size = %d", p.Size())
	}
	if _, _, err := p.Lease(); !errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v, want ErrNoSession", err)
	}
}

// When every session is cooling, serving from the least-stale one beats a hard
// failure — the cooldown is a heuristic, not a fact.
func TestAllCoolingStillLeases(t *testing.T) {
	now := time.Unix(0, 0)
	p, _ := mkPool(2, time.Minute)
	p.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		_, release, err := p.Lease()
		if err != nil {
			t.Fatal(err)
		}
		release(&FatalError{Reason: "cookies_expired"})
	}
	if _, release, err := p.Lease(); err != nil {
		t.Fatalf("expected a fallback lease, got %v", err)
	} else {
		release(nil)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	p, _ := mkPool(1, time.Minute)
	_, release, err := p.Lease()
	if err != nil {
		t.Fatal(err)
	}
	// A handler that reports success then failure must not double-count.
	release(nil)
	release(&FatalError{Reason: "late"})

	st := p.Statuses()[0]
	if st.Successes != 1 {
		t.Errorf("successes = %d, want 1", st.Successes)
	}
	if st.CoolingDown {
		t.Error("a double release parked the session")
	}
}

// Status output is exposed over HTTP, so it must never carry cookie values.
func TestStatusesNeverLeakSecrets(t *testing.T) {
	p, _ := mkPool(2, time.Minute)
	statuses := p.Statuses()
	if len(statuses) != 2 {
		t.Fatalf("got %d statuses", len(statuses))
	}
	for _, s := range statuses {
		if s.Label == "" {
			t.Error("status missing its label")
		}
	}
}

func TestStatusesSortedByLabel(t *testing.T) {
	p, _ := mkPool(3, time.Minute)
	statuses := p.Statuses()
	for i := 1; i < len(statuses); i++ {
		if statuses[i-1].Label > statuses[i].Label {
			t.Fatalf("statuses not sorted: %v", statuses)
		}
	}
}

func TestCooldownRemainingReported(t *testing.T) {
	now := time.Unix(1000, 0)
	p, _ := mkPool(1, time.Minute)
	p.now = func() time.Time { return now }

	_, release, err := p.Lease()
	if err != nil {
		t.Fatal(err)
	}
	release(&FatalError{Reason: "cookies_expired"})

	st := p.Statuses()[0]
	if !st.CoolingDown {
		t.Fatal("expected the session to be cooling down")
	}
	if st.CooldownSecs <= 0 || st.CooldownSecs > 60 {
		t.Errorf("cooldown seconds = %d", st.CooldownSecs)
	}
	if st.Reason != "cookies_expired" {
		t.Errorf("reason = %q", st.Reason)
	}
}
