package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Conversation identity.
//
// The web backend keeps per-conversation state server-side. An OpenAI client
// sends a flat message array with no conversation id, so we derive one from
// the leading messages. When a client keeps appending to the same
// conversation, the prefix is stable and we reuse the upstream conversation —
// which is both faster and preserves native multi-turn behaviour. When the
// client starts a fresh conversation, the prefix changes and we get a new id.

// ConversationMap derives stable upstream conversation ids from message
// prefixes, with a bounded cache so a long-running process cannot grow
// without limit.
type ConversationMap struct {
	mu    sync.Mutex
	seen  map[string]string
	order []string
	max   int
}

// NewConversationMap builds a map holding at most max entries.
func NewConversationMap(max int) *ConversationMap {
	if max <= 0 {
		max = 512
	}
	return &ConversationMap{seen: map[string]string{}, max: max}
}

// KeyFor returns the upstream conversation id for a message prefix.
//
// The prefix is everything except the final turn, so the first turn of a
// conversation and every continuation of it map to the same id.
func (m *ConversationMap) KeyFor(history []Turn) string {
	if len(history) == 0 {
		return ""
	}
	h := sha256.New()
	for _, t := range history {
		h.Write([]byte(t.Role))
		h.Write([]byte{0})
		h.Write([]byte(t.Content))
		h.Write([]byte{1})
	}
	key := hex.EncodeToString(h.Sum(nil)[:16])

	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.seen[key]; ok {
		return id
	}
	id := "conv-" + key[:24]
	m.seen[key] = id
	m.order = append(m.order, key)
	if len(m.order) > m.max {
		drop := m.order[0]
		m.order = m.order[1:]
		delete(m.seen, drop)
	}
	return id
}

// Turn is one normalized conversation turn.
type Turn struct {
	Role    string
	Content string
}

// BuildQuery folds an OpenAI-style message list into the single query string
// the web backend accepts.
//
// The web protocol has exactly one place for text: `query`. Roles are
// therefore rendered as labelled blocks so the model can still tell a system
// instruction from a user turn from its own earlier reply.
func BuildQuery(msgs []Turn, systemMode string) (history []Turn, query string, err error) {
	if len(msgs) == 0 {
		return nil, "", fmt.Errorf("no messages provided")
	}

	// A trailing assistant message with no following user turn means the
	// client is asking us to continue/complete it; treat it as the prompt.
	history = msgs[:len(msgs)-1]
	last := msgs[len(msgs)-1]
	query = renderTurn(last, true)

	if last.Role == "system" {
		// A trailing system message cannot be prompted on meaningfully.
		return nil, "", fmt.Errorf("final message must be from user or assistant")
	}
	if strings.TrimSpace(query) == "" {
		return nil, "", fmt.Errorf("final message has no text content")
	}
	return history, query, nil
}

// renderTurn formats one turn. The first turn is left unlabelled when it is a
// user turn, which is what a browser session would send for a plain question.
func renderTurn(t Turn, first bool) string {
	body := strings.TrimSpace(t.Content)
	switch t.Role {
	case "system":
		return "[System instruction]\n" + body
	case "assistant":
		return "[Previous assistant reply]\n" + body
	default:
		if first {
			return body
		}
		return "[User]\n" + body
	}
}

// renderHistory formats prior turns into the context preamble.
func renderHistory(history []Turn, systemMode string) string {
	if len(history) == 0 {
		return ""
	}
	var b strings.Builder
	systems := make([]string, 0, 2)
	for _, t := range history {
		if t.Role == "system" {
			if systemMode == "drop" {
				continue
			}
			systems = append(systems, strings.TrimSpace(t.Content))
			continue
		}
	}
	if len(systems) > 0 {
		b.WriteString("## Instructions\n")
		b.WriteString(strings.Join(systems, "\n\n"))
		b.WriteString("\n\n")
	}
	b.WriteString("## Conversation so far\n")
	for _, t := range history {
		if t.Role == "system" {
			continue
		}
		label := "User"
		if t.Role == "assistant" {
			label = "Assistant"
		}
		body := strings.TrimSpace(t.Content)
		if body == "" {
			continue
		}
		b.WriteString(label)
		b.WriteString(": ")
		b.WriteString(body)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// ComposeQuery joins the conversation preamble and the final turn.
func ComposeQuery(history []Turn, query, systemMode string) string {
	pre := renderHistory(history, systemMode)
	if pre == "" {
		return query
	}
	return pre + "\n\n## Current message\n" + query
}

// SortedKeys is a small helper used in tests and debugging output.
func SortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
