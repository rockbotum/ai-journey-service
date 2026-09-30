package provider_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rockbotum/ai-journey-service/internal/booking"
	"github.com/rockbotum/ai-journey-service/internal/provider"
)

func newClient(t *testing.T, baseURL string, mutate func(*provider.Options)) *provider.HTTPClient {
	t.Helper()

	opts := provider.Options{
		BaseURL:        baseURL,
		Timeout:        500 * time.Millisecond,
		MaxRetries:     2,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
		SupplierID:     "sup_a",
	}

	if mutate != nil {
		mutate(&opts)
	}

	client, err := provider.NewHTTPClient(opts)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}

	return client
}

func testQuery() provider.SearchQuery {
	return provider.SearchQuery{
		Origin:      booking.Location{Code: "LED"},
		Destination: booking.Location{Code: "JFK"},
		Departure:   time.Now().Add(48 * time.Hour).UTC().Truncate(24 * time.Hour),
		Cabin:       booking.CabinEconomy,
		Travellers:  1,
		Currency:    booking.CurrencyEUR,
	}
}

func TestNewHTTPClientRejectsUnusableOptions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*provider.Options)
	}{
		{"no base url", func(o *provider.Options) { o.BaseURL = "" }},
		{"no supplier id", func(o *provider.Options) { o.SupplierID = "" }},
		{"negative retries", func(o *provider.Options) { o.MaxRetries = -1 }},
		{"unsupported scheme", func(o *provider.Options) { o.BaseURL = "ftp://supplier.example.com" }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := provider.Options{BaseURL: "https://supplier.example.com", SupplierID: "sup_a", Timeout: time.Second}
			tc.mutate(&opts)

			if _, err := provider.NewHTTPClient(opts); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestZeroTimeoutFallsBackToASafeDefault(t *testing.T) {
	// A zero timeout must never mean "no timeout": WithDefaults fills it in so
	// the client can never be constructed without a deadline.
	opts := provider.Options{BaseURL: "https://supplier.example.com", SupplierID: "sup_a"}

	if _, err := provider.NewHTTPClient(opts); err != nil {
		t.Fatalf("NewHTTPClient with defaults: %v", err)
	}
}

func TestSearchReturnsValidatedOffers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"offers":[{"id":"off_1","total_minor":45000,"currency":"EUR","cancellation":"free until 24h"}]}`))
	}))
	defer srv.Close()

	client := newClient(t, srv.URL, nil)

	offers, err := client.Search(t.Context(), testQuery())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(offers) != 1 {
		t.Fatalf("offers = %d, want 1", len(offers))
	}

	if offers[0].SupplierOffer != "off_1" || offers[0].Total.Amount != 45_000 {
		t.Fatalf("unexpected offer: %+v", offers[0])
	}

	if offers[0].SupplierID != "sup_a" {
		t.Errorf("supplier id not stamped on the offer: %+v", offers[0])
	}
}

func TestSearchRejectsMalformedSupplierPayloads(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"no id", `{"offers":[{"total_minor":45000,"currency":"EUR"}]}`},
		{"zero price", `{"offers":[{"id":"off_1","total_minor":0,"currency":"EUR"}]}`},
		{"negative price", `{"offers":[{"id":"off_1","total_minor":-1,"currency":"EUR"}]}`},
		{"unknown currency", `{"offers":[{"id":"off_1","total_minor":45000,"currency":"XYZ"}]}`},
		{"not json", `not json at all`},
		{"empty body", ``},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			client := newClient(t, srv.URL, nil)

			if _, err := client.Search(t.Context(), testQuery()); !errors.Is(err, provider.ErrMalformedResponse) {
				t.Fatalf("err = %v, want ErrMalformedResponse", err)
			}
		})
	}
}

func TestPermanentStatusIsNotRetried(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	client := newClient(t, srv.URL, nil)

	_, err := client.Search(t.Context(), testQuery())
	if !errors.Is(err, provider.ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("permanent failure was retried %d times, want 1 call", got)
	}
}

func TestTransientStatusIsRetriedAndSucceeds(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		_, _ = w.Write([]byte(`{"offers":[{"id":"off_1","total_minor":45000,"currency":"EUR"}]}`))
	}))
	defer srv.Close()

	client := newClient(t, srv.URL, nil)

	offers, err := client.Search(t.Context(), testQuery())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(offers) != 1 {
		t.Fatalf("offers = %d, want 1", len(offers))
	}

	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3 (2 retries)", got)
	}
}

func TestRetriesAreBounded(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	client := newClient(t, srv.URL, nil)

	_, err := client.Search(t.Context(), testQuery())
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}

	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3 (initial + 2 retries), retries must be bounded", got)
	}
}

func TestTimeoutIsReportedAsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	client := newClient(t, srv.URL, func(o *provider.Options) {
		o.Timeout = 50 * time.Millisecond
		o.MaxRetries = 0
	})

	start := time.Now()
	_, err := client.Search(t.Context(), testQuery())
	elapsed := time.Since(start)

	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}

	if elapsed > time.Second {
		t.Errorf("call took %v, the per-attempt timeout was not applied", elapsed)
	}
}

func TestCallerCancellationStopsTheCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	client := newClient(t, srv.URL, func(o *provider.Options) {
		o.Timeout = 5 * time.Second
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := client.Search(ctx, testQuery()); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}

// TestFinancialCallsCarryTheIdempotencyKey verifies the supplier actually
// receives the key that makes a retry safe.
func TestFinancialCallsCarryTheIdempotencyKey(t *testing.T) {
	var seen atomic.Value

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Idempotency-Key"))
		_, _ = w.Write([]byte(`{"hold_id":"hold_1","total_minor":45000,"currency":"EUR","expires_at":"2999-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	client := newClient(t, srv.URL, nil)

	hold, err := client.Hold(t.Context(), booking.Offer{
		SupplierID:    "sup_a",
		SupplierOffer: "off_1",
		Total:         booking.Money{Amount: 45_000, Currency: booking.CurrencyEUR},
	}, "idem_hold_1")
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	if got, _ := seen.Load().(string); got != "idem_hold_1" {
		t.Errorf("Idempotency-Key = %q, want idem_hold_1", got)
	}

	if hold.SupplierRef != "hold_1" {
		t.Errorf("hold ref = %q, want hold_1", hold.SupplierRef)
	}
}

func TestFinancialCallsRequireAnIdempotencyKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("supplier must not be called without an idempotency key")
	}))
	defer srv.Close()

	client := newClient(t, srv.URL, nil)

	offer := booking.Offer{SupplierID: "sup_a", SupplierOffer: "off_1", Total: booking.Money{Amount: 1, Currency: booking.CurrencyEUR}}

	if _, err := client.Hold(t.Context(), offer, ""); err == nil {
		t.Error("Hold without an idempotency key must fail")
	}

	hold := booking.Hold{SupplierID: "sup_a", SupplierRef: "hold_1", Total: offer.Total, ExpiresAt: time.Now().Add(time.Hour)}

	if _, err := client.Confirm(t.Context(), hold, ""); err == nil {
		t.Error("Confirm without an idempotency key must fail")
	}

	if err := client.Cancel(t.Context(), provider.SupplierRef{SupplierID: "sup_a", SupplierRef: "bk_1"}, ""); err == nil {
		t.Error("Cancel without an idempotency key must fail")
	}
}

func TestHoldRejectsAnAlreadyExpiredResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hold_id":"hold_1","total_minor":45000,"currency":"EUR","expires_at":"2000-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	client := newClient(t, srv.URL, nil)

	_, err := client.Hold(t.Context(), booking.Offer{
		SupplierID:    "sup_a",
		SupplierOffer: "off_1",
		Total:         booking.Money{Amount: 45_000, Currency: booking.CurrencyEUR},
	}, "idem_1")
	if !errors.Is(err, provider.ErrMalformedResponse) {
		t.Fatalf("err = %v, want ErrMalformedResponse for an expired hold", err)
	}
}

func TestStatusMapping(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{http.StatusTooManyRequests, provider.ErrRateLimited},
		{http.StatusConflict, provider.ErrPriceUnavailable},
		{http.StatusNotFound, provider.ErrRejected},
		{http.StatusUnauthorized, provider.ErrRejected},
		{http.StatusInternalServerError, provider.ErrUnavailable},
		{http.StatusBadGateway, provider.ErrUnavailable},
	}

	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			client := newClient(t, srv.URL, func(o *provider.Options) { o.MaxRetries = 0 })

			_, err := client.Search(t.Context(), testQuery())
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: err = %v, want %v", tc.status, err, tc.want)
			}
		})
	}
}

// TestErrorsDoNotLeakTheProviderBody guards the rule that a supplier response
// is never copied into an error that may reach a log or a client.
func TestErrorsDoNotLeakTheProviderBody(t *testing.T) {
	secretish := "passport=1234567890 email=user@example.com"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"` + secretish + `"}`))
	}))
	defer srv.Close()

	client := newClient(t, srv.URL, nil)

	_, err := client.Search(t.Context(), testQuery())
	if err == nil {
		t.Fatal("expected an error")
	}

	if got := err.Error(); strings.Contains(got, secretish) {
		t.Fatalf("error leaked the provider body: %s", got)
	}
}
