// Package cardnetwork simulates a card network and the issuers behind it, over ISO 8583
// as pkg/cardnet specifies it. It stands for another company: nothing in Jupiter
// imports it, and Jupiter reaches it only over TCP and HTTP.
package cardnetwork

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/moov-io/iso8583"
	connection "github.com/moov-io/iso8583-connection"
	"github.com/moov-io/iso8583-connection/server"

	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

type Config struct {
	Now    func() time.Time
	Logger *slog.Logger
	// LateAfter is how long a late answer waits: longer than the acquirer's timeout.
	LateAfter time.Duration
	// Faults, if set, is asked about every request, on top of the card's own behaviour.
	Faults func(cardnet.Message) Fault
	// AuthenticationKey checks the 3-D Secure authentication values authorizations
	// carry; the 3-D Secure simulator makes them with the same key.
	AuthenticationKey []byte
	// TokenEventsURL is where the token service tells the token requestor about its
	// tokens, signed with TokenEventsSecret.
	TokenEventsURL    string
	TokenEventsSecret string
	// DisputeEventsURL is where the network tells the acquirer of its disputes and fraud
	// reports, signed with DisputeEventsSecret.
	DisputeEventsURL    string
	DisputeEventsSecret string
	// AcquirerToken, if set, is what the acquirer's dispute requests must carry as a
	// bearer token.
	AcquirerToken string
	// ClearingFaults, if set, is asked about every record before it goes into a clearing
	// file.
	ClearingFaults func(cardnet.ClearingRecord) RecordFault
}

// RecordFault is what happens to a clearing record: lost, listed twice, or put in the
// next business day's file.
type RecordFault struct {
	Drop      bool
	Duplicate bool
	Delay     bool
}

type Network struct {
	cfg    Config
	issuer *issuer
	tokens *tokenService
	server *server.Server

	mu    sync.Mutex
	files map[string][]byte
	// book is the disputes and fraud reports, guarded by the issuer's lock.
	book disputeBook

	stop    chan struct{}
	stopped sync.Once
	pending sync.WaitGroup
}

func New(cfg Config) *Network {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.LateAfter == 0 {
		cfg.LateAfter = 30 * time.Second
	}
	return &Network{
		cfg: cfg, issuer: newIssuer(cfg.Now, cfg.AuthenticationKey, cfg.ClearingFaults), tokens: newTokenService(),
		files: map[string][]byte{}, stop: make(chan struct{}), book: disputeBook{cases: map[string]*disputeCase{}},
	}
}

// Start listens for acquirers on addr, such as "127.0.0.1:0".
func (n *Network) Start(addr string) error {
	n.server = server.New(cardnet.Spec, cardnet.ReadLength, cardnet.WriteLength,
		connection.InboundMessageHandler(n.handle),
		connection.ErrorHandler(func(err error) {
			n.cfg.Logger.Warn("card network connection error", "error", err)
		}),
	)
	return n.server.Start(addr)
}

func (n *Network) Addr() string { return n.server.Addr }

func (n *Network) Close() {
	n.stopped.Do(func() {
		close(n.stop)
		n.pending.Wait()
		if n.server != nil {
			n.server.Close()
		}
	})
}

func (n *Network) handle(c *connection.Connection, raw *iso8583.Message) {
	req, err := cardnet.Unpack(raw)
	if err != nil {
		n.cfg.Logger.Warn("card network received an unreadable message", "error", err)
		return
	}
	if req.AcquirerID != "" {
		n.issuer.signOn(req.AcquirerID)
	}
	fault := NoFault
	if n.cfg.Faults != nil && req.MTI != cardnet.NetworkRequest {
		fault = n.cfg.Faults(req)
	}
	if fault == NoFault && (req.MTI == cardnet.AuthorizationRequest || req.MTI == cardnet.FinancialRequest) {
		fault = faultOf(behaviourOf(req.PAN))
	}
	if fault == LoseRequest {
		n.cfg.Logger.Info("card network lost a request", "message", req)
		return
	}
	resp, ok := n.process(req)
	if !ok {
		n.cfg.Logger.Warn("card network received an unexpected message", "message", req)
		return
	}
	n.send(c, resp, fault)
}

