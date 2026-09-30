package upstream

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// errReader returns some bytes, then a hard error instead of EOF.
type errReader struct {
	data string
	done bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		n := copy(p, r.data)
		return n, nil
	}
	return 0, errors.New("connection reset by peer")
}

// A read failure must be reported, not swallowed as a clean end of stream.
func TestParseSSEReportsReadError(t *testing.T) {
	r := &errReader{data: "event: message\ndata: {\"content\":\"partial\"}\n\n"}
	ch := make(chan Frame, 4)
	errCh := make(chan error, 1)
	go func() { defer close(ch); parseSSE(context.Background(), r, ch, errCh) }()

	var frames int
	for range ch {
		frames++
	}
	if frames != 1 {
		t.Fatalf("frames = %d, want 1", frames)
	}
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "read failed") {
			t.Fatalf("unexpected error: %v", err)
		}
	default:
		t.Fatal("read error was swallowed; a truncated stream looks like success")
	}
}

// A clean end of stream must NOT be reported as an error.
func TestParseSSENoErrorOnCleanEOF(t *testing.T) {
	ch := make(chan Frame, 4)
	errCh := make(chan error, 1)
	go func() {
		defer close(ch)
		parseSSE(context.Background(),
			strings.NewReader("event: message\ndata: {\"content\":\"hi\"}\n\n"), ch, errCh)
	}()
	for range ch {
	}
	select {
	case err := <-errCh:
		t.Fatalf("clean EOF reported as error: %v", err)
	default:
	}
}
