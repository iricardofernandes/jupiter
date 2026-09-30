package payments

import (
	"context"
	"slices"

	"github.com/iricardofernandes/jupiter/internal/money"
)

type Outcome string

const (
	Approved       Outcome = "approved"
	Declined       Outcome = "declined"
	ActionRequired Outcome = "requires_action"
	// Unknown means the call may or may not have taken effect: a timeout, a lost
	// response, a broken connection. It is never treated as a decline.
	Unknown Outcome = "unknown"
	// Pending answers a query about a request the rail has received but not finished.
	Pending Outcome = "pending"
	// NotFound answers a query about a request the rail never received.
	NotFound Outcome = "not_found"
)

type Result struct {
	Outcome     Outcome
	Reference   string
	DeclineCode string
	// NetworkTransactionID identifies an approved authorization across the network;
	// merchant-initiated payments quote the one that stored the card.
	NetworkTransactionID string
	// Pix is the charge a Pix attempt waits on, with ActionRequired.
	Pix *PixCharge
}

type AuthorizeRequest struct {
	Key           string
	Merchant      string
	Amount        money.Amount
	PaymentMethod string
	// Card names the vault token behind a saved card. The rail detokenizes it for the
	// one call that needs the number, so the number is never held anywhere else.
	Card          *CardReference
	Authenticated bool
	Installments  *Installments
	// MerchantInitiated marks a payment made without the cardholder, on a card stored by
	// an earlier one; FirstTransaction is that earlier payment's network transaction id.
	MerchantInitiated bool
	FirstTransaction  string
	// StoresCredential marks the customer-initiated payment that stores the card.
	StoresCredential bool
	// Authentication is the 3-D Secure result, when the cardholder authenticated.
	Authentication *AuthenticationData
}

type CardReference struct {
	Token string
	Owner string
	// NetworkToken says the card has an active network token, which the rail uses in
	// place of the number where it can.
	NetworkToken bool
}

// OperationRequest acts on an earlier authorization, named by the key it was sent with,
// so a void can reverse an authorization whose answer never arrived.
type OperationRequest struct {
	Key              string
	AuthorizationKey string
	Reference        string
	Amount           money.Amount
}

// Rail is what a payment method implements to move money: the test rail today, card
// networks and Pix in later phases. Every call carries an idempotency key derived from
// the attempt or refund it serves, so repeating a call, which recovery does, is safe.
// Query reports what became of the request sent with a key.
type Rail interface {
	Authorize(ctx context.Context, r AuthorizeRequest) Result
	Capture(ctx context.Context, r OperationRequest) Result
	Void(ctx context.Context, r OperationRequest) Result
	Refund(ctx context.Context, r OperationRequest) Result
	Query(ctx context.Context, key string) Result
}

// Test payment methods stand for cards without going through the vault; saved cards
// made from the test card numbers behave the same way. Amounts whose last two minor
// digits are 91 to 93 make the rail misbehave: see TestRail.
const (
	TestCardVisa                   = "pm_card_visa"
	TestCardMastercard             = "pm_card_mastercard"
	TestCardDeclined               = "pm_card_declined"
	TestCardInsufficientFunds      = "pm_card_insufficient_funds"
	TestCardAuthenticationRequired = "pm_card_authentication_required"
)

var TestPaymentMethods = []string{
	TestCardVisa, TestCardMastercard, TestCardDeclined, TestCardInsufficientFunds, TestCardAuthenticationRequired,
}

func knownPaymentMethod(pm string) bool {
	return slices.Contains(TestPaymentMethods, pm)
}

func captureKey(attemptID string) string { return attemptID + ":capture" }

func voidKey(attemptID string) string { return attemptID + ":void" }
