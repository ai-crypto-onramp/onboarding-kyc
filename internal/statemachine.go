package internal

import (
	"errors"
	"fmt"
	"time"
)

// State is the lifecycle state of a KYC application.
type State string

const (
	StateStarted          State = "started"
	StateDocumentsUploaded State = "documents_uploaded"
	StateLivenessPassed   State = "liveness_passed"
	StateScreening        State = "screening"
	StateVendorDecision   State = "vendor_decision"
	StatePass             State = "pass"
	StateFail             State = "fail"
	StateManualReview     State = "manual_review"
)

// TerminalStates are states from which the only legal forward motion is
// re-KYC (back to started) — no further in-flow transitions are allowed.
var TerminalStates = map[State]bool{
	StatePass: true,
	StateFail: true,
}

// legalTransitions defines the forward transition table. The special
// "re-kyc" transition (terminal -> started) is handled separately.
var legalTransitions = map[State]map[State]bool{
	StateStarted:           {StateDocumentsUploaded: true},
	StateDocumentsUploaded: {StateLivenessPassed: true, StateScreening: true},
	StateLivenessPassed:    {StateScreening: true, StateManualReview: true},
	StateScreening:         {StateVendorDecision: true, StateManualReview: true},
	StateVendorDecision:    {StatePass: true, StateFail: true, StateManualReview: true},
	StateManualReview:      {StatePass: true, StateFail: true},
	StatePass:              {StateStarted: true}, // re-KYC
	StateFail:              {StateStarted: true}, // re-KYC
}

// ErrIllegalTransition is returned when a requested state transition is
// not permitted by the transition table.
var ErrIllegalTransition = errors.New("illegal state transition")

// ErrReKYCNotTerminal is returned when a re-KYC re-open is attempted on a
// non-terminal application.
var ErrReKYCNotTerminal = errors.New("re-kyc only allowed from terminal states")

// CanTransition reports whether transitioning from -> to is legal.
func CanTransition(from, to State) bool {
	if tos, ok := legalTransitions[from]; ok {
		return tos[to]
	}
	return false
}

// ValidateTransition returns nil if from->to is legal, otherwise a typed
// error wrapping ErrIllegalTransition.
func ValidateTransition(from, to State) error {
	if CanTransition(from, to) {
		return nil
	}
	return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, from, to)
}

// IsTerminal reports whether s is a terminal state.
func IsTerminal(s State) bool {
	return TerminalStates[s]
}

// ReKYC re-opens a terminal application by transitioning it back to started.
// Returns ErrReKYCNotTerminal if the application is not in a terminal state.
func ReKYC(from State) (State, error) {
	if !IsTerminal(from) {
		return "", fmt.Errorf("%w: current state %s", ErrReKYCNotTerminal, from)
	}
	return StateStarted, nil
}

// transitionEvent is an in-memory domain event emitted on each state
// transition, consumed by the audit log.
type transitionEvent struct {
	ApplicationID string
	From         State
	To           State
	Reason       string
	OccurredAt   time.Time
}