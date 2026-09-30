package cardnetwork

import "github.com/iricardofernandes/jupiter/pkg/cardnet"

// behaviour is what the issuer, or the network in front of it, does with an
// authorization for a card.
type behaviour int

const (
	approve behaviour = iota
	declineDoNotHonour
	declineInsufficientFunds
	declineExpired
	// answerLost: the issuer approves and holds the amount, and the answer never reaches
	// the acquirer.
	answerLost
	// requestLost: the request never reaches the issuer; nothing is held.
	requestLost
	// answerLate: the issuer approves at once and the answer arrives after the
	// acquirer has given up.
	answerLate
	// answerTwice: the approval is sent twice.
	answerTwice
	// partialApproval: half the amount, when the acquirer says it can take a partial
	// approval; the whole amount otherwise.
	partialApproval
	// issuerDown: the issuer does not answer and the network stands in for it.
	issuerDown
)

// testCards are the card numbers with a behaviour of their own; every other valid number
// is approved while its credit limit lasts.
var testCards = map[string]behaviour{
	"4000000000000002": declineDoNotHonour,
	"4000000000009995": declineInsufficientFunds,
	"4000000000000069": declineExpired,
	"4000000000000119": answerLost,
	"4000000000000168": requestLost,
	"4000000000000127": answerLate,
	"4000000000000135": answerTwice,
	"4000000000000143": partialApproval,
	"4000000000000150": issuerDown,
}

func behaviourOf(pan string) behaviour {
	if b, ok := testCards[pan]; ok {
		return b
	}
	return approve
}

// Fault is what the simulation harness can make the network do to any request, on top
// of the card's own behaviour.
type Fault int

const (
	NoFault Fault = iota
	// LoseRequest drops the request before anyone processes it.
	LoseRequest
	// LoseAnswer processes the request and drops the answer.
	LoseAnswer
	// AnswerLate answers after the acquirer's timeout.
	AnswerLate
	// AnswerTwice sends the answer twice.
	AnswerTwice
)

func faultOf(b behaviour) Fault {
	switch b {
	case answerLost:
		return LoseAnswer
	case requestLost:
		return LoseRequest
	case answerLate:
		return AnswerLate
	case answerTwice:
		return AnswerTwice
	default:
		return NoFault
	}
}

func luhn(pan string) bool {
	if len(pan) < 12 || len(pan) > 19 {
		return false
	}
	sum := 0
	for i := range len(pan) {
		c := pan[len(pan)-1-i]
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if i%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

// responseFor maps a decline behaviour to its response code.
func responseFor(b behaviour) string {
	switch b {
	case declineDoNotHonour:
		return cardnet.DoNotHonour
	case declineInsufficientFunds:
		return cardnet.InsufficientFunds
	case declineExpired:
		return cardnet.ExpiredCard
	default:
		return cardnet.Approved
	}
}
