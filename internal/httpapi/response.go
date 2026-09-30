package httpapi

import (
	"time"

	"github.com/rockbotum/ai-journey-service/internal/booking"
)

// bookingResponse is the only booking shape that leaves the process.
//
// AGENTS.md requires that a response contains exclusively permitted fields.
// The domain struct is never serialised directly: it carries internal
// bookkeeping (failure reasons, supplier internal names, idempotency keys) that
// a client has no business seeing. Adding a field here is a deliberate,
// reviewable act.
type bookingResponse struct {
	ID      string              `json:"id"`
	State   string              `json:"state"`
	Request bookingRequestReply `json:"request"`
	Offer   *offerReply         `json:"offer,omitempty"`
	Hold    *holdReply          `json:"hold,omitempty"`
	// Confirmation carries the confirmed total, the confirmation time and the
	// ticket reference once the supplier has confirmed. Its presence is the
	// only reliable signal that a booking is real: a price or a hold is not.
	Confirmation *confirmationReply `json:"confirmation,omitempty"`
	// RequiresUserConsent tells the client exactly when it must collect an
	// explicit decision from a person, so a UI renders a prompt instead of
	// guessing from the state name.
	RequiresUserConsent bool `json:"requires_user_consent"`
	// Preliminary marks a booking that is not yet binding.
	Preliminary bool `json:"preliminary"`
	// RecheckPricesAt is the supplier's availability deadline. A nil value
	// means no deadline applies. Surfacing it is what lets a client show an
	// honest "valid until" instead of implying guaranteed availability.
	RecheckPricesAt string `json:"recheck_prices_at,omitempty"`
	// History is the ordered audit trail of state changes.
	History []historyReply `json:"history"`
	// Payment is always present, even though no payment provider exists yet.
	// AGENTS.md treats payment as a distinct step of the journey, and a client
	// has to be able to tell "payment is impossible here" apart from "payment
	// happened and the field was forgotten". An omitted field is ambiguous in
	// exactly the case that matters, so the non-support is stated outright
	// rather than modelled as something half-working.
	Payment *paymentReply `json:"payment"`
}

// paymentNotSupported is the only status this increment can report. It is a
// constant rather than a literal at the call site so the value stays tied to
// the field name in ADR 0003, and so a future payment provider has to make a
// deliberate choice about what to replace.
const paymentNotSupported = "not_supported"

// paymentReply reports payment capability. Supported is false in this increment
// and no amount or reference is reported, because confirming with the supplier
// creates no financial obligation: money moves only on an explicit payment step
// that does not exist yet. See docs/adr/0003-payment-gap.md.
type paymentReply struct {
	Status    string `json:"status"`
	Supported bool   `json:"supported"`
}

type bookingRequestReply struct {
	Origin      string         `json:"origin"`
	Destination string         `json:"destination"`
	Departure   string         `json:"departure"`
	Return      string         `json:"return,omitempty"`
	Cabin       string         `json:"cabin"`
	Currency    string         `json:"currency"`
	Passengers  passengerReply `json:"passengers"`
}

// passengerReply carries a count, not names. A list of personal names has no
// business in a status response: a client that needs them already holds the
// request it sent itself.
type passengerReply struct {
	Count int `json:"count"`
}

type offerReply struct {
	SupplierID    string     `json:"supplier_id"`
	SupplierOffer string     `json:"supplier_offer_id"`
	Total         moneyReply `json:"total"`
	// AvailableAt is a snapshot, not a guarantee.
	AvailableAt string `json:"available_at"`
	// CancellationSummary is free supplier text. AGENTS.md permits showing it
	// to the user as-is, and forbids parsing it for any decision. The client
	// must render it as text, never as markup.
	CancellationSummary string `json:"cancellation_summary,omitempty"`
}

type holdReply struct {
	SupplierID  string     `json:"supplier_id"`
	SupplierRef string     `json:"supplier_ref"`
	Total       moneyReply `json:"total"`
	ExpiresAt   string     `json:"expires_at"`
}

type confirmationReply struct {
	SupplierID     string     `json:"supplier_id"`
	SupplierRef    string     `json:"supplier_ref"`
	Total          moneyReply `json:"total"`
	ConfirmedAt    string     `json:"confirmed_at"`
	TicketDocument string     `json:"ticket_document,omitempty"`
}

// moneyReply renders an amount in minor units with the currency alongside, so a
// client never has to guess the exponent.
type moneyReply struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	// IsFinal is false while the amount is an estimate. Only a confirmation
	// total is final.
	IsFinal bool `json:"is_final"`
}

