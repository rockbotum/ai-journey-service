package httpapi_test

import (
	"context"
	"time"

	"github.com/rockbotum/ai-journey-service/internal/booking"
	"github.com/rockbotum/ai-journey-service/internal/provider"
)

// fakeProvider is a deterministic stand-in for a supplier. It keeps the
// transport tests independent of any network or vendor.
type fakeProvider struct {
	now *fakeClock
	// priceMinor is mutable so a test can simulate the supplier raising the
	// price between the hold and the confirmation.
	priceMinor int64
	currency   booking.Currency

	holdCalls    int
	confirmCalls int
	cancelCalls  int
	repriceCalls int
}

func newFakeProvider(clock *fakeClock) *fakeProvider {
	return &fakeProvider{now: clock, priceMinor: 12345, currency: booking.CurrencyEUR}
}

func (f *fakeProvider) Search(context.Context, provider.SearchQuery) ([]booking.Offer, error) {
	return []booking.Offer{{
		SupplierID:    "sup-1",
		SupplierOffer: "offer-1",
		Total:         f.money(),
		AvailableAt:   f.now.nowFn(),
	}}, nil
}

func (f *fakeProvider) Hold(_ context.Context, _ booking.Offer, _ string) (booking.Hold, error) {
	f.holdCalls++

	return booking.Hold{
		SupplierID:  "sup-1",
		SupplierRef: "hold-ref-1",
		ExpiresAt:   f.now.nowFn().Add(30 * time.Minute),
		Total:       f.money(),
	}, nil
}

func (f *fakeProvider) Confirm(_ context.Context, hold booking.Hold, _ string) (booking.Confirmation, error) {
	f.confirmCalls++

	return booking.Confirmation{
		SupplierID:     hold.SupplierID,
		SupplierRef:    hold.SupplierRef,
		Total:          f.money(),
		ConfirmedAt:    f.now.nowFn(),
		TicketDocument: "TICKET-1",
	}, nil
}

func (f *fakeProvider) Cancel(context.Context, provider.SupplierRef, string) error {
	f.cancelCalls++

	return nil
}

func (f *fakeProvider) Reprice(context.Context, provider.SupplierRef) (booking.Money, error) {
	f.repriceCalls++

	return f.money(), nil
}

func (f *fakeProvider) money() booking.Money {
	return booking.Money{Amount: f.priceMinor, Currency: f.currency}
}

// bumpSupplierPrice raises the quoted price, simulating a fare change between
// the moment the user saw the amount and the moment of confirmation.
func (f *fakeProvider) bumpSupplierPrice() {
	f.priceMinor += 5000
}
