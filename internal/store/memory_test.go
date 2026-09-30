package store_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rockbotum/ai-journey-service/internal/booking"
	"github.com/rockbotum/ai-journey-service/internal/store"
)

var base = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

func newBooking(t *testing.T, id string) *booking.Booking {
	t.Helper()

	b, err := booking.New(id, "user_1", booking.Request{
		Origin:      booking.Location{Code: "LED"},
		Destination: booking.Location{Code: "JFK"},
		Departure:   base.AddDate(0, 2, 0),
		Cabin:       booking.CabinEconomy,
		Passengers:  []booking.Passenger{{FullName: "A. Passenger"}},
		Currency:    booking.CurrencyEUR,
	}.Normalise(), base)
	if err != nil {
		t.Fatalf("booking.New: %v", err)
	}

	return b
}

// newBookingTo builds a booking for an arbitrary destination so a test can
// make two requests differ in a way a fingerprint can see.
func newBookingTo(t *testing.T, id, destination string) *booking.Booking {
	t.Helper()

	b, err := booking.New(id, "user_1", booking.Request{
		Origin:      booking.Location{Code: "LED"},
		Destination: booking.Location{Code: destination},
		Departure:   base.AddDate(0, 2, 0),
		Cabin:       booking.CabinEconomy,
		Passengers:  []booking.Passenger{{FullName: "A. Passenger"}},
		Currency:    booking.CurrencyEUR,
	}.Normalise(), base)
	if err != nil {
		t.Fatalf("booking.New: %v", err)
	}

	return b
}

