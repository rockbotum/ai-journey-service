// Package service orchestrates the booking lifecycle: it coordinates the
// domain, persistence and external suppliers.
//
// The rule this package exists to enforce is that a financial operation is
// never a side effect of a conversation. Confirmation and cancellation require
// an explicit user action, a fresh price check, and an idempotency key, and
// they are exposed as separate endpoints rather than as something a model can
// trigger.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rockbotum/ai-journey-service/internal/booking"
	"github.com/rockbotum/ai-journey-service/internal/provider"
	"github.com/rockbotum/ai-journey-service/internal/store"
)

// Errors surfaced to the transport layer. The HTTP layer maps these to status
// codes without inspecting messages.
var (
	ErrBookingNotFound   = errors.New("service: booking not found")
	ErrOfferNotAvailable = errors.New("service: offer is not available")
	ErrPriceReconfirm    = errors.New("service: price changed, explicit confirmation required")
	ErrConsentRequired   = errors.New("service: explicit user confirmation required")
	ErrInvalidState      = errors.New("service: operation is not allowed in the current state")
)

// Clock returns the current time. It is injected so expiry and price-recheck
// behaviour is deterministic in tests.
type Clock func() time.Time

// IDGenerator produces identifiers for new bookings.
type IDGenerator func() string

// idempotencyRecorder is an optional store capability used to bind a key to
// the outcome of a transition, so a replayed request returns the original
// result instead of contacting the supplier again.
type idempotencyRecorder interface {
	RecordIdempotency(key string, b *booking.Booking) error
}

// tracked couples a booking with the version the store currently holds.
//
// Every commit writes against that recorded version and then advances it, so
// a concurrent writer is detected as a conflict rather than silently
// overwriting a newer state. Deriving the expected version from the booking
// itself would be wrong, because the domain increments the version as part of
// applying a transition.
type tracked struct {
	b        *booking.Booking
	expected int64
}

// load reads a booking owned by userID and records its current version.
func (s *Service) load(ctx context.Context, bookingID, userID string) (*tracked, error) {
	b, err := s.Get(ctx, bookingID, userID)
	if err != nil {
		return nil, err
	}

	return &tracked{b: b, expected: b.Version}, nil
}

// commit applies a domain transition and persists it atomically from the
// caller's point of view. The transition is only written if the stored version
// is still the one this operation started from.
func (s *Service) commit(ctx context.Context, t *tracked, req booking.TransitionRequest) error {
	if err := t.b.Apply(req); err != nil {
		return err
	}

	if err := s.store.Save(ctx, t.b, t.expected); err != nil {
		return err
	}

	t.expected = t.b.Version

	return nil
}

// Service coordinates the booking process.
type Service struct {
	store    store.Store
	provider provider.Client
	log      *slog.Logger
	now      Clock
	newID    IDGenerator
}

// New builds a service. A nil logger is replaced by a discarding one so a
// misconfigured caller cannot cause a nil dereference mid-request.
func New(st store.Store, p provider.Client, log *slog.Logger, now Clock, newID IDGenerator) (*Service, error) {
	if st == nil {
		return nil, errors.New("service: store is required")
	}

	if p == nil {
		return nil, errors.New("service: provider is required")
	}

	if newID == nil {
		return nil, errors.New("service: id generator is required")
	}

	if log == nil {
		log = slog.New(slog.NewJSONHandler(discard{}, nil))
	}

	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	return &Service{store: st, provider: p, log: log, now: now, newID: newID}, nil
}

// discard swallows log output.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// CreateCommand starts a new booking from a validated request.
type CreateCommand struct {
	UserID         string
	Request        booking.Request
	IdempotencyKey string
	RequestID      string
}

// Create registers a user's intent. It contacts no supplier.
func (s *Service) Create(ctx context.Context, cmd CreateCommand) (*booking.Booking, error) {
	if cmd.IdempotencyKey == "" {
		return nil, errors.New("service: idempotency key is required")
	}

	if err := cmd.Request.ValidateAt(s.now()); err != nil {
		return nil, err
	}

	// The store owns idempotency: it compares a fingerprint of the request and
	// returns the booking created the first time for a genuine retry, while
	// refusing a key that is replayed with different data. Doing the lookup
	// here would bypass that check.
	b, err := booking.New(s.newID(), cmd.UserID, cmd.Request.Normalise(), s.now())
	if err != nil {
		return nil, err
	}

	created, err := s.store.Create(ctx, cmd.IdempotencyKey, b)
	if err != nil {
		return nil, err
	}

	s.log.InfoContext(ctx, "booking created",
		"booking_id", created.ID,
		"state", string(created.State),
		"origin", created.Request.Origin.Code,
		"destination", created.Request.Destination.Code,
		"travellers", created.Request.TravellerCount(),
		"request_id", cmd.RequestID,
	)

	return created, nil
}

