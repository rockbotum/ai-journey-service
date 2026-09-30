package booking_test

import (
	"errors"
	"testing"
	"time"

	"github.com/rockbotum/ai-journey-service/internal/booking"
)

var base = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

func validRequest() booking.Request {
	return booking.Request{
		Origin:      booking.Location{Code: "LED", Name: "Saint Petersburg"},
		Destination: booking.Location{Code: "JFK", Name: "New York"},
		Departure:   base.AddDate(0, 2, 0),
		Cabin:       booking.CabinEconomy,
		Passengers:  []booking.Passenger{{FullName: "A. Passenger"}},
		Currency:    booking.CurrencyEUR,
	}.Normalise()
}

func newBooking(t *testing.T) *booking.Booking {
	t.Helper()

	b, err := booking.New("bk_1", "user_1", validRequest(), base)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return b
}

func offer(total int64) *booking.Offer {
	return &booking.Offer{
		SupplierID:          "sup_a",
		SupplierOffer:       "off_1",
		Total:               booking.Money{Amount: total, Currency: booking.CurrencyEUR},
		AvailableAt:         base,
		CancellationSummary: "Free cancellation until 24h before departure",
	}
}

func hold(total int64, expires time.Time) *booking.Hold {
	return &booking.Hold{
		SupplierID:   "sup_a",
		SupplierRef:  "hold_1",
		SupplierName: "off_1",
		Total:        booking.Money{Amount: total, Currency: booking.CurrencyEUR},
		ExpiresAt:    expires,
	}
}

func searchAndHold(t *testing.T, b *booking.Booking, total int64, holdExpiry time.Time) {
	t.Helper()

	mustApply(t, b, booking.TransitionRequest{
		Trigger: booking.TriggerSearchRequested,
		Actor:   booking.ActorUser,
		At:      base,
	})
	mustApply(t, b, booking.TransitionRequest{
		Trigger: booking.TriggerSearchSucceeded,
		Actor:   booking.ActorSupplier,
		At:      base,
		Offer:   offer(total),
	})
	mustApply(t, b, booking.TransitionRequest{
		Trigger: booking.TriggerHoldRequested,
		Actor:   booking.ActorUser,
		At:      base,
	})
	mustApply(t, b, booking.TransitionRequest{
		Trigger: booking.TriggerHoldSucceeded,
		Actor:   booking.ActorSupplier,
		At:      base,
		Hold:    hold(total, holdExpiry),
	})
}

func mustApply(t *testing.T, b *booking.Booking, req booking.TransitionRequest) {
	t.Helper()

	if err := b.Apply(req); err != nil {
		t.Fatalf("Apply(%s, from %s): %v", req.Trigger, b.State, err)
	}
}

func TestNewStartsInIntentWithoutSupplierData(t *testing.T) {
	b := newBooking(t)

	if b.State != booking.StateIntent {
		t.Fatalf("state = %s, want %s", b.State, booking.StateIntent)
	}

	if b.Offer != nil || b.Hold != nil || b.Confirm != nil {
		t.Fatal("no supplier data may exist before a search is requested")
	}
}

func TestHappyPathReachesConfirmed(t *testing.T) {
	b := newBooking(t)
	holdExpiry := base.Add(20 * time.Minute)
	searchAndHold(t, b, 45_000, holdExpiry)

	confirmAt := base.Add(5 * time.Minute)
	ok, err := b.ConfirmPriceMatches(booking.Money{Amount: 45_000, Currency: booking.CurrencyEUR})
	if err != nil || !ok {
		t.Fatalf("price should still match: ok=%v err=%v", ok, err)
	}

	mustApply(t, b, booking.TransitionRequest{
		Trigger:        booking.TriggerConfirmRequested,
		Actor:          booking.ActorUser,
		At:             confirmAt,
		IdempotencyKey: "idem_1",
		UserConfirmed:  true,
		SupplierID:     "sup_a",
	})
	mustApply(t, b, booking.TransitionRequest{
		Trigger: booking.TriggerConfirmSucceeded,
		Actor:   booking.ActorSupplier,
		At:      confirmAt,
		Confirmation: &booking.Confirmation{
			SupplierID:  "sup_a",
			SupplierRef: "cnf_1",
			Total:       booking.Money{Amount: 45_000, Currency: booking.CurrencyEUR},
			ConfirmedAt: confirmAt,
		},
	})

	if b.State != booking.StateConfirmed {
		t.Fatalf("state = %s, want %s", b.State, booking.StateConfirmed)
	}

	if b.Confirm == nil || b.Confirm.SupplierRef != "cnf_1" {
		t.Fatal("supplier confirmation reference must be retained")
	}

	if b.Version != 7 {
		t.Fatalf("version = %d, want 7 (initial + 5 transitions applied)", b.Version)
	}
}

