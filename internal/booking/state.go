package booking

import (
	"fmt"
	"strings"
)

// State is an explicit lifecycle state of a booking. AGENTS.md requires the
// booking to be modelled as a process with states rather than a single record
// mutation, so every state change goes through a declared transition.
type State string

const (
	// StateIntent means the user's intent is captured but nothing has been
	// requested from a supplier yet.
	StateIntent State = "intent"
	// StateSearching means a search request is in flight.
	StateSearching State = "searching"
	// StateOffered means the supplier returned candidate options. An offer is
	// not a reservation: nothing is held and no availability is consumed.
	StateOffered State = "offered"
	// StateHolding means a temporary hold is being requested from a supplier.
	StateHolding State = "holding"
	// StateHeld means a temporary hold exists and is still valid. The user can
	// still walk away at no cost.
	StateHeld State = "held"
	// StateConfirming means a booking confirmation is in flight. This is the
	// only transition that can create a financial obligation.
	StateConfirming State = "confirming"
	// StateConfirmed means the supplier confirmed the booking.
	StateConfirmed State = "confirmed"
	// StateCancelling means a cancellation is in flight.
	StateCancelling State = "cancelling"
	// StateCancelled means the booking is cancelled.
	StateCancelled State = "cancelled"
	// StateFailed means the booking attempt failed and is not retryable
	// without user action.
	StateFailed State = "failed"
	// StateExpired means a hold was not confirmed in time and lapsed.
	StateExpired State = "expired"
)

// Trigger is a domain event that may cause a transition. Triggers are named
// after business facts, not after HTTP verbs, so the same vocabulary is reused
// by any transport.
type Trigger string

const (
	TriggerSearchRequested  Trigger = "search_requested"
	TriggerSearchSucceeded  Trigger = "search_succeeded"
	TriggerSearchFailed     Trigger = "search_failed"
	TriggerHoldRequested    Trigger = "hold_requested"
	TriggerHoldSucceeded    Trigger = "hold_succeeded"
	TriggerHoldFailed       Trigger = "hold_failed"
	TriggerConfirmRequested Trigger = "confirm_requested"
	TriggerConfirmSucceeded Trigger = "confirm_succeeded"
	TriggerConfirmFailed    Trigger = "confirm_failed"
	TriggerCancelRequested  Trigger = "cancel_requested"
	TriggerCancelSucceeded  Trigger = "cancel_succeeded"
	TriggerCancelFailed     Trigger = "cancel_failed"
	TriggerHoldExpired      Trigger = "hold_expired"
)

// transition is one allowed edge of the lifecycle graph.
type transition struct {
	from    State
	trigger Trigger
	to      State

	// financial marks transitions that can consume availability, create a
	// financial obligation or move money.
	financial bool

	// requiresUserConsent marks the edge that *requests* such an operation.
	// Only the request needs an explicit user action; the supplier callback
	// that settles an already-authorised request does not, because by then
	// the user has confirmed in the requesting state.
	requiresUserConsent bool
}

// lifecycle is the complete set of allowed transitions. Anything not listed
// here is rejected: unknown triggers and undeclared edges are programming or
// attack errors, not recoverable situations.
var lifecycle = []transition{
	{from: StateIntent, trigger: TriggerSearchRequested, to: StateSearching},
	{from: StateSearching, trigger: TriggerSearchSucceeded, to: StateOffered},
	{from: StateSearching, trigger: TriggerSearchFailed, to: StateFailed},

	{from: StateOffered, trigger: TriggerHoldRequested, to: StateHolding},
	{from: StateOffered, trigger: TriggerSearchRequested, to: StateSearching},
	{from: StateOffered, trigger: TriggerCancelRequested, to: StateCancelling},

	{from: StateHolding, trigger: TriggerHoldSucceeded, to: StateHeld},
	{from: StateHolding, trigger: TriggerHoldFailed, to: StateFailed},
	{from: StateHolding, trigger: TriggerHoldExpired, to: StateExpired},

	{from: StateHeld, trigger: TriggerConfirmRequested, to: StateConfirming, financial: true, requiresUserConsent: true},
	{from: StateHeld, trigger: TriggerHoldExpired, to: StateExpired},
	{from: StateHeld, trigger: TriggerCancelRequested, to: StateCancelling},
	// A user may abandon a hold and search again. The hold is dropped rather
	// than reused, so a lapsed reservation can never be turned into a booking.
	{from: StateHeld, trigger: TriggerSearchRequested, to: StateSearching},

	{from: StateConfirming, trigger: TriggerConfirmSucceeded, to: StateConfirmed, financial: true},
	{from: StateConfirming, trigger: TriggerConfirmFailed, to: StateFailed},

	{from: StateConfirmed, trigger: TriggerCancelRequested, to: StateCancelling, financial: true, requiresUserConsent: true},

	{from: StateCancelling, trigger: TriggerCancelSucceeded, to: StateCancelled, financial: true},
	{from: StateCancelling, trigger: TriggerCancelFailed, to: StateFailed},
}

