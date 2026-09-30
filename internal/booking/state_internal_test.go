package booking

import (
	"errors"
	"testing"
)

// allStates and allTriggers enumerate the closed sets. A new state or trigger
// that is not added here is a signal to extend the tests, not to let the
// omission pass silently.
var allStates = []State{
	StateIntent, StateSearching, StateOffered, StateHolding, StateHeld,
	StateConfirming, StateConfirmed, StateCancelling, StateCancelled,
	StateFailed, StateExpired,
}

var allTriggers = []Trigger{
	TriggerSearchRequested, TriggerSearchSucceeded, TriggerSearchFailed,
	TriggerHoldRequested, TriggerHoldSucceeded, TriggerHoldFailed,
	TriggerConfirmRequested, TriggerConfirmSucceeded, TriggerConfirmFailed,
	TriggerCancelRequested, TriggerCancelSucceeded, TriggerCancelFailed,
	TriggerHoldExpired,
}

// next must accept exactly the triggers that AllowedTransitions advertises. If
// the two ever disagree, a client that renders actions from AllowedTransitions
// can trigger a transition the domain refuses.
func TestAllowedTransitionsMatchTheTransitionTable(t *testing.T) {
	for _, from := range allStates {
		allowed := make(map[Trigger]bool)

		for _, tr := range AllowedTransitions(from) {
			allowed[tr] = true

			if _, _, err := next(from, tr); err != nil {
				t.Errorf("%s: advertised trigger %q is rejected: %v", from, tr, err)
			}
		}

		for _, tr := range allTriggers {
			if allowed[tr] {
				continue
			}

			if _, _, err := next(from, tr); err == nil {
				t.Errorf("%s: undeclared trigger %q is accepted", from, tr)
			} else {
				var invalid *ErrInvalidTransition

				if !errors.As(err, &invalid) {
					t.Errorf("%s + %q: want ErrInvalidTransition, got %T", from, tr, err)
				}
			}
		}
	}
}

// Every reachable state must be able to reach a terminal state. A state with no
// way out would strand a booking forever.
func TestEveryNonTerminalStateCanReachATerminalState(t *testing.T) {
	terminal := map[State]bool{
		StateConfirmed: true,
		StateCancelled: true,
		StateFailed:    true,
		StateExpired:   true,
	}

	for _, start := range allStates {
		if terminal[start] {
			continue
		}

		if !canReachTerminal(start, terminal) {
			t.Errorf("%s cannot reach any terminal state", start)
		}
	}
}

// canReachTerminal explores the transition graph breadth-first.
func canReachTerminal(start State, terminal map[State]bool) bool {
	seen := map[State]bool{start: true}
	queue := []State{start}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, tr := range AllowedTransitions(current) {
			nextState, _, err := next(current, tr)
			if err != nil {
				continue
			}

			if terminal[nextState] {
				return true
			}

			if !seen[nextState] {
				seen[nextState] = true

				queue = append(queue, nextState)
			}
		}
	}

	return false
}

// IsTerminal and the absence of outgoing transitions must agree.
func TestIsTerminalMatchesTheGraph(t *testing.T) {
	for _, s := range allStates {
		wantTerminal := len(AllowedTransitions(s)) == 0

		if IsTerminal(s) != wantTerminal {
			t.Errorf("%s: IsTerminal = %v, want %v", s, IsTerminal(s), wantTerminal)
		}
	}
}

// Only a confirmed or closed booking may be non-preliminary. A client that
// renders "purchase" for a preliminary state would mislead the user.
func TestPreliminaryCoversOnlyUnboundStates(t *testing.T) {
	for _, s := range allStates {
		switch s {
		case StateConfirmed, StateCancelled, StateFailed, StateExpired:
			if s.Preliminary() {
				t.Errorf("%s must not be preliminary", s)
			}
		default:
			if !s.Preliminary() {
				t.Errorf("%s must be preliminary", s)
			}
		}
	}
}

// An unknown state or trigger arriving from storage or from the wire must be
// rejected rather than defaulted.
func TestParseRejectsUnknownValues(t *testing.T) {
	if _, err := ParseState("teleported"); err == nil {
		t.Error("ParseState accepted an unknown state")
	}

	if _, err := ParseTrigger("user_thought_about_it"); err == nil {
		t.Error("ParseTrigger accepted an unknown trigger")
	}

	if _, err := ParseCabin("spaceship"); err == nil {
		t.Error("ParseCabin accepted an unknown cabin")
	}

	if _, err := ParseCurrency("XYZ"); err == nil {
		t.Error("ParseCurrency accepted an unknown currency")
	}
}

// Normalisation must be stable, so a retry of the same request cannot produce
// two different spellings and therefore two different idempotency keys.
func TestLocationNormalisationIsIdempotent(t *testing.T) {
	first := Location{Code: " led "}.Normalised()
	second := first.Normalised()

	if first.Code != second.Code {
		t.Fatalf("normalisation is not stable: %q vs %q", first.Code, second.Code)
	}

	if first.Code != "LED" {
		t.Fatalf("Code = %q, want LED", first.Code)
	}
}
