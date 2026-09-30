package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rockbotum/ai-journey-service/internal/auth"
	"github.com/rockbotum/ai-journey-service/internal/booking"
	"github.com/rockbotum/ai-journey-service/internal/httpapi"
	"github.com/rockbotum/ai-journey-service/internal/service"
	"github.com/rockbotum/ai-journey-service/internal/store"
)

const testKey = "test-key-value"

var testNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type harness struct {
	handler  http.Handler
	clock    *fakeClock
	provider *fakeProvider
	logs     *bytes.Buffer
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) nowFn() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newHarness(t *testing.T, limit int) *harness {
	t.Helper()

	st := store.NewMemory()

	clock := &fakeClock{now: testNow}

	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ids := 0

	prov := newFakeProvider(clock)

	svc, err := service.New(st, prov, log, clock.nowFn, func() string {
		ids++

		return fmt.Sprintf("bk_test_%d", ids)
	})
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}

	authn, err := auth.New([]auth.Credential{{Key: testKey, UserID: "user-1"}})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	h, err := httpapi.New(svc, authn, log, httpapi.Options{
		MaxBodyBytes:      4096,
		RequestsPerMinute: limit,
		Now:               clock.nowFn,
	})
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}

	return &harness{handler: h, clock: clock, provider: prov, logs: logs}
}

// bumpSupplierPrice raises the quoted price through the shared fake.
func (h *harness) bumpSupplierPrice() {
	h.provider.bumpSupplierPrice()
}

func (h *harness) do(t *testing.T, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader

	switch v := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}

		reader = bytes.NewReader(raw)
	}

	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = "203.0.113.10:5555"

	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	req.Header.Set("Authorization", "Bearer "+testKey)

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var out map[string]any

	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}

	return out
}

func errorCodeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	body := decodeBody(t, rec)

	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error object: %s", rec.Body.String())
	}

	code, _ := errObj["code"].(string)

	return code
}

func validCreateBody() map[string]any {
	return map[string]any{
		"origin":      "LED",
		"destination": "JFK",
		"departure":   "2026-04-01",
		"cabin":       "economy",
		"currency":    "EUR",
		"passengers":  []map[string]any{{"full_name": "IVAN IVANOV"}, {"full_name": "PETR PETROV"}},
	}
}

func TestCreateBookingRequiresAuthentication(t *testing.T) {
	h := newHarness(t, 1000)

	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without credentials, got %d", rec.Code)
	}
}

func TestCreateBookingRequiresIdempotencyKey(t *testing.T) {
	h := newHarness(t, 1000)

	rec := h.do(t, http.MethodPost, "/v1/bookings", validCreateBody(), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 without Idempotency-Key, got %d", rec.Code)
	}

	if code := errorCodeOf(t, rec); code != "invalid_request" {
		t.Fatalf("want invalid_request, got %q", code)
	}
}

