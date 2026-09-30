package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/rockbotum/ai-journey-service/internal/booking"
)

// Memory is an in-memory Store used in tests and in local development without
// a database. It reproduces the semantics a PostgreSQL implementation must
// provide: idempotency keys are unique, and a stale version is rejected.
//
// It is safe for concurrent use.
type Memory struct {
	mu sync.RWMutex

	bookings map[string]*booking.Booking

	// idempotency maps a key to the booking it produced, together with a
	// fingerprint of the request that created it.
	idempotency map[string]idempotencyEntry
}

type idempotencyEntry struct {
	bookingID   string
	fingerprint string
}

// NewMemory creates an empty store.
func NewMemory() *Memory {
	return &Memory{
		bookings:    make(map[string]*booking.Booking),
		idempotency: make(map[string]idempotencyEntry),
	}
}

// Create implements Store.
func (m *Memory) Create(_ context.Context, key string, b *booking.Booking) (*booking.Booking, error) {
	if b == nil {
		return nil, fmt.Errorf("store: booking is required")
	}

	if key == "" {
		return nil, fmt.Errorf("store: idempotency key is required")
	}

	fp := fingerprintBooking(b)

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.idempotency[key]; ok {
		if existing.fingerprint != fp {
			return nil, fmt.Errorf("%w: key %q", ErrIdempotencyConflict, key)
		}

		return Clone(m.bookings[existing.bookingID]), nil
	}

	stored := Clone(b)
	m.bookings[stored.ID] = stored
	m.idempotency[key] = idempotencyEntry{bookingID: stored.ID, fingerprint: fp}

	return Clone(stored), nil
}

// Get implements Store.
func (m *Memory) Get(_ context.Context, id, userID string) (*booking.Booking, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	b, ok := m.bookings[id]
	if !ok {
		return nil, ErrNotFound
	}

	// A booking owned by another user is reported as missing, so the API
	// cannot be used to discover which identifiers exist.
	if b.UserID != userID {
		return nil, ErrNotFound
	}

	return Clone(b), nil
}

// Save implements Store with optimistic concurrency control.
func (m *Memory) Save(_ context.Context, b *booking.Booking, expectedVersion int64) error {
	if b == nil {
		return fmt.Errorf("store: booking is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	current, ok := m.bookings[b.ID]
	if !ok {
		return ErrNotFound
	}

	if current.Version != expectedVersion {
		return fmt.Errorf("%w: booking %s is at version %d, expected %d", ErrConflict, b.ID, current.Version, expectedVersion)
	}

	// Copy on write keeps the stored value unreachable from the caller's
	// pointer.
	m.bookings[b.ID] = Clone(b)

	return nil
}

// FindByIdempotencyKey implements Store.
func (m *Memory) FindByIdempotencyKey(_ context.Context, key string) (*booking.Booking, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, ok := m.idempotency[key]
	if !ok {
		return nil, nil
	}

	return Clone(m.bookings[entry.bookingID]), nil
}

// RecordIdempotency binds a key to an existing booking. It is used when a
// transition is applied to a booking that already exists, so replaying the
// same request is recognised as the same operation.
//
// Unlike Create, this compares the booking identity rather than a fingerprint of
// the request: the key is bound to one concrete booking, so the same key
// pointing at a different booking is ambiguous even when both bookings happen
// to carry an identical travel request.
func (m *Memory) RecordIdempotency(key string, b *booking.Booking) error {
	if key == "" {
		return fmt.Errorf("store: idempotency key is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.idempotency[key]; ok {
		if existing.bookingID != b.ID {
			return fmt.Errorf("%w: key %q", ErrIdempotencyConflict, key)
		}

		return nil
	}

	m.idempotency[key] = idempotencyEntry{bookingID: b.ID, fingerprint: fingerprintBooking(b)}

	return nil
}

// Count reports how many bookings are stored. It exists for tests and
// diagnostics.
func (m *Memory) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.bookings)
}

// IDs returns the stored identifiers in a stable order.
func (m *Memory) IDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := slices.Collect(maps.Keys(m.bookings))
	slices.Sort(out)

	return out
}

// fingerprintBooking identifies the request that produced a booking.
//
// The booking ID is deliberately excluded: a genuine retry of the same request
// allocates a fresh ID, and including it would make every retry look like a
// different request. Volatile fields such as the version and timestamps are
// excluded for the same reason. What remains is the caller's intent, so a
// materially different request reusing one key is still detected.
func fingerprintBooking(b *booking.Booking) string {
	h := sha256.New()

	fmt.Fprintf(h, "user=%s;", b.UserID)
	fmt.Fprintf(h, "origin=%s;", b.Request.Origin.Code)
	fmt.Fprintf(h, "destination=%s;", b.Request.Destination.Code)
	fmt.Fprintf(h, "departure=%s;", b.Request.Departure.UTC().Format("2006-01-02"))

	if b.Request.Return != nil {
		fmt.Fprintf(h, "return=%s;", b.Request.Return.UTC().Format("2006-01-02"))
	}

	fmt.Fprintf(h, "cabin=%s;", b.Request.Cabin)
	fmt.Fprintf(h, "currency=%s;", b.Request.Currency)
	fmt.Fprintf(h, "travellers=%d;", b.Request.TravellerCount())

	return hex.EncodeToString(h.Sum(nil))
}

// Interface compliance is checked at compile time.
var _ Store = (*Memory)(nil)