// Get returns a booking owned by userID.
func (s *Service) Get(ctx context.Context, bookingID, userID string) (*booking.Booking, error) {
	b, err := s.store.Get(ctx, bookingID, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrBookingNotFound
		}

		return nil, err
	}

	return b, nil
}

// Search runs a supplier search and records the offers. A search is read-only:
// it consumes no availability and creates no obligation.
func (s *Service) Search(ctx context.Context, bookingID, userID string) ([]booking.Offer, error) {
	t, err := s.load(ctx, bookingID, userID)
	if err != nil {
		return nil, err
	}

	b := t.b

	if err := s.commit(ctx, t, booking.TransitionRequest{
		Trigger: booking.TriggerSearchRequested,
		Actor:   booking.ActorUser,
		At:      s.now(),
	}); err != nil {
		return nil, mapTransitionError(err)
	}

	offers, err := s.provider.Search(ctx, provider.SearchQuery{
		Origin:      b.Request.Origin,
		Destination: b.Request.Destination,
		Departure:   b.Request.Departure,
		Return:      b.Request.Return,
		Cabin:       b.Request.Cabin,
		Travellers:  b.Request.TravellerCount(),
		Currency:    b.Request.Currency,
	})
	if err != nil {
		s.fail(ctx, t, booking.TriggerSearchFailed, err)

		return nil, err
	}

	if len(offers) == 0 {
		s.fail(ctx, t, booking.TriggerSearchFailed, errNoOffers)

		return nil, errNoOffers
	}

	if err := s.commit(ctx, t, booking.TransitionRequest{
		Trigger: booking.TriggerSearchSucceeded,
		Actor:   booking.ActorSupplier,
		At:      s.now(),
		Offer:   &offers[0],
	}); err != nil {
		return nil, mapTransitionError(err)
	}

	return offers, nil
}

// HoldCommand reserves an offer temporarily.
type HoldCommand struct {
	BookingID      string
	UserID         string
	IdempotencyKey string
}

// Hold places a temporary reservation. It consumes availability, so it requires
// an idempotency key, but it creates no financial obligation: the user can
// still walk away until the hold lapses.
func (s *Service) Hold(ctx context.Context, cmd HoldCommand) (*booking.Booking, error) {
	if cmd.IdempotencyKey == "" {
		return nil, errors.New("service: idempotency key is required")
	}

	t, err := s.load(ctx, cmd.BookingID, cmd.UserID)
	if err != nil {
		return nil, err
	}

	b := t.b

	if b.Offer == nil {
		return nil, ErrOfferNotAvailable
	}

	if err := s.commit(ctx, t, booking.TransitionRequest{
		Trigger:        booking.TriggerHoldRequested,
		Actor:          booking.ActorUser,
		At:             s.now(),
		IdempotencyKey: cmd.IdempotencyKey,
	}); err != nil {
		return nil, mapTransitionError(err)
	}

	hold, err := s.provider.Hold(ctx, *b.Offer, cmd.IdempotencyKey)
	if err != nil {
		s.fail(ctx, t, booking.TriggerHoldFailed, err)

		return nil, err
	}

	if err := s.commit(ctx, t, booking.TransitionRequest{
		Trigger:        booking.TriggerHoldSucceeded,
		Actor:          booking.ActorSupplier,
		At:             s.now(),
		IdempotencyKey: cmd.IdempotencyKey,
		Hold:           &hold,
	}); err != nil {
		return nil, mapTransitionError(err)
	}

	return b, nil
}

// ConfirmCommand carries the explicit user decision. The zero value of
// UserConfirmed is the safe state: a caller that forgets to set it cannot
// confirm a booking.
type ConfirmCommand struct {
	BookingID      string
	UserID         string
	IdempotencyKey string
	// UserConfirmed must be set by the handler that received an explicit user
	// action. Nothing in the AI path may set it.
	UserConfirmed bool
	// AcceptedPrice is the amount the user agreed to. A mismatch with the
	// supplier's current price aborts the confirmation.
	AcceptedPrice *booking.Money
}

