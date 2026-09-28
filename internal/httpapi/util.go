package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
)

// readBody reads the request body with a hard size cap.
//
// The cap is enforced with MaxBytesReader so an oversized request is rejected
// while it is still being read, rather than after it has been buffered.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, errors.New("empty request body")
	}
	if limit <= 0 {
		limit = 16 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errors.New("request body exceeds the configured limit")
		}
		return nil, err
	}
	if len(body) == 0 {
		return nil, errors.New("empty request body")
	}
	return body, nil
}

// constTimeEqual compares two secrets in constant time.
//
// Length is allowed to leak: the token length is fixed by configuration and
// not itself secret, whereas the byte-by-byte comparison must not short-circuit.
func constTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		// Still consume the same code path to keep timing flat for equal-length
		// candidates, which is the interesting case for an attacker.
		_ = subtle.ConstantTimeCompare([]byte(a), []byte(a))
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// identityKey is the context key under which the caller's identity is stored.
type identityContextKey struct{}

// withIdentity attaches the resolved identity to a request context.
func withIdentity(ctx context.Context, id *identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, id)
}

// identityFrom retrieves the caller's identity, if any.
func identityFrom(ctx context.Context) *identity {
	id, _ := ctx.Value(identityContextKey{}).(*identity)
	return id
}