type historyReply struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Trigger   string `json:"trigger"`
	Actor     string `json:"actor"`
	At        string `json:"at"`
	RequestID string `json:"request_id,omitempty"`
	// IdempotencyKey is included deliberately: a client reconciling a retried
	// request needs to prove the retry was the same operation.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

const (
	dateLayout = "2006-01-02"
	timeLayout = time.RFC3339
)

// toBookingResponse maps the domain aggregate to the public shape.
func toBookingResponse(b *booking.Booking) bookingResponse {
	resp := bookingResponse{
		ID:                  b.ID,
		State:               string(b.State),
		RequiresUserConsent: b.RequiresUserConfirmation(),
		Preliminary:         b.State.Preliminary(),
		Request: bookingRequestReply{
			Origin:      b.Request.Origin.Code,
			Destination: b.Request.Destination.Code,
			Departure:   b.Request.Departure.Format(dateLayout),
			Cabin:       string(b.Request.Cabin),
			Currency:    string(b.Request.Currency),
			Passengers:  passengerReply{Count: b.Request.TravellerCount()},
		},
		History: make([]historyReply, 0, len(b.History)),
		// Set for every state, terminal or not. A client reading only a
		// confirmed booking still needs to learn that nothing was charged.
		Payment: &paymentReply{Status: paymentNotSupported, Supported: false},
	}

	if b.Request.Return != nil {
		resp.Request.Return = b.Request.Return.Format(dateLayout)
	}

	if b.Offer != nil {
		resp.Offer = &offerReply{
			SupplierID:          b.Offer.SupplierID,
			SupplierOffer:       b.Offer.SupplierOffer,
			Total:               toMoneyReply(b.Offer.Total, false),
			AvailableAt:         b.Offer.AvailableAt.UTC().Format(timeLayout),
			CancellationSummary: b.Offer.CancellationSummary,
		}
	}

	if b.Hold != nil {
		resp.Hold = &holdReply{
			SupplierID:  b.Hold.SupplierID,
			SupplierRef: b.Hold.SupplierRef,
			Total:       toMoneyReply(b.Hold.Total, false),
			ExpiresAt:   b.Hold.ExpiresAt.UTC().Format(timeLayout),
		}

		resp.RecheckPricesAt = b.Hold.ExpiresAt.UTC().Format(timeLayout)
	}

	if b.Confirm != nil {
		resp.Confirmation = &confirmationReply{
			SupplierID:     b.Confirm.SupplierID,
			SupplierRef:    b.Confirm.SupplierRef,
			Total:          toMoneyReply(b.Confirm.Total, true),
			ConfirmedAt:    b.Confirm.ConfirmedAt.UTC().Format(timeLayout),
			TicketDocument: b.Confirm.TicketDocument,
		}
	}

	for _, entry := range b.History {
		resp.History = append(resp.History, historyReply{
			From:           string(entry.From),
			To:             string(entry.To),
			Trigger:        string(entry.Trigger),
			Actor:          string(entry.Actor),
			At:             entry.At.UTC().Format(timeLayout),
			RequestID:      entry.RequestID,
			IdempotencyKey: entry.IdempotencyKey,
		})
	}

	return resp
}

func toMoneyReply(m booking.Money, final bool) moneyReply {
	return moneyReply{
		Amount:   m.Amount,
		Currency: string(m.Currency),
		IsFinal:  final,
	}
}

// offersResponse lists search candidates. A search creates no obligation, so
// every entry is marked preliminary.
type offersResponse struct {
	BookingID string       `json:"booking_id"`
	State     string       `json:"state"`
	Offers    []offerReply `json:"offers"`
	// Preliminary is always true for a search result: nothing is reserved.
	Preliminary bool `json:"preliminary"`
	// RecheckPricesAt is the deadline after which the prices must be re-read.
	RecheckPricesAt string `json:"recheck_prices_at,omitempty"`
}

func toOffersResponse(b *booking.Booking, offers []booking.Offer) offersResponse {
	items := make([]offerReply, 0, len(offers))

	for _, o := range offers {
		items = append(items, offerReply{
			SupplierID:          o.SupplierID,
			SupplierOffer:       o.SupplierOffer,
			Total:               toMoneyReply(o.Total, false),
			AvailableAt:         o.AvailableAt.UTC().Format(timeLayout),
			CancellationSummary: o.CancellationSummary,
		})
	}

	resp := offersResponse{
		BookingID:   b.ID,
		State:       string(b.State),
		Offers:      items,
		Preliminary: true,
	}

	// A search result is valid only until the supplier's snapshot ages.
	if b.Offer != nil && !b.Offer.AvailableAt.IsZero() {
		resp.RecheckPricesAt = b.Offer.AvailableAt.UTC().Format(timeLayout)
	}

	return resp
}