// AllowedTransitions reports the triggers accepted in the given state, in
// declaration order. It is used by API responses so a client can render only
// the actions that are actually possible.
func AllowedTransitions(s State) []Trigger {
	out := make([]Trigger, 0, 4)
	for _, t := range lifecycle {
		if t.from == s {
			out = append(out, t.trigger)
		}
	}

	return out
}

// IsTerminal reports whether no transition leaves the state.
func IsTerminal(s State) bool {
	return len(AllowedTransitions(s)) == 0
}

// Preliminary reports whether a booking in this state is not yet binding.
//
// A preliminary booking creates no financial obligation and reserves nothing.
// Only a confirmed booking is binding, and a cancelled or failed or expired one
// is final but not binding. The API surfaces this so a client can label an
// offer honestly instead of implying that it is a purchase.
func (s State) Preliminary() bool {
	switch s {
	case StateIntent, StateSearching, StateOffered, StateHolding, StateHeld, StateConfirming, StateCancelling:
		return true
	default:
		return false
	}
}

// ErrInvalidTransition is returned when a trigger is not declared for the
// current state.
type ErrInvalidTransition struct {
	From    State
	Trigger Trigger
	Reason  string
}

func (e *ErrInvalidTransition) Error() string {
	msg := fmt.Sprintf("booking: transition %s is not allowed from state %s", e.Trigger, e.From)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}

	return msg
}

// next resolves the target state for a trigger, or reports why it cannot.
func next(from State, trigger Trigger) (State, transition, error) {
	for _, t := range lifecycle {
		if t.from == from && t.trigger == trigger {
			return t.to, t, nil
		}
	}

	if !isKnownState(from) {
		return "", transition{}, fmt.Errorf("booking: unknown state %q", from)
	}

	if !isKnownTrigger(trigger) {
		return "", transition{}, fmt.Errorf("booking: unknown trigger %q", trigger)
	}

	return "", transition{}, &ErrInvalidTransition{From: from, Trigger: trigger}
}

// isFinancial reports whether a trigger is financial in the given state.
func isFinancial(from State, trigger Trigger) bool {
	for _, t := range lifecycle {
		if t.from == from && t.trigger == trigger {
			return t.financial
		}
	}

	return false
}

func isKnownState(s State) bool {
	switch s {
	case StateIntent, StateSearching, StateOffered, StateHolding, StateHeld,
		StateConfirming, StateConfirmed, StateCancelling, StateCancelled,
		StateFailed, StateExpired:
		return true
	}

	return false
}

func isKnownTrigger(t Trigger) bool {
	for _, tr := range lifecycle {
		if tr.trigger == t {
			return true
		}
	}

	return false
}

// ParseState validates a state coming from storage or a wire format.
func ParseState(raw string) (State, error) {
	s := State(strings.ToLower(strings.TrimSpace(raw)))
	if !isKnownState(s) {
		return "", fmt.Errorf("booking: unknown state %q", raw)
	}

	return s, nil
}

// ParseTrigger validates a trigger coming from storage or a wire format.
func ParseTrigger(raw string) (Trigger, error) {
	t := Trigger(strings.ToLower(strings.TrimSpace(raw)))
	if !isKnownTrigger(t) {
		return "", fmt.Errorf("booking: unknown trigger %q", raw)
	}

	return t, nil
}
