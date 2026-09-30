package auth_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/rockbotum/ai-journey-service/internal/auth"
)

const key = "correct-horse-battery-staple"

func newAuth(t *testing.T) *auth.Authenticator {
	t.Helper()

	a, err := auth.New([]auth.Credential{{Key: key, UserID: "user-1", Scopes: []string{"bookings:write"}}})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	return a
}

func TestValidBearerTokenAuthenticates(t *testing.T) {
	a := newAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/bookings", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	p, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	if p.UserID != "user-1" {
		t.Fatalf("UserID = %q, want user-1", p.UserID)
	}

	if !p.HasScope("bookings:write") {
		t.Fatalf("scopes = %v, want bookings:write", p.Scopes)
	}

	if p.HasScope("bookings:delete") {
		t.Fatal("a scope that was not granted must not be reported")
	}
}

func TestMissingAndWrongCredentialsAreRejected(t *testing.T) {
	a := newAuth(t)

	cases := map[string]string{
		"no header":          "",
		"wrong scheme":       "Basic " + key,
		"bearer without key": "Bearer ",
		"wrong key":          "Bearer wrong-key",
		"prefix of key":      "Bearer " + key[:10],
	}

	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/bookings", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}

			if _, err := a.Authenticate(req); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

// An unknown key and a wrong key must be indistinguishable, so the endpoint
// cannot be used to enumerate which credentials are valid.
func TestUnknownAndWrongKeyAreIndistinguishable(t *testing.T) {
	a := newAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/bookings", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-key")

	_, err := a.Authenticate(req)
	if err != auth.ErrInvalidCredential {
		t.Fatalf("err = %v, want ErrInvalidCredential", err)
	}
}

func TestSchemeIsCaseInsensitive(t *testing.T) {
	a := newAuth(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/bookings", nil)
	req.Header.Set("Authorization", "bearer "+key)

	if _, err := a.Authenticate(req); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
}

func TestNewRejectsUnusableConfiguration(t *testing.T) {
	if _, err := auth.New(nil); err == nil {
		t.Fatal("want an error for an empty credential set")
	}

	if _, err := auth.New([]auth.Credential{{Key: "", UserID: "u"}}); err == nil {
		t.Fatal("want an error for an empty key")
	}

	if _, err := auth.New([]auth.Credential{{Key: key, UserID: ""}}); err == nil {
		t.Fatal("want an error for a missing user id")
	}
}

func TestMiddlewareAttachesPrincipal(t *testing.T) {
	a := newAuth(t)

	var seen string

	h := a.Middleware("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.FromContext(r.Context())
		if !ok {
			t.Error("principal is missing from the context")

			return
		}

		seen = p.UserID
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/bookings", nil)
	req.Header.Set("Authorization", "Bearer "+key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	if seen != "user-1" {
		t.Fatalf("handler saw user %q, want user-1", seen)
	}
}

func TestMiddlewareBlocksUnauthenticatedRequest(t *testing.T) {
	a := newAuth(t)

	called := false

	h := a.Middleware("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bookings", nil))

	if called {
		t.Fatal("the handler must not run for an unauthenticated request")
	}

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("a 401 must carry a WWW-Authenticate header")
	}
}

// The 401 body must not reveal whether a key exists.
func TestUnauthorizedBodyIsOpaque(t *testing.T) {
	a := newAuth(t)

	h := a.Middleware("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/v1/bookings", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "invalid") || strings.Contains(body, "wrong") {
		t.Fatalf("401 body reveals detail: %s", body)
	}
}

// The authenticator must store only a hash, so a debug dump or a heap dump of
// the process cannot expose a usable credential.
func TestOnlyTheHashIsRetained(t *testing.T) {
	a := newAuth(t)

	digests := storedDigests(t, a)
	if len(digests) != 1 {
		t.Fatalf("stored %d keys, want 1", len(digests))
	}

	want := sha256.Sum256([]byte(key))
	if !bytes.Equal(digests[0], want[:]) {
		t.Fatal("the stored value is not the hash of the key")
	}

	// The key struct holds a digest and identity strings only. There is no
	// field that could hold the plaintext, so hashing is what protects it.
	if strings.Contains(identityFields(t, a), key) {
		t.Fatal("the plaintext key is retained alongside the hash")
	}
}

func TestFingerprintIsStableAndShort(t *testing.T) {
	first := auth.Fingerprint(key)
	second := auth.Fingerprint(key)

	if first != second {
		t.Fatal("fingerprint is not deterministic")
	}

	if first == auth.Fingerprint("other-key") {
		t.Fatal("different keys must fingerprint differently")
	}

	if len(first) > 16 {
		t.Fatalf("fingerprint is too long to be a log tag: %d", len(first))
	}

	if strings.Contains(first, key) {
		t.Fatal("fingerprint leaks the credential")
	}
}

func TestPrincipalRoundTripsThroughContext(t *testing.T) {
	want := auth.Principal{UserID: "user-2"}

	ctx := auth.WithPrincipal(context.Background(), want)

	got, ok := auth.FromContext(ctx)
	if !ok {
		t.Fatal("principal is missing")
	}

	if got.UserID != want.UserID {
		t.Fatalf("UserID = %q, want %q", got.UserID, want.UserID)
	}

	if _, ok := auth.FromContext(context.Background()); ok {
		t.Fatal("a bare context must not carry a principal")
	}
}

// storedDigests reads the digests held by the authenticator.
//
// Reflection cannot call Interface on an unexported field, so each digest byte
// is read by index instead of materialising the value.
func storedDigests(t *testing.T, a *auth.Authenticator) [][]byte {
	t.Helper()

	keys := reflect.ValueOf(a).Elem().Field(0)
	out := make([][]byte, 0, keys.Len())

	for i := range keys.Len() {
		digest := keys.Index(i).Field(0)

		buf := make([]byte, digest.Len())
		for j := range buf {
			buf[j] = byte(digest.Index(j).Uint())
		}

		out = append(out, buf)
	}

	return out
}

// identityFields renders the non-digest fields so a leak assertion has
// something to inspect.
func identityFields(t *testing.T, a *auth.Authenticator) string {
	t.Helper()

	keys := reflect.ValueOf(a).Elem().Field(0)

	var b strings.Builder

	for i := range keys.Len() {
		elem := keys.Index(i)
		for f := 1; f < elem.NumField(); f++ {
			fmt.Fprintf(&b, "%v ", elem.Field(f))
		}
	}

	return b.String()
}