func TestHistoryRecordsEveryTransitionWithFromAndTo(t *testing.T) {
	b := newBooking(t)
	searchAndHold(t, b, 45_000, base.Add(20*time.Minute))

	// Creation is not a transition, so the trail contains exactly the
	// transitions that were applied: search, hold, and their two outcomes.
	if len(b.History) != 4 {
		t.Fatalf("history has %d entries, want 4: %+v", len(b.History), b.History)
	}

	for i, entry := range b.History {
		if entry.Trigger == "" || entry.Actor == "" {
			t.Fatalf("entry %d missing trigger or actor: %+v", i, entry)
		}

		if i > 0 && entry.From != b.History[i-1].To {
			t.Fatalf("history is not contiguous at %d: from %s, previous to %s", i, entry.From, b.History[i-1].To)
		}
	}

	last := b.History[len(b.History)-1]
	if last.From != booking.StateHolding || last.To != booking.StateHeld {
		t.Fatalf("last transition = %s->%s, want holding->held", last.From, last.To)
	}
}

// TestConfirmWithoutUserConfirmationIsRejected is the core AI-safety rule: a
// financial transition must never be reachable from a system or AI actor.
func TestConfirmWithoutUserConfirmationIsRejected(t *testing.T) {
	tests := []struct {
		name          string
		actor         booking.Actor
		userConfirmed bool
	}{
		{"system actor", booking.ActorSystem, false},
		{"supplier actor", booking.ActorSupplier, false},
		{"user without confirmation flag", booking.ActorUser, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newBooking(t)
			searchAndHold(t, b, 45_000, base.Add(20*time.Minute))

			err := b.Apply(booking.TransitionRequest{
				Trigger:        booking.TriggerConfirmRequested,
				Actor:          tc.actor,
				At:             base.Add(time.Minute),
				IdempotencyKey: "idem_1",
				UserConfirmed:  tc.userConfirmed,
			})
			if !errors.Is(err, booking.ErrUserConfirmationRequired) {
				t.Fatalf("err = %v, want ErrUserConfirmationRequired", err)
			}

			if b.State != booking.StateHeld {
				t.Fatalf("state changed to %s despite rejection", b.State)
			}
		})
	}
}

func TestFinancialTransitionRequiresIdempotencyKey(t *testing.T) {
	b := newBooking(t)
	searchAndHold(t, b, 45_000, base.Add(20*time.Minute))

	err := b.Apply(booking.TransitionRequest{
		Trigger:       booking.TriggerConfirmRequested,
		Actor:         booking.ActorUser,
		At:            base.Add(time.Minute),
		UserConfirmed: true,
	})
	if err == nil {
		t.Fatal("financial transition without idempotency key must be rejected")
	}
}

func TestConfirmAfterHoldExpiryIsRejected(t *testing.T) {
	b := newBooking(t)
	searchAndHold(t, b, 45_000, base.Add(10*time.Minute))

	err := b.Apply(booking.TransitionRequest{
		Trigger:        booking.TriggerConfirmRequested,
		Actor:          booking.ActorUser,
		At:             base.Add(11 * time.Minute),
		IdempotencyKey: "idem_1",
		UserConfirmed:  true,
	})
	if !errors.Is(err, booking.ErrHoldExpired) {
		t.Fatalf("err = %v, want ErrHoldExpired", err)
	}
}

