package booking

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors for domain rule violations. Callers classify on these with
// errors.Is; the transport layer maps them to status codes without inspecting
// error strings.
var (
	// ErrHoldExpired means the temporary hold lapsed before confirmation.
	ErrHoldExpired = errors.New("booking: hold expired")
	// ErrPriceChanged means the supplier price moved between the moment the
	// user saw it and the moment of confirmation, so a new explicit
	// confirmation is required.
	ErrPriceChanged = errors.New("booking: price changed, reconfirmation required")
	// ErrAvailabilityLost means the option is no longer bookable.
	ErrAvailabilityLost = errors.New("booking: option no longer available")
	// ErrUserConfirmationRequired means a financial transition was attempted
	// without an explicit confirmation from the user.
	ErrUserConfirmationRequired = errors.New("booking: explicit user confirmation required")
	// ErrSupplierRefMissing means an external operation is required but no
	// supplier reference is stored.
	ErrSupplierRefMissing = errors.New("booking: supplier reference missing")
	// ErrSupplierMismatch means the external response belongs to a different
	// supplier or offer than the one the booking holds.
	ErrSupplierMismatch = errors.New("booking: supplier response does not match booking")
	// ErrEmptyTransition means no transition was requested.
	ErrEmptyTransition = errors.New("booking: empty transition")
)

// Actor records who initiated a state change. It is required for auditing
// financial transitions and must never be a free-form string from a model.
type Actor string

const (
	ActorUser     Actor = "user"
	ActorSystem   Actor = "system"
	ActorSupplier Actor = "supplier"
)