func TestCreateIsIdempotent(t *testing.T) {
	m := store.NewMemory()
	ctx := t.Context()

	first, err := m.Create(ctx, "idem_1", newBooking(t, "bk_1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	second, err := m.Create(ctx, "idem_1", newBooking(t, "bk_1"))
	if err != nil {
		t.Fatalf("Create retried: %v", err)
	}

	if first.ID != second.ID {
		t.Fatalf("idempotent create returned a different booking: %s vs %s", first.ID, second.ID)
	}

	if m.Count() != 1 {
		t.Fatalf("stored %d bookings, want 1", m.Count())
	}
}

func TestIdempotencyKeyReuseWithDifferentRequestIsRejected(t *testing.T) {
	m := store.NewMemory()
	ctx := t.Context()

	if _, err := m.Create(ctx, "idem_1", newBooking(t, "bk_1")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Same key, materially different request: silently accepting this would
	// drop one of the two operations.
	_, err := m.Create(ctx, "idem_1", newBookingTo(t, "bk_2", "CDG"))
	if !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want ErrIdempotencyConflict", err)
	}
}

func TestCreateRequiresKeyAndBooking(t *testing.T) {
	m := store.NewMemory()

	if _, err := m.Create(t.Context(), "", newBooking(t, "bk_1")); err == nil {
		t.Error("create without an idempotency key must fail")
	}

	if _, err := m.Create(t.Context(), "idem_1", nil); err == nil {
		t.Error("create without a booking must fail")
	}
}

func TestGetHidesOtherUsersBookings(t *testing.T) {
	m := store.NewMemory()
	ctx := t.Context()

	if _, err := m.Create(ctx, "idem_1", newBooking(t, "bk_1")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Reporting "forbidden" instead of "not found" would let a caller probe
	// which booking identifiers exist.
	_, err := m.Get(ctx, "bk_1", "user_other")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for another user's booking", err)
	}

	if _, err := m.Get(ctx, "bk_1", "user_1"); err != nil {
		t.Fatalf("owner should be able to read the booking: %v", err)
	}
}

func TestGetUnknownBooking(t *testing.T) {
	m := store.NewMemory()

	if _, err := m.Get(t.Context(), "missing", "user_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSaveRejectsStaleVersion(t *testing.T) {
	m := store.NewMemory()
	ctx := t.Context()

	created, err := m.Create(ctx, "idem_1", newBooking(t, "bk_1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Writer A loads and modifies.
	a, err := m.Get(ctx, "bk_1", "user_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	mustApply(t, a, booking.TransitionRequest{Trigger: booking.TriggerSearchRequested, Actor: booking.ActorUser, At: base})

	// Writer B loads the same version and modifies it too.
	b, err := m.Get(ctx, "bk_1", "user_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	mustApply(t, b, booking.TransitionRequest{Trigger: booking.TriggerSearchRequested, Actor: booking.ActorUser, At: base})

	if err := m.Save(ctx, a, created.Version); err != nil {
		t.Fatalf("first save: %v", err)
	}

	// B's version is now stale and must be refused.
	err = m.Save(ctx, b, created.Version)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestSaveUnknownBooking(t *testing.T) {
	m := store.NewMemory()

	if err := m.Save(t.Context(), newBooking(t, "bk_missing"), 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestStoredBookingIsIsolatedFromCallerPointers verifies copy-on-read and
// copy-on-write. Without it a caller could mutate a stored booking in memory
// and bypass every domain invariant.
func TestStoredBookingIsIsolatedFromCallerPointers(t *testing.T) {
	m := store.NewMemory()
	ctx := t.Context()

	original := newBooking(t, "bk_1")

	created, err := m.Create(ctx, "idem_1", original)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Mutate the object handed back by Create.
	created.State = booking.StateConfirmed
	created.Version = 999

	// Mutate the object originally passed in.
	original.State = booking.StateCancelled

	fetched, err := m.Get(ctx, "bk_1", "user_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if fetched.State != booking.StateIntent {
		t.Fatalf("stored state = %s, want %s: the store returned a live pointer", fetched.State, booking.StateIntent)
	}

	if fetched.Version == 999 {
		t.Fatal("stored version was mutated through a returned pointer")
	}
}

func TestCloneIsDeep(t *testing.T) {
	original := newBooking(t, "bk_1")

	mustApply(t, original, booking.TransitionRequest{Trigger: booking.TriggerSearchRequested, Actor: booking.ActorUser, At: base})
	mustApply(t, original, booking.TransitionRequest{
		Trigger: booking.TriggerSearchSucceeded, Actor: booking.ActorSupplier, At: base,
		Offer: &booking.Offer{SupplierID: "sup_a", SupplierOffer: "off_1", Total: booking.Money{Amount: 100, Currency: booking.CurrencyEUR}},
	})

	clone := store.Clone(original)

	clone.Offer.Total.Amount = 999
	clone.History[0].Trigger = booking.TriggerConfirmRequested
	clone.Request.Passengers[0].FullName = "Changed"

	if original.Offer.Total.Amount == 999 {
		t.Error("offer is shared between the original and the clone")
	}

	if original.History[0].Trigger == booking.TriggerConfirmRequested {
		t.Error("history is shared between the original and the clone")
	}

	if original.Request.Passengers[0].FullName == "Changed" {
		t.Error("passengers are shared between the original and the clone")
	}
}

func TestFindByIdempotencyKey(t *testing.T) {
	m := store.NewMemory()
	ctx := t.Context()

	if _, err := m.Create(ctx, "idem_1", newBooking(t, "bk_1")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	found, err := m.FindByIdempotencyKey(ctx, "idem_1")
	if err != nil {
		t.Fatalf("FindByIdempotencyKey: %v", err)
	}

	if found == nil || found.ID != "bk_1" {
		t.Fatalf("found = %+v, want bk_1", found)
	}

	missing, err := m.FindByIdempotencyKey(ctx, "idem_unknown")
	if err != nil {
		t.Fatalf("FindByIdempotencyKey: %v", err)
	}

	if missing != nil {
		t.Fatalf("expected nil for an unused key, got %+v", missing)
	}
}

func TestRecordIdempotencyDetectsKeyReuse(t *testing.T) {
	m := store.NewMemory()

	if err := m.RecordIdempotency("idem_1", newBooking(t, "bk_1")); err != nil {
		t.Fatalf("RecordIdempotency: %v", err)
	}

	if err := m.RecordIdempotency("idem_1", newBooking(t, "bk_1")); err != nil {
		t.Fatalf("recording the same key for the same booking must be idempotent: %v", err)
	}

	if err := m.RecordIdempotency("idem_1", newBooking(t, "bk_2")); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want ErrIdempotencyConflict", err)
	}
}

// TestConcurrentCreateIsIdempotent exercises the guarantee the database must
// also provide: two simultaneous requests with the same key create one booking.
func TestConcurrentCreateIsIdempotent(t *testing.T) {
	m := store.NewMemory()
	ctx := t.Context()

	const workers = 16

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ids  []string
		errs []error
	)

	for range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			b, err := m.Create(ctx, "idem_1", newBooking(t, "bk_1"))

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errs = append(errs, err)

				return
			}

			ids = append(ids, b.ID)
		}()
	}

	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("concurrent creates failed: %v", errs)
	}

	for _, id := range ids {
		if id != "bk_1" {
			t.Fatalf("concurrent create returned %s, want bk_1", id)
		}
	}

	if m.Count() != 1 {
		t.Fatalf("stored %d bookings, want exactly 1", m.Count())
	}
}

// TestConcurrentStaleSavesAreAllRejected covers the guarantee the database
// must provide with an UPDATE ... WHERE version = expected clause: once one
// writer has advanced the booking, every concurrent writer holding the old
// version is refused, so a superseded state can never be written back.
func TestConcurrentStaleSavesAreAllRejected(t *testing.T) {
	m := store.NewMemory()
	ctx := t.Context()

	created, err := m.Create(ctx, "idem_1", newBooking(t, "bk_1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// One writer advances the booking.
	winner, err := m.Get(ctx, "bk_1", "user_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	mustApply(t, winner, booking.TransitionRequest{Trigger: booking.TriggerSearchRequested, Actor: booking.ActorUser, At: base})

	if err := m.Save(ctx, winner, created.Version); err != nil {
		t.Fatalf("winning save: %v", err)
	}

	const workers = 8

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
		conflict int
	)

	for range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			stale, err := m.Get(ctx, "bk_1", "user_1")
			if err != nil {
				t.Errorf("Get: %v", err)

				return
			}

			// Every contender still writes against the version it read before
			// the winning write landed.
			err = m.Save(ctx, stale, created.Version)

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err == nil:
				accepted++
			case errors.Is(err, store.ErrConflict):
				conflict++
			default:
				t.Errorf("unexpected save error: %v", err)
			}
		}()
	}

	wg.Wait()

	if accepted != 0 {
		t.Fatalf("accepted %d stale writes, want 0", accepted)
	}

	if conflict != workers {
		t.Fatalf("conflicts = %d, want %d", conflict, workers)
	}
}

func mustApply(t *testing.T, b *booking.Booking, req booking.TransitionRequest) {
	t.Helper()

	if err := b.Apply(req); err != nil {
		t.Fatalf("Apply(%s): %v", req.Trigger, err)
	}
}
