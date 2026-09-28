package httpapi

import "strings"

// The upstream wraps its chain of thought in <think> ... </think> markers
// inside the normal text stream, interleaved with the answer. A client that
// receives it verbatim shows the model's scratchpad as the answer, and an
// agent that consumes output_text as a final result will act on reasoning
// rather than on the reply.
//
// The markers are stripped here rather than upstream because the relay is
// already the component that knows which protocol the caller speaks: the
// Responses API has no field for reasoning text, while chat completions can
// carry it in reasoning_content.

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// splitThink separates reasoning from answer text.
//
// The upstream interleaves a NUL byte after each marker, and the same text can
// arrive split across chunks, so this works on characters rather than lines.
//
// It returns the reasoning text and the answer text. When no marker is present
// the whole input is the answer, which is the common case for models that do
// not emit a scratchpad.
func splitThink(s string) (reasoning, answer string) {
	if s == "" {
		return "", ""
	}
	if !strings.Contains(s, thinkOpen) && !strings.Contains(s, thinkClose) {
		return "", s
	}

	var r, a strings.Builder
	rest := s
	for {
		start := strings.Index(rest, thinkOpen)
		if start < 0 {
			a.WriteString(rest)
			break
		}
		// Text before the marker is answer text.
		a.WriteString(rest[:start])
		body := rest[start+len(thinkOpen):]
		end := strings.Index(body, thinkClose)
		if end < 0 {
			// An unterminated block: the rest is reasoning. This happens when
			// a stream is cut short mid-thought.
			r.WriteString(stripNUL(body))
			break
		}
		r.WriteString(stripNUL(body[:end]))
		rest = body[end+len(thinkClose):]
	}
	return strings.TrimLeft(r.String(), "\x00"), stripNUL(a.String())
}

// stripNUL removes the separator byte the upstream emits after each marker.
func stripNUL(s string) string { return strings.ReplaceAll(s, "\x00", "") }

// thinkStripper filters reasoning out of a token stream.
//
// A marker can straddle two chunks, so a naive per-chunk strip leaks partial
// tags into the answer. The buffer therefore holds back the trailing bytes
// that could still turn out to be the start of a marker.
//
// Buffering is only engaged once a marker is actually suspected. A stream with
// no markers — the common case for a model that emits no scratchpad — passes
// through chunk for chunk, so the relay does not silently re-chunk a stream
// its client is rendering incrementally.
type thinkStripper struct {
	inThink bool
	pending string
	// armed is set once any '<' has been seen. Until then there is nothing a
	// marker could be split across and no reason to hold bytes back.
	armed bool
}

// safeCut reports how many leading bytes of s are certainly not the start of
// a marker, so they can be emitted without waiting.
//
// A byte can only be the start of a marker if it is '<'. When no '<' appears
// in the tail, everything is safe and nothing needs buffering.
func safeCut(s string) int {
	lt := strings.LastIndexByte(s, '<')
	if lt < 0 {
		// No possible marker start: release everything except a trailing NUL,
		// which may be the separator belonging to a marker in the next chunk.
		if n := len(s); n > 0 && s[n-1] == 0 {
			return n - 1
		}
		return len(s)
	}
	if lt < len(s)-maxPartialMarker() {
		// The '<' is far enough back that it cannot be a partial marker.
		return len(s)
	}
	return lt
}

// maxPartialMarker is the longest suffix of a marker prefix worth holding.
func maxPartialMarker() int {
	if len(thinkClose) > len(thinkOpen) {
		return len(thinkClose)
	}
	return len(thinkOpen)
}

// push splits one chunk into reasoning and answer text.
//
// The chat completions API can carry reasoning in its own field, so the two
// halves are returned separately there; callers that have nowhere to put the
// reasoning simply ignore the first value.
func (t *thinkStripper) pushSplit(chunk string) (reasoning, answer string) {
	t.pending += chunk
	var r, a strings.Builder

	for {
		if t.inThink {
			idx := strings.Index(t.pending, thinkClose)
			if idx < 0 {
				// Keep only what could still be a partial closer.
				if len(t.pending) > maxPartialMarker() {
					emit := t.pending[:len(t.pending)-maxPartialMarker()]
					r.WriteString(stripNUL(emit))
					t.pending = t.pending[len(t.pending)-maxPartialMarker():]
				}
				return r.String(), a.String()
			}
			r.WriteString(stripNUL(t.pending[:idx]))
			t.pending = t.pending[idx+len(thinkClose):]
			t.inThink = false
			continue
		}

		idx := strings.Index(t.pending, thinkOpen)
		if idx < 0 {
			// Hold bytes back only when the tail could still become a marker.
			cut := safeCut(t.pending)
			a.WriteString(stripNUL(t.pending[:cut]))
			t.pending = t.pending[cut:]
			return r.String(), a.String()
		}
		a.WriteString(stripNUL(t.pending[:idx]))
		t.pending = t.pending[idx+len(thinkOpen):]
		t.inThink = true
	}
}

// push feeds one chunk and returns the answer text that is safe to emit now.
func (t *thinkStripper) push(chunk string) string {
	t.pending += chunk
	var out strings.Builder

	for {
		if t.inThink {
			idx := strings.Index(t.pending, thinkClose)
			if idx < 0 {
				// Keep only what could still be a partial closer.
				if len(t.pending) > maxPartialMarker() {
					t.pending = t.pending[len(t.pending)-maxPartialMarker():]
				}
				return out.String()
			}
			t.pending = t.pending[idx+len(thinkClose):]
			t.inThink = false
			continue
		}

		idx := strings.Index(t.pending, thinkOpen)
		if idx < 0 {
			// Emit everything that cannot be part of a marker. The NUL
			// separators the upstream emits after each marker are removed here
			// too, not only at flush: a separator that reaches the client
			// mid-stream corrupts the text just as badly as one at the end.
			cut := safeCut(t.pending)
			out.WriteString(stripNUL(t.pending[:cut]))
			t.pending = t.pending[cut:]
			return out.String()
		}
		out.WriteString(stripNUL(t.pending[:idx]))
		t.pending = t.pending[idx+len(thinkOpen):]
		t.inThink = true
	}
}

// flush returns whatever is still buffered once the stream ends.
func (t *thinkStripper) flush() string {
	rest := stripNUL(t.pending)
	t.pending = ""
	if t.inThink {
		// The stream ended inside a reasoning block, so there is no answer
		// text to release.
		return ""
	}
	// A dangling partial marker is answer text, not a tag.
	return rest
}
