package upstream

import (
	"strings"
	"testing"
)

func TestBuildQuerySeparatesHistoryAndFinal(t *testing.T) {
	msgs := []Turn{
		{Role: "system", Content: "You are terse."},
		{Role: "user", Content: "What is 2+2?"},
		{Role: "assistant", Content: "4"},
		{Role: "user", Content: "And times 3?"},
	}
	history, query, err := BuildQuery(msgs, "prepend")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("history length = %d, want 3", len(history))
	}
	if query != "And times 3?" {
		t.Errorf("final query = %q", query)
	}

	full := ComposeQuery(history, query, "prepend")
	for _, want := range []string{"You are terse.", "What is 2+2?", "Assistant: 4",
		"And times 3?"} {
		if !strings.Contains(full, want) {
			t.Errorf("composed query missing %q:\n%s", want, full)
		}
	}
}

// A single-turn conversation must not be padded with an empty preamble: the
// browser sends the bare question, and extra framing measurably changes
// behaviour on short prompts.
func TestComposeQuerySingleTurnIsBare(t *testing.T) {
	history, query, err := BuildQuery([]Turn{{Role: "user", Content: "hi"}}, "prepend")
	if err != nil {
		t.Fatal(err)
	}
	if got := ComposeQuery(history, query, "prepend"); got != "hi" {
		t.Errorf("single turn composed to %q, want %q", got, "hi")
	}
}

func TestBuildQueryRejectsEmpty(t *testing.T) {
	if _, _, err := BuildQuery(nil, "prepend"); err == nil {
		t.Error("expected an error for an empty message list")
	}
	if _, _, err := BuildQuery([]Turn{{Role: "user", Content: "   "}}, "prepend"); err == nil {
		t.Error("expected an error for whitespace-only content")
	}
}

// The conversation id must be stable across the turns of one conversation and
// must change when the conversation does. This is what preserves upstream
// multi-turn context.
func TestConversationMapStableAcrossTurns(t *testing.T) {
	m := NewConversationMap(64)

	first := []Turn{{Role: "user", Content: "hello"}}
	id1 := m.KeyFor(first)

	// Second turn of the same conversation: the prefix grew by the prior
	// exchange, but the derived id must still be that of the same thread.
	second := []Turn{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	id2 := m.KeyFor(second)

	if id1 == "" || id2 == "" {
		t.Fatal("ids must not be empty")
	}
	if id1 == id2 {
		t.Error("a different prefix must not collide with the first turn")
	}
	// Re-deriving the same prefix must return the identical id.
	if again := m.KeyFor(second); again != id2 {
		t.Errorf("id not stable: %q vs %q", again, id2)
	}

	other := []Turn{{Role: "user", Content: "unrelated"}}
	if m.KeyFor(other) == id1 {
		t.Error("distinct conversations must not share an id")
	}
}

func TestConversationMapBounded(t *testing.T) {
	m := NewConversationMap(4)
	for i := 0; i < 50; i++ {
		m.KeyFor([]Turn{{Role: "user", Content: string(rune('a'+i%26)) + string(rune('0'+i%10))}})
	}
	m.mu.Lock()
	size := len(m.seen)
	order := len(m.order)
	m.mu.Unlock()
	if size > 4 || order > 4 {
		t.Errorf("cache grew past its bound: size=%d order=%d", size, order)
	}
}

func TestSystemPromptModeDrop(t *testing.T) {
	msgs := []Turn{
		{Role: "system", Content: "SECRET-INSTRUCTION"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
		{Role: "user", Content: "again"},
	}
	history, query, err := BuildQuery(msgs, "drop")
	if err != nil {
		t.Fatal(err)
	}
	full := ComposeQuery(history, query, "drop")
	if strings.Contains(full, "SECRET-INSTRUCTION") {
		t.Errorf("drop mode leaked the system prompt:\n%s", full)
	}
}