// Confirm converts a hold into a confirmed booking.
//
// The order of operations matters: the current price is re-read from the
// supplier first, because a booking must never be confirmed at a price the
// user did not agree to.
func (s *Service) Confirm(ctx context.Context, cmd ConfirmCommand) (*booking.Booking, error) {
	if !cmd.UserConfirmed {
		return nil, ErrConsentRequired
	}

	if cmd.IdempotencyKey == "" {
		return nil, errors.New("service: idempotency key is required")
	}

	// A replay returns the outcome of the original request rather than
	// contacting the supplier again.
	if existing, err := s.store.FindByIdempotencyKey(ctx, cmd.IdempotencyKey); err == nil && existing != nil {
		return existing, nil
	}

	t, err := s.load(ctx, cmd.BookingID, cmd.UserID)
	if err != nil {
		return nil, err
	}

	b := t.b

	if b.Hold == nil {
		return nil, booking.ErrHoldExpired
	}

	if err := s.recheckPrice(ctx, b, cmd.AcceptedPrice); err != nil {
		return nil, err
	}

	if err := s.commit(ctx, t, booking.TransitionRequest{
		Trigger:        booking.TriggerConfirmRequested,
		Actor:          booking.ActorUser,
		At:             s.now(),
		IdempotencyKey: cmd.IdempotencyKey,
		UserConfirmed:  true,
	}); err != nil {
		return nil, mapTransitionError(err)
	}

	confirmation, err := s.provider.Confirm(ctx, *b.Hold, cmd.IdempotencyKey)
	if err != nil {
		s.fail(ctx, t, booking.TriggerConfirmFailed, err)

		return nil, err
	}

	if err := s.commit(ctx, t, booking.TransitionRequest{
		Trigger:        booking.TriggerConfirmSucceeded,
		Actor:          booking.ActorSupplier,
		At:             s.now(),
		IdempotencyKey: cmd.IdempotencyKey,
		Confirmation:   &confirmation,
	}); err != nil {
		return nil, mapTransitionError(err)
	}

	// Bind the key to this booking so a retried confirmation returns the
	// original result instead of attempting a second booking at the supplier.
	// The capability is optional: a store that cannot record it still works and
	// simply loses replay protection for this operation.
	if recorder, ok := s.store.(idempotencyRecorder); ok {
		if err := recorder.RecordIdempotency(cmd.IdempotencyKey, b); err != nil {
			s.log.WarnContext(ctx, "could not record the confirmation idempotency key",
				"booking_id", b.ID, "err", err)
		}
	}

	s.log.InfoContext(ctx, "booking confirmed",
		"booking_id", b.ID,
		"supplier_id", confirmation.SupplierID,
		"amount_minor", confirmation.Total.Amount,
		"currency", string(confirmation.Total.Currency),
	)

	return b, nil
}

// recheckPrice aborts the confirmation when the supplier's current price no
// longer matches what the user agreed to pay.
func (s *Service) recheckPrice(ctx context.Context, b *booking.Booking, accepted *booking.Money) error {
	if b.Hold == nil {
		return booking.ErrHoldExpired
	}

	current, err := s.provider.Reprice(ctx, provider.SupplierRef{
		SupplierID:  b.Hold.SupplierID,
		SupplierRef: b.Hold.SupplierRef,
	})
	if err != nil {
		// A price that cannot be verified is not a price that can be charged.
		return fmt.Errorf("%w: current price is unavailable", ErrPriceReconfirm)
	}

	ok, err := b.ConfirmPriceMatches(current)
	if err != nil || !ok {
		return fmt.Errorf("%w: held %s, current %s", ErrPriceReconfirm, b.Hold.Total, current)
	}

	if accepted != nil && *accepted != current {
		return fmt.Errorf("%w: user accepted %s, current %s", ErrPriceReconfirm, *accepted, current)
	}

	return nil
}

// CancelCommand carries the explicit user decision to cancel.
type CancelCommand struct {
	BookingID      string
	UserID         string
	IdempotencyKey string
	UserConfirmed  bool
}