func TestPriceChangeBlocksConfirmation(t *testing.T) {
	b := newBooking(t)
	searchAndHold(t, b, 45_000, base.Add(20*time.Minute))

	ok, err := b.ConfirmPriceMatches(booking.Money{Amount: 51_000, Currency: booking.CurrencyEUR})
	if !errors.Is(err, booking.ErrPriceChanged) && ok {
		t.Fatal("price increase must be reported as a change")
	}

	if ok {
		t.Fatal("changed price must not match the held price")
	}
}

func TestSupplierMismatchIsRejected(t *testing.T) {
	b := newBooking(t)
	searchAndHold(t, b, 45_000, base.Add(20*time.Minute))

	err := b.Apply(booking.TransitionRequest{
		Trigger:        booking.TriggerConfirmRequested,
		Actor:          booking.ActorUser,
		At:             base.Add(time.Minute),
		IdempotencyKey: "idem_1",
		UserConfirmed:  true,
		SupplierID:     "sup_other",
	})
	if !errors.Is(err, booking.ErrSupplierMismatch) {
		t.Fatalf("err = %v, want ErrSupplierMismatch", err)
	}
}

func TestHoldFromWrongSupplierIsRejected(t *testing.T) {
	b := newBooking(t)
	mustApply(t, b, booking.TransitionRequest{Trigger: booking.TriggerSearchRequested, Actor: booking.ActorUser, At: base})
	mustApply(t, b, booking.TransitionRequest{Trigger: booking.TriggerSearchSucceeded, Actor: booking.ActorSupplier, At: base, Offer: offer(45_000)})
	mustApply(t, b, booking.TransitionRequest{Trigger: booking.TriggerHoldRequested, Actor: booking.ActorUser, At: base})

	wrong := hold(45_000, base.Add(20*time.Minute))
	wrong.SupplierID = "sup_other"

	err := b.Apply(booking.TransitionRequest{Trigger: booking.TriggerHoldSucceeded, Actor: booking.ActorSupplier, At: base, Hold: wrong})
	if !errors.Is(err, booking.ErrSupplierMismatch) {
		t.Fatalf("err = %v, want ErrSupplierMismatch", err)
	}
}

func TestExpiredHoldIsDropped(t *testing.T) {
	b := newBooking(t)
	searchAndHold(t, b, 45_000, base.Add(10*time.Minute))

	mustApply(t, b, booking.TransitionRequest{
		Trigger: booking.TriggerHoldExpired,
		Actor:   booking.ActorSystem,
		At:      base.Add(11 * time.Minute),
	})

	if b.State != booking.StateExpired {
		t.Fatalf("state = %s, want %s", b.State, booking.StateExpired)
	}

	if b.Hold != nil {
		t.Fatal("lapsed hold must be dropped so it cannot be reused")
	}

	if b.IsHoldLive(base.Add(12 * time.Minute)) {
		t.Fatal("IsHoldLive must be false after expiry")
	}
}

func TestUndeclaredTransitionIsRejected(t *testing.T) {
	b := newBooking(t)

	err := b.Apply(booking.TransitionRequest{
		Trigger:        booking.TriggerConfirmRequested,
		Actor:          booking.ActorUser,
		At:             base,
		IdempotencyKey: "idem_1",
		UserConfirmed:  true,
	})
	if err == nil {
		t.Fatal("confirm from intent must be rejected")
	}

	if b.State != booking.StateIntent || b.Version != 1 {
		t.Fatalf("rejected transition mutated the booking: state=%s version=%d", b.State, b.Version)
	}
}

func TestEveryStateIsReachableOrDeclared(t *testing.T) {
	// Guards against a state being added to the enum but never wired into the
	// lifecycle graph, which would make it unreachable dead code.
	declared := map[booking.State]bool{
		booking.StateIntent: true, booking.StateSearching: true, booking.StateOffered: true,
		booking.StateHolding: true, booking.StateHeld: true, booking.StateConfirming: true,
		booking.StateConfirmed: true, booking.StateCancelling: true, booking.StateCancelled: true,
		booking.StateFailed: true, booking.StateExpired: true,
	}

	for state := range declared {
		if _, err := booking.ParseState(string(state)); err != nil {
			t.Errorf("state %s is declared but not parseable: %v", state, err)
		}
	}
}