func TestCreateBookingRejectsUnknownFields(t *testing.T) {
	h := newHarness(t, 1000)

	body := validCreateBody()
	body["skip_payment"] = true

	rec := h.do(t, http.MethodPost, "/v1/bookings", body, map[string]string{
		"Idempotency-Key": "idem-1",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unknown field, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateBookingRejectsUnsupportedCabin(t *testing.T) {
	h := newHarness(t, 1000)

	body := validCreateBody()
	body["cabin"] = "first_class_with_champagne"

	rec := h.do(t, http.MethodPost, "/v1/bookings", body, map[string]string{
		"Idempotency-Key": "idem-1",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unknown cabin, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateBookingIsIdempotent(t *testing.T) {
	h := newHarness(t, 1000)

	first := h.do(t, http.MethodPost, "/v1/bookings", validCreateBody(), map[string]string{
		"Idempotency-Key": "same-key",
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", first.Code, first.Body.String())
	}

	second := h.do(t, http.MethodPost, "/v1/bookings", validCreateBody(), map[string]string{
		"Idempotency-Key": "same-key",
	})

	id := decodeBody(t, first)["id"]
	if got := decodeBody(t, second)["id"]; got != id {
		t.Fatalf("retry created a second booking: %v != %v", got, id)
	}
}

// A replayed idempotency key carrying a different body must be refused, not
// silently treated as the original request.
func TestCreateBookingRejectsIdempotencyKeyReuseWithDifferentBody(t *testing.T) {
	h := newHarness(t, 1000)

	first := h.do(t, http.MethodPost, "/v1/bookings", validCreateBody(), map[string]string{
		"Idempotency-Key": "reused",
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", first.Code, first.Body.String())
	}

	other := validCreateBody()
	other["destination"] = "CDG"

	second := h.do(t, http.MethodPost, "/v1/bookings", other, map[string]string{
		"Idempotency-Key": "reused",
	})
	if second.Code != http.StatusConflict {
		t.Fatalf("want 409 for a reused key with a different body, got %d: %s", second.Code, second.Body.String())
	}
}

// The response must never carry the passenger names, because a booking status
// endpoint has no need to echo personal data.
func TestBookingResponseOmitsPassengerNames(t *testing.T) {
	h := newHarness(t, 1000)

	rec := h.do(t, http.MethodPost, "/v1/bookings", validCreateBody(), map[string]string{
		"Idempotency-Key": "idem-pii",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), "IVAN IVANOV") {
		t.Fatalf("passenger name leaked into the response: %s", rec.Body.String())
	}
}

func TestConfirmWithoutExplicitFlagIsRefused(t *testing.T) {
	h := newHarness(t, 1000)

	id := createHeldBooking(t, h)

	rec := h.do(t, http.MethodPost, "/v1/bookings/"+id+"/confirm", map[string]any{
		"confirmed": false,
	}, map[string]string{"Idempotency-Key": "cf-1"})

	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("want 428 without explicit consent, got %d: %s", rec.Code, rec.Body.String())
	}

	if code := errorCodeOf(t, rec); code != "user_confirmation_required" {
		t.Fatalf("want user_confirmation_required, got %q", code)
	}
}

func TestConfirmWithoutIdempotencyKeyIsRefused(t *testing.T) {
	h := newHarness(t, 1000)

	id := createHeldBooking(t, h)

	rec := h.do(t, http.MethodPost, "/v1/bookings/"+id+"/confirm", map[string]any{
		"confirmed": true,
	}, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 without Idempotency-Key, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestConfirmSucceedsWithExplicitConsent(t *testing.T) {
	h := newHarness(t, 1000)

	id := createHeldBooking(t, h)

	rec := h.do(t, http.MethodPost, "/v1/bookings/"+id+"/confirm", map[string]any{
		"confirmed": true,
	}, map[string]string{"Idempotency-Key": "cf-ok"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	body := decodeBody(t, rec)
	if body["state"] != string(booking.StateConfirmed) {
		t.Fatalf("want confirmed, got %v", body["state"])
	}

	// A confirmed booking is not preliminary and needs no further consent.
	if body["preliminary"] != false {
		t.Fatalf("a confirmed booking must not be preliminary: %v", body["preliminary"])
	}

	if _, ok := body["confirmation"]; !ok {
		t.Fatal("a confirmed booking must carry a confirmation object")
	}

	// Confirmed with the supplier is not paid. The response has to say so
	// explicitly, otherwise a client reads a successful confirmation as money
	// having moved. See docs/adr/0003-payment-gap.md.
	assertPaymentNotSupported(t, body)
}

// assertPaymentNotSupported checks the payment block that every booking response
// must carry. The field is asserted to exist rather than merely to be correct:
// a missing field is precisely the ambiguity ADR 0003 rules out, so its absence
// has to fail a test.
func assertPaymentNotSupported(t *testing.T, body map[string]any) {
	t.Helper()

	const wantStatus = "not_supported"

	raw, ok := body["payment"]
	if !ok {
		t.Fatal("every booking response must carry an explicit payment block")
	}

	payment, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("payment must be an object, got %T: %v", raw, raw)
	}
	if payment["status"] != wantStatus {
		t.Fatalf("want payment status %q, got %v", wantStatus, payment["status"])
	}
	if payment["supported"] != false {
		t.Fatalf("no payment provider exists, so supported must be false, got %v", payment["supported"])
	}
}

// TestPaymentBlockIsPresentInEveryState guards the promise that the field is
// never omitted. A client that has to branch on the absence of a key will
// eventually treat "missing" as "not applicable" and hide the gap.
func TestPaymentBlockIsPresentInEveryState(t *testing.T) {
	h := newHarness(t, 1000)

	// A freshly created booking, before any supplier interaction.
	rec := h.do(t, http.MethodPost, "/v1/bookings", validCreateBody(), map[string]string{
		"Idempotency-Key": "pay-intent",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	assertPaymentNotSupported(t, decodeBody(t, rec))

	// A confirmed booking, the state a client is most likely to read as paid.
	id := createHeldBooking(t, h)
	rec = h.do(t, http.MethodPost, "/v1/bookings/"+id+"/confirm", map[string]any{
		"confirmed": true,
	}, map[string]string{"Idempotency-Key": "pay-confirm"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	assertPaymentNotSupported(t, decodeBody(t, rec))
}

// The price moved between the hold and the confirmation. The API must refuse
// and require a new explicit user decision.
func TestConfirmRefusesWhenPriceChanged(t *testing.T) {
	h := newHarness(t, 1000)

	id := createHeldBooking(t, h)
	h.clock.advance(2 * time.Minute)
	h.bumpSupplierPrice()

	rec := h.do(t, http.MethodPost, "/v1/bookings/"+id+"/confirm", map[string]any{
		"confirmed": true,
	}, map[string]string{"Idempotency-Key": "cf-price"})

	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 when the price changed, got %d: %s", rec.Code, rec.Body.String())
	}

	if code := errorCodeOf(t, rec); code != "price_changed" {
		t.Fatalf("want price_changed, got %q", code)
	}
}

// Confirming a lapsed hold must be refused rather than attempting a financial
// operation on a reservation that no longer exists.
func TestConfirmRefusesWhenHoldExpired(t *testing.T) {
	h := newHarness(t, 1000)

	id := createHeldBooking(t, h)
	h.clock.advance(48 * time.Hour)

	rec := h.do(t, http.MethodPost, "/v1/bookings/"+id+"/confirm", map[string]any{
		"confirmed": true,
	}, map[string]string{"Idempotency-Key": "cf-expired"})

	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 for an expired hold, got %d: %s", rec.Code, rec.Body.String())
	}

	if code := errorCodeOf(t, rec); code != "hold_expired" {
		t.Fatalf("want hold_expired, got %q", code)
	}
}

func TestCancelConfirmedBooking(t *testing.T) {
	h := newHarness(t, 1000)

	id := createHeldBooking(t, h)

	if rec := h.do(t, http.MethodPost, "/v1/bookings/"+id+"/confirm", map[string]any{
		"confirmed": true,
	}, map[string]string{"Idempotency-Key": "cf-1"}); rec.Code != http.StatusOK {
		t.Fatalf("confirm failed: %d %s", rec.Code, rec.Body.String())
	}

	rec := h.do(t, http.MethodPost, "/v1/bookings/"+id+"/cancel", map[string]any{
		"confirmed": true,
	}, map[string]string{"Idempotency-Key": "cn-1"})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if state := decodeBody(t, rec)["state"]; state != string(booking.StateCancelled) {
		t.Fatalf("want cancelled, got %v", state)
	}
}

func TestGetUnknownBookingIs404(t *testing.T) {
	h := newHarness(t, 1000)

	rec := h.do(t, http.MethodGet, "/v1/bookings/bk_does_not_exist", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	h := newHarness(t, 1000)

	huge := strings.Repeat("A", 8192)
	body := fmt.Sprintf(`{"origin":%q,"destination":"JFK","departure":"2026-04-01","cabin":"economy"}`, huge)

	rec := h.do(t, http.MethodPost, "/v1/bookings", body, map[string]string{
		"Idempotency-Key": "idem-big",
	})

	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Fatalf("want 413 or 400 for an oversized body, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRateLimitIsEnforced(t *testing.T) {
	h := newHarness(t, 2)

	for i := range 2 {
		rec := h.do(t, http.MethodGet, "/healthz", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: want 200, got %d", i, rec.Code)
		}
	}

	rec := h.do(t, http.MethodGet, "/healthz", nil, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429 after the budget is spent, got %d", rec.Code)
	}

	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("a 429 must carry a Retry-After header")
	}

	// The window must roll over rather than blocking forever.
	h.clock.advance(2 * time.Minute)

	rec = h.do(t, http.MethodGet, "/healthz", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 after the window rolled, got %d", rec.Code)
	}
}

func TestHealthCheckIsNotPIIBearing(t *testing.T) {
	h := newHarness(t, 1000)

	rec := h.do(t, http.MethodGet, "/healthz", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}

	if strings.Contains(rec.Body.String(), "user-1") {
		t.Fatalf("health response leaked an identity: %s", rec.Body.String())
	}
}

func TestErrorBodyNeverLeaksInternals(t *testing.T) {
	h := newHarness(t, 1000)

	id := createHeldBooking(t, h)
	h.clock.advance(2 * time.Minute)
	h.bumpSupplierPrice()

	rec := h.do(t, http.MethodPost, "/v1/bookings/"+id+"/confirm", map[string]any{
		"confirmed": true,
	}, map[string]string{"Idempotency-Key": "cf-leak"})

	body := rec.Body.String()
	for _, forbidden := range []string{"goroutine", "panic", ".go:", "supplier_url", "sql"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("error body leaked %q: %s", forbidden, body)
		}
	}
}

// The same key repeated on confirm must not charge or confirm twice.
func TestConfirmRetryIsIdempotent(t *testing.T) {
	h := newHarness(t, 1000)

	id := createHeldBooking(t, h)

	first := h.do(t, http.MethodPost, "/v1/bookings/"+id+"/confirm", map[string]any{
		"confirmed": true,
	}, map[string]string{"Idempotency-Key": "cf-retry"})

	if first.Code != http.StatusOK {
		t.Fatalf("first confirm: %d %s", first.Code, first.Body.String())
	}

	second := h.do(t, http.MethodPost, "/v1/bookings/"+id+"/confirm", map[string]any{
		"confirmed": true,
	}, map[string]string{"Idempotency-Key": "cf-retry"})

	if second.Code != http.StatusOK {
		t.Fatalf("a retried confirm must succeed, got %d: %s", second.Code, second.Body.String())
	}

	history, _ := decodeBody(t, second)["history"].([]any)

	// Exactly one confirm_succeeded entry proves the supplier was called once.
	successes := 0

	for _, raw := range history {
		entry, _ := raw.(map[string]any)
		if entry["trigger"] == "confirm_succeeded" {
			successes++
		}
	}

	if successes != 1 {
		t.Fatalf("want exactly one confirm_succeeded, got %d: %s", successes, second.Body.String())
	}
}

// createHeldBooking drives a booking to the held state and returns its id.
func createHeldBooking(t *testing.T, h *harness) string {
	t.Helper()

	rec := h.do(t, http.MethodPost, "/v1/bookings", validCreateBody(), map[string]string{
		"Idempotency-Key": "create-held",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}

	id, _ := decodeBody(t, rec)["id"].(string)

	if rec := h.do(t, http.MethodPost, "/v1/bookings/"+id+"/search", nil, map[string]string{
		"Idempotency-Key": "search-1",
	}); rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}

	if rec := h.do(t, http.MethodPost, "/v1/bookings/"+id+"/hold", nil, map[string]string{
		"Idempotency-Key": "hold-1",
	}); rec.Code != http.StatusOK {
		t.Fatalf("hold: %d %s", rec.Code, rec.Body.String())
	}

	return id
}

// A liveness probe has no credential, so it must not be gated by
// authentication. Returning 401 would make an orchestrator restart a healthy
// service.
func TestHealthIsReachableWithoutCredentials(t *testing.T) {
	h := newHarness(t, 1000)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for an unauthenticated health probe, got %d", rec.Code)
	}
}

// The exemption must be exact: another route may not inherit it.
func TestOnlyHealthIsExemptFromAuthentication(t *testing.T) {
	h := newHarness(t, 1000)

	req := httptest.NewRequest(http.MethodGet, "/v1/bookings/bk_1", nil)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 on a protected route, got %d", rec.Code)
	}
}

// A request-validation failure detected by the service, not the transport, must
// still be a client error. It previously escaped as a 500.
func TestDepartureInThePastIsABadRequest(t *testing.T) {
	h := newHarness(t, 1000)

	body := validCreateBody()
	body["departure"] = "2020-01-01"

	rec := h.do(t, http.MethodPost, "/v1/bookings", body, map[string]string{
		"Idempotency-Key": "idem-past",
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a departure in the past, got %d: %s", rec.Code, rec.Body.String())
	}

	if code := errorCodeOf(t, rec); code != "invalid_request" {
		t.Fatalf("want invalid_request, got %q", code)
	}
}

func TestSameOriginAndDestinationIsABadRequest(t *testing.T) {
	h := newHarness(t, 1000)

	body := validCreateBody()
	body["destination"] = "LED"

	rec := h.do(t, http.MethodPost, "/v1/bookings", body, map[string]string{
		"Idempotency-Key": "idem-same",
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 when origin equals destination, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUnsupportedCurrencyIsABadRequest(t *testing.T) {
	h := newHarness(t, 1000)

	body := validCreateBody()
	body["currency"] = "BTC"

	rec := h.do(t, http.MethodPost, "/v1/bookings", body, map[string]string{
		"Idempotency-Key": "idem-btc",
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unsupported currency, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReturnBeforeDepartureIsABadRequest(t *testing.T) {
	h := newHarness(t, 1000)

	body := validCreateBody()
	body["return"] = "2026-03-01"

	rec := h.do(t, http.MethodPost, "/v1/bookings", body, map[string]string{
		"Idempotency-Key": "idem-return",
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a return before departure, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A second JSON document in one body must be refused rather than the trailing
// content being ignored.
func TestTrailingJSONDocumentIsRejected(t *testing.T) {
	h := newHarness(t, 1000)

	body := `{"origin":"LED","destination":"JFK","departure":"2026-04-01","cabin":"economy"}{"extra":1}`

	rec := h.do(t, http.MethodPost, "/v1/bookings", body, map[string]string{
		"Idempotency-Key": "idem-trailing",
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a body with two documents, got %d: %s", rec.Code, rec.Body.String())
	}
}

// The confirm endpoint must refuse a partially specified accepted price.
func TestConfirmRejectsHalfSpecifiedPrice(t *testing.T) {
	h := newHarness(t, 1000)

	id := createHeldBooking(t, h)

	rec := h.do(t, http.MethodPost, "/v1/bookings/"+id+"/confirm", map[string]any{
		"confirmed":         true,
		"accepted_amount":   12345,
		"accepted_currency": nil,
	}, map[string]string{"Idempotency-Key": "cf-half"})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a half-specified price, got %d: %s", rec.Code, rec.Body.String())
	}
}
