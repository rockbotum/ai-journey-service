package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rockbotum/ai-journey-service/internal/booking"
	"github.com/rockbotum/ai-journey-service/internal/provider"
	"github.com/rockbotum/ai-journey-service/internal/service"
	"github.com/rockbotum/ai-journey-service/internal/store"
)

var base = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

// fakeProvider is a scriptable stand-in for a supplier. Tests must never reach
// a real external API.
type fakeProvider struct {
	offers     []booking.Offer
	searchErr  error
	hold       booking.Hold
	holdErr    error
	confirm    booking.Confirmation
	confirmErr error
	cancelErr  error
	reprice    booking.Money
	repriceErr error

	holdCalls    int
	confirmCalls int
	cancelCalls  int
	lastHoldKey  string
	lastCnfKey   string
}

func (f *fakeProvider) Search(context.Context, provider.SearchQuery) ([]booking.Offer, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}

	return f.offers, nil
}

func (f *fakeProvider) Hold(_ context.Context, _ booking.Offer, key string) (booking.Hold, error) {
	f.holdCalls++
	f.lastHoldKey = key

	if f.holdErr != nil {
		return booking.Hold{}, f.holdErr
	}

	return f.hold, nil
}

func (f *fakeProvider) Confirm(_ context.Context, _ booking.Hold, key string) (booking.Confirmation, error) {
	f.confirmCalls++
	f.lastCnfKey = key

	if f.confirmErr != nil {
		return booking.Confirmation{}, f.confirmErr
	}

	return f.confirm, nil
}

func (f *fakeProvider) Cancel(context.Context, provider.SupplierRef, string) error {
	f.cancelCalls++

	return f.cancelErr
}

func (f *fakeProvider) Reprice(context.Context, provider.SupplierRef) (booking.Money, error) {
	if f.repriceErr != nil {
		return booking.Money{}, f.repriceErr
	}

	return f.reprice, nil
}

type fixture struct {
	svc   *service.Service
	st    *store.Memory
	prov  *fakeProvider
	clock *fakeClock
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) nowFn() time.Time { return c.now }

func newFixture(t *testing.T) *fixture {
	t.Helper()

	clock := &fakeClock{now: base}
	prov := &fakeProvider{
		offers: []booking.Offer{{
			SupplierID:    "sup_a",
			SupplierOffer: "off_1",
			Total:         booking.Money{Amount: 45_000, Currency: booking.CurrencyEUR},
		}},
		hold: booking.Hold{
			SupplierID:   "sup_a",
			SupplierRef:  "hold_1",
			SupplierName: "off_1",
			Total:        booking.Money{Amount: 45_000, Currency: booking.CurrencyEUR},
			ExpiresAt:    base.Add(20 * time.Minute),
		},
		confirm: booking.Confirmation{
			SupplierID:  "sup_a",
			SupplierRef: "cnf_1",
			Total:       booking.Money{Amount: 45_000, Currency: booking.CurrencyEUR},
			ConfirmedAt: base,
		},
		reprice: booking.Money{Amount: 45_000, Currency: booking.CurrencyEUR},
	}

	st := store.NewMemory()

	var counter int

	svc, err := service.New(st, prov, nil, clock.nowFn, func() string {
		counter++

		return fmt.Sprintf("bk_%d", counter)
	})
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}

	return &fixture{svc: svc, st: st, prov: prov, clock: clock}
}

func (f *fixture) request() booking.Request {
	return booking.Request{
		Origin:      booking.Location{Code: "LED"},
		Destination: booking.Location{Code: "JFK"},
		Departure:   base.AddDate(0, 2, 0),
		Cabin:       booking.CabinEconomy,
		Passengers:  []booking.Passenger{{FullName: "A. Passenger"}},
		Currency:    booking.CurrencyEUR,
	}.Normalise()
}

