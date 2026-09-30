package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/rockbotum/ai-journey-service/internal/booking"
)

// maxBodyBytes bounds a provider response. A supplier that streams an
// unbounded body must not be able to exhaust the service's memory.
const maxBodyBytes = 2 << 20 // 2 MiB

// HTTPClient talks to an OpenAPI-style supplier over HTTP.
type HTTPClient struct {
	baseURL string
	http    *http.Client
	opts    Options
}

// NewHTTPClient builds a client. It fails on an unusable configuration so a
// misconfigured supplier is a startup error, not a runtime surprise.
func NewHTTPClient(opts Options) (*HTTPClient, error) {
	opts = opts.WithDefaults()

	if err := opts.Validate(); err != nil {
		return nil, err
	}

	parsed, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("provider: invalid base URL: %w", err)
	}

	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("provider: base URL must use http or https, got %q", parsed.Scheme)
	}

	return &HTTPClient{
		baseURL: parsed.String(),
		// No timeout on the client itself: the per-attempt deadline is
		// derived from the caller's context so a caller's own deadline wins.
		http: &http.Client{},
		opts: opts,
	}, nil
}

// Search queries candidate offers.
func (c *HTTPClient) Search(ctx context.Context, q SearchQuery) ([]booking.Offer, error) {
	payload := map[string]any{
		"origin":      q.Origin.Code,
		"destination": q.Destination.Code,
		"departure":   q.Departure.UTC().Format("2006-01-02"),
		"cabin":       string(q.Cabin),
		"travellers":  q.Travellers,
		"currency":    string(q.Currency),
	}

	if q.Return != nil {
		payload["return"] = q.Return.UTC().Format("2006-01-02")
	}

	var out searchResponse

	if err := c.call(ctx, http.MethodPost, "/v1/search", payload, "", true, &out); err != nil {
		return nil, err
	}

	return c.convertOffers(out)
}

// convertOffers validates the supplier payload before it becomes domain data.
// A missing or non-positive price is treated as malformed rather than coerced
// to zero, because a zero price would flow into a financial decision.
func (c *HTTPClient) convertOffers(resp searchResponse) ([]booking.Offer, error) {
	offers := make([]booking.Offer, 0, len(resp.Offers))

	for i, raw := range resp.Offers {
		if raw.ID == "" {
			return nil, malformedError(fmt.Sprintf("offer %d has no id", i))
		}

		if raw.TotalMinor <= 0 {
			return nil, malformedError(fmt.Sprintf("offer %d has a non-positive total", i))
		}

		currency, err := booking.ParseCurrency(raw.Currency)
		if err != nil {
			return nil, malformedError(fmt.Sprintf("offer %d has an unknown currency", i))
		}

		offers = append(offers, booking.Offer{
			SupplierID:          c.opts.SupplierID,
			SupplierOffer:       raw.ID,
			Total:               booking.Money{Amount: raw.TotalMinor, Currency: currency},
			AvailableAt:         time.Now().UTC(),
			CancellationSummary: raw.Cancellation,
		})
	}

	return offers, nil
}

// Hold reserves an offer temporarily.
func (c *HTTPClient) Hold(ctx context.Context, offer booking.Offer, idempotencyKey string) (booking.Hold, error) {
	if idempotencyKey == "" {
		return booking.Hold{}, errors.New("provider: idempotency key is required to hold")
	}

	payload := map[string]any{
		"offer_id":    offer.SupplierOffer,
		"expected_in": offer.Total.Amount,
		"currency":    string(offer.Total.Currency),
	}

	var out holdResponse

	if err := c.call(ctx, http.MethodPost, "/v1/holds", payload, idempotencyKey, true, &out); err != nil {
		return booking.Hold{}, err
	}

	if out.HoldID == "" {
		return booking.Hold{}, malformedError("hold response has no id")
	}

	if out.ExpiresAt.IsZero() {
		return booking.Hold{}, malformedError("hold response has no expiry")
	}

	if out.ExpiresAt.Before(time.Now()) {
		return booking.Hold{}, malformedError("hold response is already expired")
	}

	if out.TotalMinor <= 0 {
		return booking.Hold{}, malformedError("hold response has a non-positive total")
	}

	currency, err := booking.ParseCurrency(out.Currency)
	if err != nil {
		return booking.Hold{}, malformedError("hold response has an unknown currency")
	}

	return booking.Hold{
		SupplierID:   c.opts.SupplierID,
		SupplierRef:  out.HoldID,
		SupplierName: offer.SupplierOffer,
		Total:        booking.Money{Amount: out.TotalMinor, Currency: currency},
		ExpiresAt:    out.ExpiresAt.UTC(),
	}, nil
}

// Confirm converts a hold into a confirmed booking. It is never retried
// automatically: a blind replay risks a second charge.
func (c *HTTPClient) Confirm(ctx context.Context, hold booking.Hold, idempotencyKey string) (booking.Confirmation, error) {
	if idempotencyKey == "" {
		return booking.Confirmation{}, errors.New("provider: idempotency key is required to confirm")
	}

	payload := map[string]any{"hold_id": hold.SupplierRef}

	var out confirmResponse

	if err := c.call(ctx, http.MethodPost, "/v1/bookings", payload, idempotencyKey, true, &out); err != nil {
		return booking.Confirmation{}, err
	}

	if out.BookingID == "" {
		return booking.Confirmation{}, malformedError("confirmation response has no id")
	}

	currency, err := booking.ParseCurrency(out.Currency)
	if err != nil {
		return booking.Confirmation{}, malformedError("confirmation response has an unknown currency")
	}

	if out.TotalMinor < 0 {
		return booking.Confirmation{}, malformedError("confirmation response has a negative total")
	}

	confirmedAt := out.ConfirmedAt
	if confirmedAt.IsZero() {
		confirmedAt = time.Now().UTC()
	}

	return booking.Confirmation{
		SupplierID:     c.opts.SupplierID,
		SupplierRef:    out.BookingID,
		Total:          booking.Money{Amount: out.TotalMinor, Currency: currency},
		ConfirmedAt:    confirmedAt.UTC(),
		TicketDocument: out.Document,
	}, nil
}