// Cancel releases a hold or cancels a confirmed booking.
func (s *Service) Cancel(ctx context.Context, cmd CancelCommand) (*booking.Booking, error) {
	if !cmd.UserConfirmed {
		return nil, ErrConsentRequired
	}

	if cmd.IdempotencyKey == "" {
		return nil, errors.New("service: idempotency key is required")
	}

	t, err := s.load(ctx, cmd.BookingID, cmd.UserID)
	if err != nil {
		return nil, err
	}

	b := t.b

	// A supplier cancellation needs a reference from either a hold or a
	// confirmation. A booking in an earlier state has neither, and asking the
	// supplier to cancel an empty reference would be a meaningless call.
	var ref provider.SupplierRef

	switch {
	case b.Confirm != nil:
		ref = provider.SupplierRef{SupplierID: b.Confirm.SupplierID, SupplierRef: b.Confirm.SupplierRef}
	case b.Hold != nil:
		ref = provider.SupplierRef{SupplierID: b.Hold.SupplierID, SupplierRef: b.Hold.SupplierRef}
	default:
		return nil, ErrInvalidState
	}

	if err := s.commit(ctx, t, booking.TransitionRequest{
		Trigger:        booking.TriggerCancelRequested,
		Actor:          booking.ActorUser,
		At:             s.now(),
		IdempotencyKey: cmd.IdempotencyKey,
		UserConfirmed:  true,
	}); err != nil {
		return nil, mapTransitionError(err)
	}

	if err := s.provider.Cancel(ctx, ref, cmd.IdempotencyKey); err != nil {
		s.fail(ctx, t, booking.TriggerCancelFailed, err)

		return nil, err
	}

	if err := s.commit(ctx, t, booking.TransitionRequest{
		Trigger:        booking.TriggerCancelSucceeded,
		Actor:          booking.ActorSupplier,
		At:             s.now(),
		IdempotencyKey: cmd.IdempotencyKey,
	}); err != nil {
		return nil, mapTransitionError(err)
	}

	return b, nil
}

// ExpireHolds marks a hold that lapsed before confirmation, so a lapsed
// reservation is never presented as still confirmable.
func (s *Service) ExpireHolds(ctx context.Context, bookingID, userID string) error {
	t, err := s.load(ctx, bookingID, userID)
	if err != nil {
		return err
	}

	if t.b.IsHoldLive(s.now()) {
		return nil
	}

	if err := s.commit(ctx, t, booking.TransitionRequest{
		Trigger: booking.TriggerHoldExpired,
		Actor:   booking.ActorSystem,
		At:      s.now(),
	}); err != nil {
		return mapTransitionError(err)
	}

	return nil
}

// fail records a supplier failure on the booking and persists it. The failure
// reason is a short code: a raw provider body is never stored.
func (s *Service) fail(ctx context.Context, t *tracked, trigger booking.Trigger, cause error) {
	s.log.WarnContext(ctx, "booking step failed",
		"booking_id", t.b.ID,
		"trigger", string(trigger),
		"reason", failureCode(cause),
	)

	if err := s.commit(ctx, t, booking.TransitionRequest{
		Trigger:       trigger,
		Actor:         booking.ActorSystem,
		At:            s.now(),
		FailureReason: failureCode(cause),
	}); err != nil {
		s.log.WarnContext(ctx, "could not record the failure state",
			"booking_id", t.b.ID, "trigger", string(trigger), "reason", failureCode(err))
	}
}

// failureCode maps an error to a short, non-sensitive code suitable for
// storage and for a log line.
func failureCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, provider.ErrUnavailable):
		return "supplier_unavailable"
	case errors.Is(err, provider.ErrRateLimited):
		return "supplier_rate_limited"
	case errors.Is(err, provider.ErrRejected):
		return "supplier_rejected"
	case errors.Is(err, provider.ErrPriceUnavailable):
		return "option_unavailable"
	case errors.Is(err, provider.ErrMalformedResponse):
		return "supplier_contract_violation"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "deadline_exceeded"
	default:
		return "internal_error"
	}
}

// mapTransitionError converts a domain transition failure into a service error
// so the transport layer does not need to import the domain package.
func mapTransitionError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, booking.ErrHoldExpired):
		return booking.ErrHoldExpired
	case errors.Is(err, booking.ErrPriceChanged):
		return ErrPriceReconfirm
	case errors.Is(err, booking.ErrAvailabilityLost):
		return ErrOfferNotAvailable
	case errors.Is(err, booking.ErrUserConfirmationRequired):
		return ErrConsentRequired
	default:
		var invalid *booking.ErrInvalidTransition
		if errors.As(err, &invalid) {
			return fmt.Errorf("%w: %s", ErrInvalidState, invalid.Trigger)
		}

		return err
	}
}

// errNoOffers is returned when a supplier reports availability but returns no
// candidates. It is distinct from a supplier failure: nothing went wrong, the
// route simply has no options.
var errNoOffers = errors.New("service: no offers available")
