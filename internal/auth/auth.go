// Package auth identifies the caller of a public API request.
//
// AGENTS.md requires that access is limited and testable. This implementation
// is deliberately minimal and explicitly a development stand-in: it compares
// constant-time hashes of configured keys. A production deployment must
// replace it with a real identity provider, which is recorded in the ADR.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Errors returned by the authenticator.
var (
	// ErrMissingCredential means no usable credential was presented.
	ErrMissingCredential = errors.New("auth: credential is missing")
	// ErrInvalidCredential means the credential did not match. The same error
	// is returned for an unknown key and a wrong key, so the API cannot be
	// used to enumerate valid keys.
	ErrInvalidCredential = errors.New("auth: credential is invalid")
)

// contextKey is unexported so no other package can overwrite the value.
type contextKey struct{}

// Principal is the authenticated identity. It carries no secret material.
type Principal struct {
	UserID string
	// Scopes are coarse permissions granted to the credential.
	Scopes []string
}

// HasScope reports whether the principal holds a scope.
func (p Principal) HasScope(scope string) bool {
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}

	return false
}

// key is a hashed credential. The plaintext is never retained, so a heap dump
// or a debug print of the configuration cannot reveal it.
type key struct {
	digest [sha256.Size]byte
	userID string
	scopes []string
}

// Credential is one configured API key together with the identity it grants.
type Credential struct {
	// Key is the plaintext secret. It is hashed during construction and the
	// plaintext is not retained.
	Key string
	// UserID is the booking owner the key acts as.
	UserID string
	// Scopes are the coarse permissions granted to the key.
	Scopes []string
}

// Authenticator maps a presented credential to a Principal.
type Authenticator struct {
	keys []key
}

// New builds an authenticator from configured credentials.
func New(credentials []Credential) (*Authenticator, error) {
	if len(credentials) == 0 {
		return nil, fmt.Errorf("auth: at least one credential is required")
	}

	auth := &Authenticator{keys: make([]key, 0, len(credentials))}

	for _, c := range credentials {
		if c.Key == "" {
			return nil, fmt.Errorf("auth: empty key is not allowed")
		}

		if c.UserID == "" {
			return nil, fmt.Errorf("auth: user id is required for every credential")
		}

		auth.keys = append(auth.keys, key{
			digest: sha256.Sum256([]byte(c.Key)),
			userID: c.UserID,
			scopes: c.Scopes,
		})
	}

	return auth, nil
}

// Authenticate extracts and verifies a bearer token from the request.
func (a *Authenticator) Authenticate(r *http.Request) (Principal, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return Principal{}, ErrMissingCredential
	}

	const prefix = "Bearer "

	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return Principal{}, ErrMissingCredential
	}

	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return Principal{}, ErrMissingCredential
	}

	digest := sha256.Sum256([]byte(token))

	for _, k := range a.keys {
		// Constant-time comparison keeps the check free of timing signal.
		if subtle.ConstantTimeCompare(digest[:], k.digest[:]) == 1 {
			return Principal{UserID: k.userID, Scopes: k.scopes}, nil
		}
	}

	return Principal{}, ErrInvalidCredential
}

// FromContext returns the principal attached by Middleware.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(contextKey{}).(Principal)

	return p, ok
}

// WithPrincipal attaches a principal to a context. It is exported for tests
// that exercise handlers without the middleware.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, p)
}

// Middleware rejects unauthenticated requests before they reach a handler.
//
// An exempt path is passed through untouched. It is used for the liveness
// probe: an orchestrator has no credential, and a probe that fails with 401
// would restart a perfectly healthy service.
func (a *Authenticator) Middleware(exemptPath string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exemptPath != "" && r.URL.Path == exemptPath {
			next.ServeHTTP(w, r)

			return
		}

		p, err := a.Authenticate(r)
		if err != nil {
			// A single opaque code: the distinction between missing and
			// invalid is not useful to an attacker and only helps them.
			w.Header().Set("WWW-Authenticate", `Bearer realm="ai-journey-service"`)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"unauthorized"}}`)

			return
		}

		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// Fingerprint returns a short, non-reversible identifier for a credential, so
// an operator can correlate log lines without the secret ever being written.
func Fingerprint(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))

	return hex.EncodeToString(sum[:4])
}
