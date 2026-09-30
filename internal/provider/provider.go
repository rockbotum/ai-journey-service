// Package provider integrates with an external travel supplier.
//
// It is deliberately isolated from business logic: the service layer depends on
// the interfaces declared here, never on an HTTP client. Every call obeys the
// rules from AGENTS.md that apply to external APIs: a timeout, context
// cancellation, a bounded number of retries with backoff, a distinction between
// temporary and permanent failures, and validation of both the request and the
// response instead of trusting HTTPS alone.
package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rockbotum/ai-journey-service/internal/booking"
)

// Sentinel errors. Callers branch on these instead of inspecting status codes
// or provider message strings, so a provider can change its wording without
// silently changing business behaviour.
var (
	// ErrUnavailable means the supplier could not be reached or failed
	// transiently. Retrying later is reasonable.
	ErrUnavailable = errors.New("provider: temporarily unavailable")
	// ErrRejected means the supplier understood the request and refused it.
	// Retrying the identical request is pointless.
	ErrRejected = errors.New("provider: request rejected")
	// ErrMalformedResponse means the response did not match the expected
	// contract. It is treated as permanent: a corrupted payload must not be
	// retried into a booking.
	ErrMalformedResponse = errors.New("provider: malformed response")
	// ErrPriceUnavailable means the quoted option is no longer bookable.
	ErrPriceUnavailable = errors.New("provider: option no longer available")
	// ErrRateLimited means the supplier asked the client to slow down.
	ErrRateLimited = errors.New("provider: rate limited")
	// ErrDeadlineExceeded means the caller's context ended first.
	ErrDeadlineExceeded = errors.New("provider: deadline exceeded")
)

// SearchQuery is the structured search request. It is built from validated
// domain data, never concatenated from free text.
type SearchQuery struct {
	Origin      booking.Location
	Destination booking.Location
	Departure   time.Time
	Return      *time.Time
	Cabin       booking.Cabin
	Travellers  int
	Currency    booking.Currency
}

// Options tune the transport behaviour of a client.
type Options struct {
	// BaseURL is the supplier API root.
	BaseURL string
	// Timeout bounds a single attempt, including connection setup.
	Timeout time.Duration
	// MaxRetries is the number of retries after the first attempt. Zero means
	// a single attempt.
	MaxRetries int
	// InitialBackoff is the first retry delay; it doubles each attempt.
	InitialBackoff time.Duration
	// MaxBackoff caps the delay so a long retry chain cannot exceed the
	// caller's deadline.
	MaxBackoff time.Duration
	// APIKey authenticates the outbound call. It is never logged.
	APIKey string
	// SupplierID identifies this integration, and is stored with every result
	// so a booking can always be traced back to its source.
	SupplierID string
}

// WithDefaults fills unset options with safe values. A zero Options would
// otherwise mean no timeout at all, which is the one configuration that must
// never be allowed through.
func (o Options) WithDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}

	if o.InitialBackoff <= 0 {
		o.InitialBackoff = 200 * time.Millisecond
	}

	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 2 * time.Second
	}

	if o.MaxBackoff < o.InitialBackoff {
		o.MaxBackoff = o.InitialBackoff
	}

	return o
}

// Validate rejects an unusable configuration at construction time rather than
// on the first user request.
func (o Options) Validate() error {
	if o.BaseURL == "" {
		return errors.New("provider: base URL is required")
	}

	if o.SupplierID == "" {
		return errors.New("provider: supplier id is required")
	}

	if o.MaxRetries < 0 {
		return errors.New("provider: max retries must not be negative")
	}

	if o.Timeout <= 0 {
		return errors.New("provider: timeout must be positive")
	}

	return nil
}

// Client is the contract the service layer depends on. A fake implementation
// is used in tests, so no test ever reaches a real supplier.
type Client interface {
	// Search returns candidate offers. It is read-only and safe to retry.
	Search(ctx context.Context, q SearchQuery) ([]booking.Offer, error)
	// Hold reserves an offer temporarily. It consumes supplier availability,
	// so it carries an idempotency key and must not be retried blindly.
	Hold(ctx context.Context, offer booking.Offer, idempotencyKey string) (booking.Hold, error)
	// Confirm creates the booking. It is financial: the idempotency key is
	// mandatory and the call is never retried automatically.
	Confirm(ctx context.Context, hold booking.Hold, idempotencyKey string) (booking.Confirmation, error)
	// Cancel releases a hold or cancels a confirmed booking. Financial, so the
	// same idempotency rules apply.
	Cancel(ctx context.Context, ref SupplierRef, idempotencyKey string) error
	// Reprice re-reads the current price of a held offer, which is how a price
	// change between the user's decision and the confirmation is detected.
	Reprice(ctx context.Context, ref SupplierRef) (booking.Money, error)
}

// SupplierRef identifies a supplier-side reservation or booking.
type SupplierRef struct {
	SupplierID  string
	SupplierRef string
}

func (r SupplierRef) Validate() error {
	if r.SupplierID == "" || r.SupplierRef == "" {
		return fmt.Errorf("provider: incomplete supplier reference")
	}

	return nil
}

// classify decides whether an error is worth retrying. Only transport failures
// and 5xx responses qualify: a 4xx means the request itself is wrong, and
// replaying it would either waste quota or, worse, create a second booking.
func classify(statusCode int, err error) (permanent bool, retryable bool) {
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return true, false
		}

		// Transport-level failures are worth another attempt.
		return false, true
	}

	switch {
	case statusCode == 429:
		return false, true
	case statusCode >= 500:
		return false, true
	case statusCode >= 400:
		return true, false
	default:
		return true, false
	}
}

// backoffFor returns the delay before the given retry attempt, doubling each
// time and capped by MaxBackoff.
func backoffFor(attempt int, initial, max time.Duration) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}

	delay := initial
	for range attempt - 1 {
		delay *= 2
		if delay >= max {
			return max
		}
	}

	if delay > max {
		return max
	}

	return delay
}

// sleepCtx waits for d or returns early when the context ends, so an
// abandoned request does not keep the process waiting through a backoff.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// permanentError builds a non-retryable error with a stable, non-sensitive
// reason. A raw provider body is never wrapped into it: it may contain
// personal data or an internal identifier.
func permanentError(reason string) error {
	return fmt.Errorf("%w: %s", ErrRejected, reason)
}

func malformedError(reason string) error {
	return fmt.Errorf("%w: %s", ErrMalformedResponse, reason)
}