func (f *fixture) create(t *testing.T) *booking.Booking {
	t.Helper()

	b, err := f.svc.Create(t.Context(), service.CreateCommand{
		UserID:         "user_1",
		Request:        f.request(),
		IdempotencyKey: "idem_create_1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	return b
}

func (f *fixture) held(t *testing.T) *booking.Booking {
	t.Helper()

	f.create(t)

	if _, err := f.svc.Search(t.Context(), "bk_1", "user_1"); err != nil {
		t.Fatalf("Search: %v", err)
	}

	b, err := f.svc.Hold(t.Context(), service.HoldCommand{
		BookingID:      "bk_1",
		UserID:         "user_1",
		IdempotencyKey: "idem_hold_1",
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	return b
}

func TestCreateRegistersIntentWithoutContactingSupplier(t *testing.T) {
	f := newFixture(t)

	b := f.create(t)

	if b.State != booking.StateIntent {
		t.Fatalf("state = %s, want %s", b.State, booking.StateIntent)
	}

	if f.prov.holdCalls != 0 || f.prov.confirmCalls != 0 {
		t.Fatal("creating a booking must not contact the supplier")
	}
}

func TestCreateIsIdempotent(t *testing.T) {
	f := newFixture(t)

	first := f.create(t)
	second, err := f.svc.Create(t.Context(), service.CreateCommand{
		UserID:         "user_1",
		Request:        f.request(),
		IdempotencyKey: "idem_create_1",
	})
	if err != nil {
		t.Fatalf("Create retried: %v", err)
	}

	if first.ID != second.ID {
		t.Fatalf("retry created a second booking: %s vs %s", first.ID, second.ID)
	}

	if f.st.Count() != 1 {
		t.Fatalf("stored %d bookings, want 1", f.st.Count())
	}
}

func TestCreateRequiresIdempotencyKey(t *testing.T) {
	f := newFixture(t)

	_, err := f.svc.Create(t.Context(), service.CreateCommand{
		UserID:  "user_1",
		Request: f.request(),
	})
	if err == nil {
		t.Fatal("create without an idempotency key must fail")
	}
}

func TestCreateRejectsADepartureInThePast(t *testing.T) {
	f := newFixture(t)

	req := f.request()
	req.Departure = f.clock.now.AddDate(0, 0, -1)

	_, err := f.svc.Create(t.Context(), service.CreateCommand{
		UserID:         "user_1",
		Request:        req,
		IdempotencyKey: "idem_1",
	})
	if !errors.Is(err, booking.ErrDepartureInPast) {
		t.Fatalf("err = %v, want ErrDepartureInPast", err)
	}
}

func TestGetHidesAnotherUsersBooking(t *testing.T) {
	f := newFixture(t)
	f.create(t)

	_, err := f.svc.Get(t.Context(), "bk_1", "user_other")
	if !errors.Is(err, service.ErrBookingNotFound) {
		t.Fatalf("err = %v, want ErrBookingNotFound", err)
	}
}

func TestSearchStoresTheOffer(t *testing.T) {
	f := newFixture(t)
	f.create(t)

	offers, err := f.svc.Search(t.Context(), "bk_1", "user_1")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(offers) != 1 {
		t.Fatalf("offers = %d, want 1", len(offers))
	}

	b, err := f.svc.Get(t.Context(), "bk_1", "user_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if b.State != booking.StateOffered {
		t.Fatalf("state = %s, want %s", b.State, booking.StateOffered)
	}
}

func TestSearchFailureIsRecorded(t *testing.T) {
	f := newFixture(t)
	f.create(t)
	f.prov.searchErr = errors.New("supplier exploded")

	if _, err := f.svc.Search(t.Context(), "bk_1", "user_1"); err == nil {
		t.Fatal("expected the search to fail")
	}

	b, _ := f.svc.Get(t.Context(), "bk_1", "user_1")
	if b.State != booking.StateFailed {
		t.Fatalf("state = %s, want %s", b.State, booking.StateFailed)
	}

	if b.Failure == "" {
		t.Fatal("failure reason must be recorded for investigation")
	}
}

func TestHoldPassesTheIdempotencyKeyToTheSupplier(t *testing.T) {
	f := newFixture(t)
	f.held(t)

	if f.prov.lastHoldKey != "idem_hold_1" {
		t.Fatalf("supplier received key %q, want idem_hold_1", f.prov.lastHoldKey)
	}
}

func TestHoldFailureIsRecorded(t *testing.T) {
	f := newFixture(t)
	f.create(t)
	f.svc.Search(t.Context(), "bk_1", "user_1")
	f.prov.holdErr = errors.New("supplier refused")

	if _, err := f.svc.Hold(t.Context(), service.HoldCommand{
		BookingID: "bk_1", UserID: "user_1", IdempotencyKey: "idem_hold_1",
	}); err == nil {
		t.Fatal("expected the hold to fail")
	}

	b, _ := f.svc.Get(t.Context(), "bk_1", "user_1")
	if b.State != booking.StateFailed {
		t.Fatalf("state = %s, want %s", b.State, booking.StateFailed)
	}
}

// TestConfirmWithoutExplicitConsentIsRefused is the central AI-safety
// assertion: a confirmation cannot be produced without a user action, and the
// supplier is never contacted in that case.
func TestConfirmWithoutExplicitConsentIsRefused(t *testing.T) {
	f := newFixture(t)
	f.held(t)

	_, err := f.svc.Confirm(t.Context(), service.ConfirmCommand{
		BookingID:      "bk_1",
		UserID:         "user_1",
		IdempotencyKey: "idem_cnf_1",
		UserConfirmed:  false,
	})
	if !errors.Is(err, service.ErrConsentRequired) {
		t.Fatalf("err = %v, want ErrConsentRequired", err)
	}

	if f.prov.confirmCalls != 0 {
		t.Fatal("the supplier was contacted without an explicit user confirmation")
	}
}

func TestConfirmReachesConfirmed(t *testing.T) {
	f := newFixture(t)
	f.held(t)

	b, err := f.svc.Confirm(t.Context(), service.ConfirmCommand{
		BookingID:      "bk_1",
		UserID:         "user_1",
		IdempotencyKey: "idem_cnf_1",
		UserConfirmed:  true,
	})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	if b.State != booking.StateConfirmed {
		t.Fatalf("state = %s, want %s", b.State, booking.StateConfirmed)
	}

	if b.Confirm == nil || b.Confirm.SupplierRef != "cnf_1" {
		t.Fatal("supplier confirmation reference must be stored")
	}
}

// TestPriceChangeBlocksConfirmation verifies the re-check that separates
// "the user agreed to this price" from "the supplier now quotes this price".
func TestPriceChangeBlocksConfirmation(t *testing.T) {
	f := newFixture(t)
	f.held(t)
	f.prov.reprice = booking.Money{Amount: 51_000, Currency: booking.CurrencyEUR}

	_, err := f.svc.Confirm(t.Context(), service.ConfirmCommand{
		BookingID:      "bk_1",
		UserID:         "user_1",
		IdempotencyKey: "idem_cnf_1",
		UserConfirmed:  true,
	})
	if !errors.Is(err, service.ErrPriceReconfirm) {
		t.Fatalf("err = %v, want ErrPriceReconfirm", err)
	}

	if f.prov.confirmCalls != 0 {
		t.Fatal("the supplier was asked to confirm at a price the user did not accept")
	}

	b, _ := f.svc.Get(t.Context(), "bk_1", "user_1")
	if b.State != booking.StateHeld {
		t.Fatalf("state = %s, want %s: a failed price check must not advance the booking", b.State, booking.StateHeld)
	}
}

// TestUnverifiablePriceBlocksConfirmation covers the case where the supplier
// cannot be asked: an unknown price is not a price that can be charged.
func TestUnverifiablePriceBlocksConfirmation(t *testing.T) {
	f := newFixture(t)
	f.held(t)
	f.prov.repriceErr = errors.New("supplier unreachable")

	_, err := f.svc.Confirm(t.Context(), service.ConfirmCommand{
		BookingID:      "bk_1",
		UserID:         "user_1",
		IdempotencyKey: "idem_cnf_1",
		UserConfirmed:  true,
	})
	if !errors.Is(err, service.ErrPriceReconfirm) {
		t.Fatalf("err = %v, want ErrPriceReconfirm", err)
	}

	if f.prov.confirmCalls != 0 {
		t.Fatal("the supplier must not be asked to confirm when the price is unknown")
	}
}

func TestConfirmAfterHoldExpiryIsRefused(t *testing.T) {
	f := newFixture(t)
	f.held(t)

	f.clock.now = base.Add(21 * time.Minute)

	_, err := f.svc.Confirm(t.Context(), service.ConfirmCommand{
		BookingID:      "bk_1",
		UserID:         "user_1",
		IdempotencyKey: "idem_cnf_1",
		UserConfirmed:  true,
	})
	if !errors.Is(err, booking.ErrHoldExpired) {
		t.Fatalf("err = %v, want ErrHoldExpired", err)
	}

	if f.prov.confirmCalls != 0 {
		t.Fatal("an expired hold must never be confirmed")
	}
}

// TestConfirmIsIdempotent verifies a retried confirmation returns the original
// result instead of creating a second booking at the supplier.
func TestConfirmIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.held(t)

	cmd := service.ConfirmCommand{
		BookingID: "bk_1", UserID: "user_1", IdempotencyKey: "idem_cnf_1", UserConfirmed: true,
	}

	first, err := f.svc.Confirm(t.Context(), cmd)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	second, err := f.svc.Confirm(t.Context(), cmd)
	if err != nil {
		t.Fatalf("Confirm retried: %v", err)
	}

	if first.ID != second.ID || second.State != booking.StateConfirmed {
		t.Fatalf("retry did not return the original result: %+v", second)
	}

	if f.prov.confirmCalls != 1 {
		t.Fatalf("supplier was called %d times, want 1: a retry must not double-book", f.prov.confirmCalls)
	}
}

func TestConfirmRequiresIdempotencyKey(t *testing.T) {
	f := newFixture(t)
	f.held(t)

	_, err := f.svc.Confirm(t.Context(), service.ConfirmCommand{
		BookingID: "bk_1", UserID: "user_1", UserConfirmed: true,
	})
	if err == nil {
		t.Fatal("confirmation without an idempotency key must fail")
	}
}

func TestConfirmFailureIsRecorded(t *testing.T) {
	f := newFixture(t)
	f.held(t)
	f.prov.confirmErr = errors.New("supplier refused")

	if _, err := f.svc.Confirm(t.Context(), service.ConfirmCommand{
		BookingID: "bk_1", UserID: "user_1", IdempotencyKey: "idem_cnf_1", UserConfirmed: true,
	}); err == nil {
		t.Fatal("expected the confirmation to fail")
	}

	b, _ := f.svc.Get(t.Context(), "bk_1", "user_1")
	if b.State != booking.StateFailed {
		t.Fatalf("state = %s, want %s", b.State, booking.StateFailed)
	}
}

func TestCancelRequiresConsent(t *testing.T) {
	f := newFixture(t)
	f.held(t)

	_, err := f.svc.Cancel(t.Context(), service.CancelCommand{
		BookingID: "bk_1", UserID: "user_1", IdempotencyKey: "idem_cxl_1", UserConfirmed: false,
	})
	if !errors.Is(err, service.ErrConsentRequired) {
		t.Fatalf("err = %v, want ErrConsentRequired", err)
	}

	if f.prov.cancelCalls != 0 {
		t.Fatal("the supplier was contacted without an explicit user confirmation")
	}
}

func TestCancelReachesCancelled(t *testing.T) {
	f := newFixture(t)
	f.held(t)

	b, err := f.svc.Cancel(t.Context(), service.CancelCommand{
		BookingID: "bk_1", UserID: "user_1", IdempotencyKey: "idem_cxl_1", UserConfirmed: true,
	})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	if b.State != booking.StateCancelled {
		t.Fatalf("state = %s, want %s", b.State, booking.StateCancelled)
	}

	if b.CancelRequestedAt == nil {
		t.Fatal("the cancellation must record when and on whose authority it started")
	}
}

func TestCancelConfirmedBooking(t *testing.T) {
	f := newFixture(t)
	f.held(t)

	if _, err := f.svc.Confirm(t.Context(), service.ConfirmCommand{
		BookingID: "bk_1", UserID: "user_1", IdempotencyKey: "idem_cnf_1", UserConfirmed: true,
	}); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	b, err := f.svc.Cancel(t.Context(), service.CancelCommand{
		BookingID: "bk_1", UserID: "user_1", IdempotencyKey: "idem_cxl_1", UserConfirmed: true,
	})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	if b.State != booking.StateCancelled {
		t.Fatalf("state = %s, want %s", b.State, booking.StateCancelled)
	}
}

func TestExpireHoldsMarksLapsedReservation(t *testing.T) {
	f := newFixture(t)
	f.held(t)

	f.clock.now = base.Add(21 * time.Minute)

	if err := f.svc.ExpireHolds(t.Context(), "bk_1", "user_1"); err != nil {
		t.Fatalf("ExpireHolds: %v", err)
	}

	b, _ := f.svc.Get(t.Context(), "bk_1", "user_1")
	if b.State != booking.StateExpired {
		t.Fatalf("state = %s, want %s", b.State, booking.StateExpired)
	}

	if b.Hold != nil {
		t.Fatal("a lapsed hold must not remain attached to the booking")
	}
}

func TestExpireHoldsLeavesLiveHoldsAlone(t *testing.T) {
	f := newFixture(t)
	f.held(t)

	if err := f.svc.ExpireHolds(t.Context(), "bk_1", "user_1"); err != nil {
		t.Fatalf("ExpireHolds: %v", err)
	}

	b, _ := f.svc.Get(t.Context(), "bk_1", "user_1")
	if b.State != booking.StateHeld {
		t.Fatalf("state = %s, want %s: a live hold must not expire", b.State, booking.StateHeld)
	}
}

func TestAnotherUserCannotActOnABooking(t *testing.T) {
	f := newFixture(t)
	f.held(t)

	if _, err := f.svc.Confirm(t.Context(), service.ConfirmCommand{
		BookingID: "bk_1", UserID: "user_other", IdempotencyKey: "idem_x", UserConfirmed: true,
	}); !errors.Is(err, service.ErrBookingNotFound) {
		t.Fatalf("err = %v, want ErrBookingNotFound", err)
	}

	if f.prov.confirmCalls != 0 {
		t.Fatal("another user triggered a supplier call")
	}
}

func TestServiceNewRejectsMissingDependencies(t *testing.T) {
	if _, err := service.New(nil, &fakeProvider{}, nil, nil, func() string { return "x" }); err == nil {
		t.Error("a nil store must be rejected")
	}

	if _, err := service.New(store.NewMemory(), nil, nil, nil, func() string { return "x" }); err == nil {
		t.Error("a nil provider must be rejected")
	}

	if _, err := service.New(store.NewMemory(), &fakeProvider{}, nil, nil, nil); err == nil {
		t.Error("a nil id generator must be rejected")
	}
}