// Cancel releases a hold or cancels a booking.
func (c *HTTPClient) Cancel(ctx context.Context, ref SupplierRef, idempotencyKey string) error {
	if err := ref.Validate(); err != nil {
		return err
	}

	if idempotencyKey == "" {
		return errors.New("provider: idempotency key is required to cancel")
	}

	path := "/v1/bookings/" + url.PathEscape(ref.SupplierRef) + "/cancellation"
	if err := c.call(ctx, http.MethodPost, path, map[string]any{}, idempotencyKey, true, nil); err != nil {
		return err
	}

	return nil
}

// Reprice re-reads the price of a held offer.
func (c *HTTPClient) Reprice(ctx context.Context, ref SupplierRef) (booking.Money, error) {
	if err := ref.Validate(); err != nil {
		return booking.Money{}, err
	}

	var out repriceResponse

	path := "/v1/holds/" + url.PathEscape(ref.SupplierRef)
	if err := c.call(ctx, http.MethodGet, path, nil, "", true, &out); err != nil {
		return booking.Money{}, err
	}

	currency, err := booking.ParseCurrency(out.Currency)
	if err != nil {
		return booking.Money{}, malformedError("reprice response has an unknown currency")
	}

	return booking.Money{Amount: out.TotalMinor, Currency: currency}, nil
}

// call performs one logical request with bounded retries.
//
// replaySafe is decided by the caller from the semantics of the operation, not
// inferred from the HTTP method, because the same method can be safe or unsafe
// depending on what it does: a search is a read even over POST, and a booking
// write is unsafe to repeat unless the supplier deduplicates on an
// Idempotency-Key.
func (c *HTTPClient) call(ctx context.Context, method, path string, payload any, idempotencyKey string, replaySafe bool, out any) error {
	var body []byte

	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("provider: encoding request: %w", err)
		}

		body = encoded
	}

	attempts := c.opts.MaxRetries + 1
	if !replaySafe {
		attempts = 1
	}

	var lastErr error

	for attempt := range attempts {
		if err := sleepCtx(ctx, backoffFor(attempt-1, c.opts.InitialBackoff, c.opts.MaxBackoff)); err != nil {
			return fmt.Errorf("%w: %v", ErrDeadlineExceeded, err)
		}

		err := c.attempt(ctx, method, path, body, idempotencyKey, out)
		if err == nil {
			return nil
		}

		lastErr = err

		if !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrRateLimited) {
			return err
		}
	}

	return lastErr
}

// attempt performs exactly one HTTP round trip.
func (c *HTTPClient) attempt(ctx context.Context, method, path string, body []byte, idempotencyKey string, out any) error {
	attemptCtx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(attemptCtx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("provider: building request: %w", err)
	}

	req.Header.Set("Accept", "application/json")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	if c.opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.opts.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if attemptCtx.Err() != nil {
			return fmt.Errorf("%w: %v", ErrUnavailable, attemptCtx.Err())
		}

		return fmt.Errorf("%w: transport error", ErrUnavailable)
	}

	defer func() {
		// Drain and close so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return statusError(resp.StatusCode)
	}

	if out == nil {
		return nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("%w: reading response", ErrUnavailable)
	}

	if len(raw) == 0 {
		return malformedError("empty response body")
	}

	if err := json.Unmarshal(raw, out); err != nil {
		// The payload itself is not echoed: it may contain personal data.
		return malformedError("response is not valid JSON")
	}

	return nil
}

// statusError maps an HTTP status to a domain error, without including the
// provider's body in the message.
func statusError(code int) error {
	switch {
	case code == http.StatusTooManyRequests:
		return fmt.Errorf("%w: status %d", ErrRateLimited, code)
	case code == http.StatusConflict:
		return fmt.Errorf("%w: status %d", ErrPriceUnavailable, code)
	case code == http.StatusNotFound:
		return permanentError("not found")
	case code >= 500:
		return fmt.Errorf("%w: status %d", ErrUnavailable, code)
	case code >= 400:
		return permanentError("status " + strconv.Itoa(code))
	default:
		return malformedError("unexpected status " + strconv.Itoa(code))
	}
}

// Wire shapes. They mirror the supplier contract and are converted explicitly
// into domain types, so a supplier-side rename cannot silently change the
// domain.
type searchResponse struct {
	Offers []struct {
		ID           string `json:"id"`
		TotalMinor   int64  `json:"total_minor"`
		Currency     string `json:"currency"`
		Cancellation string `json:"cancellation"`
	} `json:"offers"`
}

type holdResponse struct {
	HoldID     string    `json:"hold_id"`
	TotalMinor int64     `json:"total_minor"`
	Currency   string    `json:"currency"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type confirmResponse struct {
	BookingID   string    `json:"booking_id"`
	TotalMinor  int64     `json:"total_minor"`
	Currency    string    `json:"currency"`
	ConfirmedAt time.Time `json:"confirmed_at"`
	Document    string    `json:"document"`
}

type repriceResponse struct {
	TotalMinor int64  `json:"total_minor"`
	Currency   string `json:"currency"`
}

// Interface compliance is checked at compile time.
var _ Client = (*HTTPClient)(nil)
