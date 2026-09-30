// Package store persists bookings with the guarantees the domain depends on:
// idempotent creation and financial operations, and protection against
// concurrent modification.
//
// AGENTS.md requires idempotency to be enforced at the database level, not
// only in application code, because a retried request can arrive at two
// instances at once. The interface below is the contract a PostgreSQL
// implementation must satisfy; the in-memory implementation in this package
// mirrors those semantics so the service can be tested without a database.
package store

import (
	"context"
	"errors"

	"github.com/rockbotum/ai-journey-service/internal/booking"
)

// Sentinel errors for persistence failures.
var (
	// ErrNotFound means the booking does not exist, or does not belong to
	// the requesting user.
	ErrNotFound = errors.New("store: booking not found")
	// ErrConflict means another writer changed the booking first. The caller
	// reloads and retries rather than overwriting a newer state.
	ErrConflict = errors.New("store: concurrent modification detected")
	// ErrIdempotencyConflict means the same key was reused for a different
	// request body, which would otherwise silently discard one of them.
	ErrIdempotencyConflict = errors.New("store: idempotency key reused with a different request")
)

// Store is the persistence contract.
type Store interface {
	// Create stores a new booking. Repeating the call with the same
	// idempotencyKey returns the booking created by the first call instead of
	// creating a second one. Reusing the key with different parameters is an
	// ErrIdempotencyConflict.
	Create(ctx context.Context, key string, b *booking.Booking) (*booking.Booking, error)

	// Get returns a booking owned by userID. A booking owned by somebody else
	// reports ErrNotFound rather than ErrForbidden, so the API cannot be used
	// to probe which booking identifiers exist.
	Get(ctx context.Context, id, userID string) (*booking.Booking, error)

	// Save writes a booking only if its stored version still matches
	// expectedVersion, which is how a stale write is rejected instead of
	// resurrecting a lapsed hold or undoing a cancellation.
	Save(ctx context.Context, b *booking.Booking, expectedVersion int64) error

	// FindByIdempotencyKey returns the booking previously created or
	// transitioned with this key, or nil when the key is unused.
	FindByIdempotencyKey(ctx context.Context, key string) (*booking.Booking, error)
}

// Clone returns a deep copy so a stored booking cannot be mutated through a
// pointer handed out by Get. Without it, a caller could change a booking in
// memory and skip every invariant check in the domain.
func Clone(b *booking.Booking) *booking.Booking {
	if b == nil {
		return nil
	}

	out := *b

	if b.Offer != nil {
		offer := *b.Offer
		out.Offer = &offer
	}

	if b.Hold != nil {
		hold := *b.Hold
		out.Hold = &hold
	}

	if b.Confirm != nil {
		confirm := *b.Confirm
		out.Confirm = &confirm
	}

	out.Request = cloneRequest(b.Request)

	if b.CancelRequestedAt != nil {
		at := *b.CancelRequestedAt
		out.CancelRequestedAt = &at
	}

	out.History = append([]booking.HistoryEntry(nil), b.History...)

	return &out
}

func cloneRequest(r booking.Request) booking.Request {
	out := r

	if r.Return != nil {
		ret := *r.Return
		out.Return = &ret
	}

	if r.Passengers != nil {
		out.Passengers = append([]booking.Passenger(nil), r.Passengers...)
	}

	return out
}
