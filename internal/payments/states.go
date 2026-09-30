package payments

import (
	"fmt"
	"slices"
)

type Status string

const (
	RequiresPaymentMethod Status = "requires_payment_method"
	RequiresConfirmation  Status = "requires_confirmation"
	RequiresAction        Status = "requires_action"
	Processing            Status = "processing"
	RequiresCapture       Status = "requires_capture"
	Succeeded             Status = "succeeded"
	Canceled              Status = "canceled"
)

var Statuses = []Status{
	RequiresPaymentMethod, RequiresConfirmation, RequiresAction, Processing, RequiresCapture, Succeeded, Canceled,
}

// transitions is the whole state machine: every status an intent may move to from each
// status. Nothing else changes an intent's status.
var transitions = map[Status][]Status{
	RequiresPaymentMethod: {RequiresConfirmation, Processing, Canceled},
	RequiresConfirmation:  {RequiresPaymentMethod, Processing, Canceled},
	// Processing covers a call to the rail in flight or with an unknown outcome.
	Processing: {RequiresPaymentMethod, RequiresAction, RequiresCapture, Succeeded, Canceled},
	// Completing the action resumes the authorization; failing it needs a new method.
	RequiresAction: {Processing, RequiresPaymentMethod, Canceled},
	// Capturing and voiding both call the rail, so both pass through processing.
	RequiresCapture: {Processing},
	Succeeded:       {},
	Canceled:        {},
}

func CanTransition(from, to Status) bool {
	return slices.Contains(transitions[from], to)
}

func checkTransition(from, to Status) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: a payment intent cannot go from %s to %s", ErrInvalidState, from, to)
	}
	return nil
}

type attemptStatus string

const (
	attemptAuthorizing          attemptStatus = "authorizing"
	attemptAuthorizationUnknown attemptStatus = "authorization_unknown"
	attemptRequiresAction       attemptStatus = "requires_action"
	attemptAuthorized           attemptStatus = "authorized"
	attemptDeclined             attemptStatus = "declined"
	attemptFailed               attemptStatus = "failed"
	attemptCapturing            attemptStatus = "capturing"
	attemptCaptureUnknown       attemptStatus = "capture_unknown"
	attemptCaptured             attemptStatus = "captured"
	attemptAuthenticating       attemptStatus = "authenticating"
	attemptVoiding              attemptStatus = "voiding"
	attemptVoidUnknown          attemptStatus = "void_unknown"
	attemptVoided               attemptStatus = "voided"
)