// HistoryEntry is one audited state change.
type HistoryEntry struct {
	From      State     `json:"from"`
	To        State     `json:"to"`
	Trigger   Trigger   `json:"trigger"`
	Actor     Actor     `json:"actor"`
	At        time.Time `json:"at"`
	RequestID string    `json:"request_id,omitempty"`
	// IdempotencyKey ties the change to the client request that caused it, so
	// a retried request is provably the same operation.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// Offer is a candidate returned by a supplier. It is data, not a reservation.
type Offer struct {
	SupplierID    string `json:"supplier_id"`
	SupplierOffer string `json:"supplier_offer_id"`
	Total         Money  `json:"total"`
	// AvailableAt records when the supplier last confirmed availability. The
	// UI must present it as a snapshot, never as a guarantee.
	AvailableAt time.Time `json:"available_at"`
	// CancellationSummary is free supplier text shown to the user verbatim.
	// It is never parsed for decisions.
	CancellationSummary string `json:"cancellation_summary,omitempty"`
}

// Hold is a temporary supplier-side reservation that consumes availability but
// creates no financial obligation.
type Hold struct {
	SupplierRef  string    `json:"supplier_ref"`
	ExpiresAt    time.Time `json:"expires_at"`
	Total        Money     `json:"total"`
	SupplierID   string    `json:"supplier_id"`
	SupplierName string    `json:"supplier_offer_id"`
}

// Confirmation is the result of a confirmed booking.
type Confirmation struct {
	SupplierID     string    `json:"supplier_id"`
	SupplierRef    string    `json:"supplier_ref"`
	Total          Money     `json:"total"`
	ConfirmedAt    time.Time `json:"confirmed_at"`
	TicketDocument string    `json:"ticket_document,omitempty"`
}

// TransitionRequest carries everything needed to apply one state change.
type TransitionRequest struct {
	Trigger Trigger
	Actor   Actor
	At      time.Time
	// IdempotencyKey is the caller's dedup key. It is mandatory for financial
	// transitions and stored in history for audit.
	IdempotencyKey string
	RequestID      string

	// UserConfirmed must be true for financial transitions. It may only be set
	// by a code path that received an explicit user action, never by parsing
	// model output.
	UserConfirmed bool

	// SupplierID and SupplierRef must match the stored hold or offer; a
	// mismatch is rejected instead of being written through.
	SupplierID  string
	SupplierRef string

	// Offer updates the current offer on a successful search.
	Offer *Offer
	// Hold is set on a successful hold.
	Hold *Hold
	// Confirmation is set on a successful confirmation.
	Confirmation *Confirmation
	// FailureReason is a short, non-sensitive code, never a raw provider body.
	FailureReason string
}

// Booking is the aggregate root. All mutation happens through Apply, which
// validates the transition against the lifecycle graph and the booking
// invariants. Fields are exported for storage serialisation but must not be
// written directly by callers outside this package's constructors.
type Booking struct {
	ID     string
	UserID string

	State   State
	Version int64

	Request Request
	Offer   *Offer
	Hold    *Hold
	Confirm *Confirmation
	Failure string
	History []HistoryEntry
	Created time.Time
	Updated time.Time
	// CancelRequestedAt is set when cancellation starts, for audit of who
	// initiated it and when.
	CancelRequestedAt *time.Time
}

// New starts a booking in StateIntent. The user's intent exists, nothing has
// been requested from a supplier.
func New(id, userID string, req Request, now time.Time) (*Booking, error) {
	if id == "" {
		return nil, errors.New("booking: id is required")
	}

	if userID == "" {
		return nil, errors.New("booking: user id is required")
	}

	if err := req.Validate(); err != nil {
		return nil, err
	}

	now = normaliseTime(now)

	return &Booking{
		ID:      id,
		UserID:  userID,
		State:   StateIntent,
		Version: 1,
		Request: req,
		Created: now,
		Updated: now,
		// History starts empty on purpose. A booking is born in its initial
		// state, so creation is not a transition and must not be recorded as
		// one. Recording a search here would claim a supplier call that never
		// happened, which is exactly the kind of false record the audit trail
		// exists to prevent.
		History: []HistoryEntry{},
	}, nil
}

// Apply performs one lifecycle transition with full invariant checks.
func (b *Booking) Apply(req TransitionRequest) error {
	if req.Trigger == "" {
		return ErrEmptyTransition
	}

	if req.Actor == "" {
		return errors.New("booking: actor is required")
	}

	now := normaliseTime(req.At)

	to, edge, err := next(b.State, req.Trigger)
	if err != nil {
		return err
	}

	if edge.requiresUserConsent {
		if err := checkUserConsent(req); err != nil {
			return err
		}
	}

	// A hold and a price check are required at the moment the user asks to
	// confirm, not when the supplier answers: availability can lapse between
	// the two calls.
	if edge.financial {
		if err := b.checkFinancialPreconditions(req, edge, now); err != nil {
			return err
		}
	}

	if err := b.checkSupplierBinding(req); err != nil {
		return err
	}

	from := b.State

	b.mutate(req, to, now)
	b.State = to
	b.Version++
	b.Updated = now
	b.History = append(b.History, HistoryEntry{
		From:           from,
		To:             to,
		Trigger:        req.Trigger,
		Actor:          req.Actor,
		At:             now,
		RequestID:      req.RequestID,
		IdempotencyKey: req.IdempotencyKey,
	})

	return nil
}

func (b *Booking) mutate(req TransitionRequest, to State, now time.Time) {
	switch to {
	case StateSearching:
		b.Offer = nil
		b.Hold = nil
		b.Failure = ""
	case StateOffered:
		if req.Offer != nil {
			b.Offer = req.Offer
		}

		b.Failure = ""
	case StateHolding:
		b.Failure = ""
	case StateHeld:
		if req.Hold != nil {
			b.Hold = req.Hold
		}

		b.Failure = ""
	case StateConfirming:
		b.Failure = ""
	case StateConfirmed:
		if req.Confirmation != nil {
			b.Confirm = req.Confirmation
		}

		b.Failure = ""
	case StateCancelling:
		cancelAt := now
		b.CancelRequestedAt = &cancelAt
		b.Failure = ""
	case StateFailed:
		b.Failure = sanitiseReason(req.FailureReason)
	case StateExpired:
		// The hold no longer reserves anything upstream; drop it so a stale
		// hold cannot be reused for a later confirmation.
		b.Hold = nil
	}
}

// checkUserConsent enforces that an operation able to spend money, consume
// availability or create an obligation was explicitly approved by the user.
// Only a user actor can supply this consent, so no system, supplier or model
// driven path can reach a financial edge.
func checkUserConsent(req TransitionRequest) error {
	if req.Actor != ActorUser || !req.UserConfirmed {
		return ErrUserConfirmationRequired
	}

	if req.IdempotencyKey == "" {
		return errors.New("booking: idempotency key is required for financial transitions")
	}

	return nil
}

// checkFinancialPreconditions enforces that a financial edge is still backed by
// live supplier state: a hold that has not lapsed and a stored offer to confirm
// against.
func (b *Booking) checkFinancialPreconditions(req TransitionRequest, edge transition, now time.Time) error {
	// Confirmation must be backed by a live hold: this is what makes a price
	// re-check possible and prevents confirming an expired reservation.
	if req.Trigger == TriggerConfirmRequested {
		if b.Hold == nil {
			return ErrHoldExpired
		}

		if !b.Hold.ExpiresAt.After(now) {
			return ErrHoldExpired
		}

		if b.Offer == nil {
			return ErrAvailabilityLost
		}
	}

	// Cancelling a confirmed booking is financial too, but there is no hold to
	// check: the confirmation reference is the anchor.
	if req.Trigger == TriggerCancelRequested && b.State == StateConfirmed {
		if b.Confirm == nil || b.Confirm.SupplierRef == "" {
			return ErrSupplierRefMissing
		}
	}

	// A settlement edge must still be answered by the same supplier it was
	// requested from.
	if edge.to == StateConfirmed || edge.to == StateCancelled {
		if b.Hold == nil && b.Confirm == nil {
			return ErrSupplierRefMissing
		}
	}

	return nil
}

// checkSupplierBinding rejects an external response that does not belong to
// the booking it is being applied to. Without this check a delayed or
// misrouted supplier response could confirm the wrong offer.
func (b *Booking) checkSupplierBinding(req TransitionRequest) error {
	if req.SupplierID != "" && b.Offer != nil && req.SupplierID != b.Offer.SupplierID {
		return fmt.Errorf("%w: supplier %q, booking holds %q", ErrSupplierMismatch, req.SupplierID, b.Offer.SupplierID)
	}

	if req.Hold != nil && b.Offer != nil {
		if req.Hold.SupplierID != b.Offer.SupplierID {
			return fmt.Errorf("%w: hold from %q, offer from %q", ErrSupplierMismatch, req.Hold.SupplierID, b.Offer.SupplierID)
		}

		if req.Hold.SupplierRef == "" {
			return ErrSupplierRefMissing
		}
	}

	if req.Confirmation != nil {
		if req.Confirmation.SupplierRef == "" {
			return ErrSupplierRefMissing
		}

		if b.Hold != nil && req.Confirmation.SupplierID != b.Hold.SupplierID {
			return fmt.Errorf("%w: confirmation from %q, hold from %q", ErrSupplierMismatch, req.Confirmation.SupplierID, b.Hold.SupplierID)
		}
	}

	return nil
}

// ConfirmPriceMatches reports whether the price the supplier now quotes equals
// the price the user agreed to. A mismatch must block confirmation and force a
// fresh, explicit user decision; the service layer calls this before Apply.
func (b *Booking) ConfirmPriceMatches(current Money) (bool, error) {
	if b.Offer == nil || b.Hold == nil {
		return false, ErrAvailabilityLost
	}

	if !b.Hold.Total.SameCurrency(current) {
		return false, ErrPriceChanged
	}

	return b.Hold.Total == current, nil
}

// IsHoldLive reports whether the hold can still be confirmed.
func (b *Booking) IsHoldLive(now time.Time) bool {
	return b.Hold != nil && b.Hold.ExpiresAt.After(normaliseTime(now))
}

// RequiresUserConfirmation reports whether the booking is currently sitting on
// a financial transition. The API uses it to require an explicit confirm
// parameter instead of assuming consent.
func (b *Booking) RequiresUserConfirmation() bool {
	return isFinancial(b.State, TriggerConfirmRequested) || isFinancial(b.State, TriggerCancelRequested)
}

func normaliseTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}

	return t.UTC()
}

// sanitiseReason keeps failure reasons short and free of control characters.
// Raw provider bodies and model text must never reach this field verbatim.
func sanitiseReason(reason string) string {
	const maxLen = 200

	out := make([]rune, 0, len(reason))
	for _, r := range reason {
		if r < 0x20 || r == 0x7f {
			continue
		}

		out = append(out, r)
		if len(out) == maxLen {
			break
		}
	}

	return string(out)
}