func (n *Network) process(req cardnet.Message) (cardnet.Message, bool) {
	switch req.MTI {
	case cardnet.NetworkRequest:
		if req.NetworkCode == cardnet.SignOn {
			n.issuer.signOn(req.AcquirerID)
		}
		resp := answer(req, cardnet.Approved)
		resp.NetworkCode = req.NetworkCode
		return resp, true
	case cardnet.AuthorizationRequest:
		if resp, ok := n.detokenize(&req); !ok {
			return resp, true
		}
		return n.issuer.authorize(req), true
	case cardnet.FinancialRequest:
		if req.ProcessingCode == cardnet.ProcessingRefund {
			return n.issuer.refund(req), true
		}
		if resp, ok := n.detokenize(&req); !ok {
			return resp, true
		}
		return n.purchase(req), true
	case cardnet.CompletionAdvice, cardnet.CompletionAdviceRepeat:
		return n.issuer.complete(req), true
	case cardnet.ReversalRequest, cardnet.ReversalAdvice, cardnet.ReversalAdviceRepeat:
		return n.issuer.reverse(req), true
	default:
		return cardnet.Message{}, false
	}
}

// detokenize swaps a network token in DE 2 for the card behind it, once its cryptogram
// checks out; a card number goes through as it is.
func (n *Network) detokenize(req *cardnet.Message) (cardnet.Message, bool) {
	cryptogram := ""
	if req.Private != nil {
		cryptogram = req.Private.TokenCryptogram
	}
	pan, rc := n.tokens.redeem(req.PAN, cryptogram, req.Amount)
	switch {
	case rc == "":
		return cardnet.Message{}, true
	case rc != cardnet.Approved:
		return answer(*req, rc), false
	}
	req.PAN = pan
	return cardnet.Message{}, true
}

// purchase is a single-message 0200: authorized and completed at once.
func (n *Network) purchase(req cardnet.Message) cardnet.Message {
	resp := n.issuer.authorize(req)
	resp.MTI = cardnet.FinancialResponse
	if resp.ResponseCode != cardnet.Approved {
		return resp
	}
	completion := req
	completion.MTI, completion.Private = cardnet.CompletionAdvice, &cardnet.PrivateData{NetworkTransactionID: resp.NetworkTransactionID()}
	n.issuer.complete(completion)
	return resp
}

func (n *Network) send(c *connection.Connection, resp cardnet.Message, fault Fault) {
	msg, err := cardnet.Pack(resp)
	if err != nil {
		n.cfg.Logger.Error("card network could not build an answer", "error", err)
		return
	}
	switch fault {
	case LoseAnswer:
		n.cfg.Logger.Info("card network lost an answer", "message", resp)
	case AnswerLate:
		n.later(func() { n.reply(c, msg) })
	case AnswerTwice:
		n.reply(c, msg)
		n.later(func() { n.reply(c, msg) })
	default:
		n.reply(c, msg)
	}
}

func (n *Network) reply(c *connection.Connection, msg *iso8583.Message) {
	if err := c.Reply(msg); err != nil && !errors.Is(err, connection.ErrConnectionClosed) {
		n.cfg.Logger.Warn("card network could not answer", "error", err)
	}
}

// later runs fn after LateAfter, unless the network stops first.
func (n *Network) later(fn func()) {
	n.pending.Add(1)
	go func() {
		defer n.pending.Done()
		select {
		case <-time.After(n.cfg.LateAfter):
			fn()
		case <-n.stop:
		}
	}()
}

// CloseDay ends the business day date: every acquirer's completions and refunds since
// the last close go into its clearing file for that date.
func (n *Network) CloseDay(date time.Time) error {
	date = time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	n.mu.Lock()
	defer n.mu.Unlock()
	for acquirer := range n.issuer.acquirersSeen() {
		if _, done := n.files[fileKey(acquirer, date)]; done {
			return fmt.Errorf("cardnetwork: the business day %s is already closed", date.Format(time.DateOnly))
		}
	}
	for acquirer, f := range n.issuer.closeDay(date) {
		n.files[fileKey(acquirer, date)] = f.Encode()
	}
	return nil
}

func fileKey(acquirer string, date time.Time) string {
	return cardnet.PadAcquirer(acquirer) + "/" + date.Format(time.DateOnly)
}

// ClearingFile returns an acquirer's file for a closed business day.
func (n *Network) ClearingFile(acquirer string, date time.Time) ([]byte, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	f, ok := n.files[fileKey(acquirer, date)]
	return f, ok
}

// Holds lists the authorizations still holding credit.
func (n *Network) Holds() []Hold { return n.issuer.holds() }

// Completions lists the authorizations the acquirer completed.
func (n *Network) Completions() []Completion { return n.issuer.completions() }

// RunDays closes each business day when the clock passes midnight, until ctx ends.
func (n *Network) RunDays(ctx context.Context, every time.Duration) error {
	day := n.cfg.Now().UTC()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		n.AdvanceDisputes(ctx)
		now := n.cfg.Now().UTC()
		if now.YearDay() != day.YearDay() || now.Year() != day.Year() {
			if err := n.CloseDay(day); err != nil {
				n.cfg.Logger.ErrorContext(ctx, "closing the business day", "error", err)
			}
			day = now
		}
	}
}
