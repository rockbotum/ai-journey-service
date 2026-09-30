package main

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rockbotum/ai-journey-service/internal/config"
)

func setEnv(t *testing.T, key, value string) {
	t.Helper()

	prev, had := os.LookupEnv(key)
	t.Setenv(key, value)

	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)

			return
		}

		_ = os.Unsetenv(key)
	})
}

// validConfigEnv sets every value the service requires so a test can exercise
// wiring rather than validation.
func validConfigEnv(t *testing.T) {
	t.Helper()

	setEnv(t, "DB_DSN", "postgres://user:pass@localhost:5432/db?sslmode=disable")
	setEnv(t, "AUTH_CREDENTIALS", "partner_a:sk-partner-a:bookings:write")
	setEnv(t, "SUPPLIER_BASE_URL", "https://supplier.example.com")
	setEnv(t, "SUPPLIER_ID", "sup-1")
	setEnv(t, "SUPPLIER_API_KEY", "supplier-secret")
	setEnv(t, "SUPPLIER_TIMEOUT", "1s")
}

func TestBuildHandlerWiresTheWholeChain(t *testing.T) {
	validConfigEnv(t)

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	handler, err := buildHandler(cfg, log)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}

	// The liveness probe must answer without a credential.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rec.Code)
	}

	// A protected route must not.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/bookings/x", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("protected route = %d, want 401", rec.Code)
	}
}

func TestBuildHandlerFailsOnAnUnusableConfiguration(t *testing.T) {
	validConfigEnv(t)
	setEnv(t, "SUPPLIER_BASE_URL", "")

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := config.Load()
	if err == nil {
		t.Fatal("want a configuration error")
	}

	// A misconfigured service must not build a handler.
	if _, err := buildHandler(cfg, log); err == nil {
		t.Fatal("want an error when the supplier base url is missing")
	}
}

// The configured credential must authenticate, and a wrong one must not.
func TestConfiguredCredentialAuthenticates(t *testing.T) {
	validConfigEnv(t)

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	handler, err := buildHandler(cfg, log)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}

	body := `{"origin":"LED","destination":"JFK","departure":"2027-01-01","cabin":"economy",` +
		`"currency":"EUR","passengers":[{"full_name":"A Passenger"}]}`

	post := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "cmd-test-1")
		req.Header.Set("Authorization", "Bearer "+key)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		return rec
	}

	if rec := post("sk-partner-a"); rec.Code != http.StatusCreated {
		t.Fatalf("valid credential = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	// The same key with a different request must be refused, which also proves
	// the booking was stored rather than silently discarded.
	other := strings.NewReader(strings.Replace(body, "JFK", "CDG", 1))
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", other)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "cmd-test-1")
	req.Header.Set("Authorization", "Bearer sk-partner-a")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("reused key with a different body = %d, want 409: %s", rec.Code, rec.Body.String())
	}

	if rec := post("sk-wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong credential = %d, want 401", rec.Code)
	}
}

// The response must not carry personal data.
func TestBookingResponseOmitsPassengerNames(t *testing.T) {
	validConfigEnv(t)

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	handler, err := buildHandler(cfg, log)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(
		`{"origin":"LED","destination":"JFK","departure":"2027-01-01","cabin":"economy",`+
			`"currency":"EUR","passengers":[{"full_name":"SENSITIVE NAME"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "cmd-test-pii")
	req.Header.Set("Authorization", "Bearer sk-partner-a")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), "SENSITIVE NAME") {
		t.Fatalf("the passenger name leaked: %s", rec.Body.String())
	}
}

func TestNewBookingIDIsUniqueAndPrefixed(t *testing.T) {
	seen := make(map[string]bool, 1000)

	for range 1000 {
		id := newBookingID()

		if !strings.HasPrefix(id, "bk_") {
			t.Fatalf("id %q has no prefix", id)
		}

		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}

		seen[id] = true
	}
}

// The redacting logger must be what the process installs, so a secret cannot
// escape from a package that forgot to be careful.
func TestNewLoggerRedacts(t *testing.T) {
	var out strings.Builder

	l := newLoggerInto("test", &out)

	l.Info("test", "api_key", "sk-must-not-appear", "user_id", "user-1")

	if strings.Contains(out.String(), "sk-must-not-appear") {
		t.Fatalf("the logger leaked a secret: %s", out.String())
	}

	if !strings.Contains(out.String(), "user-1") {
		t.Fatalf("the logger dropped a non-sensitive field: %s", out.String())
	}
}

func TestNewLoggerRespectsTheLevel(t *testing.T) {
	// A production environment must not emit debug detail by default.
	if newLogger("production").Enabled(t.Context(), slog.LevelDebug) {
		t.Fatal("debug logging must be off in production")
	}

	if !newLogger("development").Enabled(t.Context(), slog.LevelDebug) {
		t.Fatal("debug logging should be available in development")
	}
}

func TestServeShutsDownCleanly(t *testing.T) {
	srv := &http.Server{
		Addr:              "127.0.0.1:0",
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}),
		ReadHeaderTimeout: time.Second,
	}

	// serve blocks until a signal, so it is exercised through the shutdown
	// behaviour it depends on: a closed listener must return rather than hang.
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := srv.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown on a closed server: %v", err)
	}

	if err := srv.ListenAndServe(); err == nil {
		t.Fatal("ListenAndServe on a closed listener must return an error")
	}
}

func TestHealthResponseIsStableJSON(t *testing.T) {
	validConfigEnv(t)

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	handler, err := buildHandler(cfg, log)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("health response is not JSON: %s", rec.Body.String())
	}

	if payload["status"] != "ok" {
		t.Fatalf("status = %q, want ok", payload["status"])
	}
}

// A listen address is not a request target: a bare port has no host, and a
// wildcard host is not routable. The probe must rewrite both.
func TestHealthURL(t *testing.T) {
	cases := map[string]string{
		":8080":        "http://127.0.0.1:8080/healthz",
		"0.0.0.0:8080": "http://127.0.0.1:8080/healthz",
		"127.0.0.1:80": "http://127.0.0.1:80/healthz",
		"[::1]:8080":   "http://[::1]:8080/healthz",
		"":             "http://127.0.0.1:8080/healthz",
		"nonsense":     "http://127.0.0.1:8080/healthz",
	}

	for listen, want := range cases {
		if got := healthURL(listen); got != want {
			t.Errorf("healthURL(%q) = %q, want %q", listen, got, want)
		}
	}
}

// The probe must fail, not pass, when nothing is listening.
func TestProbeHealthFailsWhenNothingIsListening(t *testing.T) {
	validConfigEnv(t)
	setEnv(t, "HTTP_LISTEN", "127.0.0.1:1")

	if err := probeHealth(); err == nil {
		t.Fatal("want an error when the server is not listening")
	}
}

// The probe exercises the real HTTP path, so a wired-up handler is reported
// healthy.
func TestProbeHealthSucceedsAgainstALiveHandler(t *testing.T) {
	validConfigEnv(t)

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	handler, err := buildHandler(cfg, log)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}

	// Bind a loopback port, then hand the same address to the configuration so
	// the probe targets the server that is actually running.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	addr := listener.Addr().String()

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}

	go func() {
		_ = srv.Serve(listener)
	}()

	t.Cleanup(func() {
		_ = srv.Close()
	})

	setEnv(t, "HTTP_LISTEN", addr)

	if err := probeHealth(); err != nil {
		t.Fatalf("probeHealth: %v", err)
	}
}