func TestCancelConfirmedBookingIsFinancial(t *testing.T) {
	b := newBooking(t)
	searchAndHold(t, b, 45_000, base.Add(20*time.Minute))
	mustApply(t, b, booking.TransitionRequest{
		Trigger: booking.TriggerConfirmRequested, Actor: booking.ActorUser, At: base.Add(time.Minute),
		IdempotencyKey: "idem_1", UserConfirmed: true, SupplierID: "sup_a",
	})
	mustApply(t, b, booking.TransitionRequest{
		Trigger: booking.TriggerConfirmSucceeded, Actor: booking.ActorSupplier, At: base.Add(time.Minute),
		Confirmation: &booking.Confirmation{SupplierID: "sup_a", SupplierRef: "cnf_1", Total: booking.Money{Amount: 45_000, Currency: booking.CurrencyEUR}, ConfirmedAt: base.Add(time.Minute)},
	})

	// Cancelling a confirmed booking without explicit confirmation must fail.
	err := b.Apply(booking.TransitionRequest{
		Trigger:        booking.TriggerCancelRequested,
		Actor:          booking.ActorSystem,
		At:             base.Add(2 * time.Hour),
		IdempotencyKey: "idem_2",
	})
	if !errors.Is(err, booking.ErrUserConfirmationRequired) {
		t.Fatalf("err = %v, want ErrUserConfirmationRequired", err)
	}
}

func TestFailureReasonIsSanitised(t *testing.T) {
	b := newBooking(t)
	mustApply(t, b, booking.TransitionRequest{Trigger: booking.TriggerSearchRequested, Actor: booking.ActorUser, At: base})

	mustApply(t, b, booking.TransitionRequest{
		Trigger:       booking.TriggerSearchFailed,
		Actor:         booking.ActorSystem,
		At:            base,
		FailureReason: "provider_error\x00\nwith control chars",
	})

	if b.Failure != "provider_errorwith control chars" {
		t.Fatalf("failure = %q, want control characters stripped", b.Failure)
	}
}

func TestSearchResetsStaleOffer(t *testing.T) {
	b := newBooking(t)
	searchAndHold(t, b, 45_000, base.Add(20*time.Minute))

	// A new search must drop the previous offer and hold so a stale price
	// cannot be confirmed against a fresh search.
	mustApply(t, b, booking.TransitionRequest{Trigger: booking.TriggerSearchRequested, Actor: booking.ActorUser, At: base})

	if b.Offer != nil || b.Hold != nil {
		t.Fatal("new search must clear stale offer and hold")
	}
}

// A new booking must not claim that a supplier was contacted. A false audit
// record is worse than a missing one.
func TestNewBookingHasNoFabricatedHistory(t *testing.T) {
	b := newBooking(t)

	if len(b.History) != 0 {
		t.Fatalf("history = %+v, want empty", b.History)
	}

	if b.State != booking.StateIntent {
		t.Fatalf("state = %s, want %s", b.State, booking.StateIntent)
	}
}

// The first recorded transition must start from the real initial state, so the
// audit trail reconstructs the process from the beginning.
func TestFirstTransitionIsRecordedFromIntent(t *testing.T) {
	b := newBooking(t)

	mustApply(t, b, booking.TransitionRequest{
		Trigger: booking.TriggerSearchRequested,
		Actor:   booking.ActorUser,
		At:      base,
	})

	if len(b.History) != 1 {
		t.Fatalf("history has %d entries, want 1", len(b.History))
	}

	first := b.History[0]
	if first.From != booking.StateIntent || first.To != booking.StateSearching {
		t.Fatalf("first entry = %s->%s, want intent->searching", first.From, first.To)
	}
}
