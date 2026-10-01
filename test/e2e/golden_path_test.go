//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/authentication"
	"github.com/iricardofernandes/jupiter/internal/bank"
	"github.com/iricardofernandes/jupiter/internal/disputes"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/pix"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/receivables"
	"github.com/iricardofernandes/jupiter/internal/recipients"
	"github.com/iricardofernandes/jupiter/internal/registry"
	"github.com/iricardofernandes/jupiter/internal/risk"
	threedssim "github.com/iricardofernandes/jupiter/internal/sim/3ds"
	"github.com/iricardofernandes/jupiter/internal/sim/cardnetwork"
	pixsim "github.com/iricardofernandes/jupiter/internal/sim/pix"
	registrysim "github.com/iricardofernandes/jupiter/internal/sim/registry"
	slcsim "github.com/iricardofernandes/jupiter/internal/sim/slc"
	"github.com/iricardofernandes/jupiter/internal/slc"
	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/internal/vault/vaulttest"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
	"github.com/iricardofernandes/jupiter/pkg/webhook"
)

var server *postgrestest.Server

func TestMain(m *testing.M) {
	os.Exit(postgrestest.Templates(m, &server, map[string][]postgrestest.MigrateFunc{
		postgrestest.DefaultTemplate: {
			ledger.Migrate, merchant.Migrate, events.Migrate, payments.Migrate, api.Migrate, jobs.Migrate, risk.Migrate, acquirer.Migrate,
			authentication.Migrate, recipients.Migrate, receivables.Migrate, bank.Migrate, disputes.Migrate,
		},
		vaulttest.Template: {vaulttest.Migrate},
	}))
}

// The golden path, as far as phase 11 reaches: a marketplace onboards a seller; a
// customer's card goes from their browser to the vault; the customer pays R$ 600.00 in
// six installments, split between the seller and the marketplace, passes the risk engine,
// is authenticated with 3-D Secure without a challenge, and is authorized by the issuer
// over ISO 8583; the marketplace captures it; the ledger's typed split lines and a signed
// webhook say so; the network's clearing file for the day confirms it; each one's
// receivable units are registered with the registry and reconcile with it; the seller
// anticipates two units, which Jupiter reports to the SLC, and is paid out by Pix; months
// later the SLC settles the units due, and she is paid out what settled to her by Pix.
func TestGoldenPath(t *testing.T) {
	ctx := t.Context()
	pool := server.Pool(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, err := secretbox.FromBase64(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	inserter, err := jobs.NewInserter(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Live-mode webhooks go to https only; the receiver's client trusts its test certificate.
	delivered := make(chan []byte, 20)
	receiverSecret := make(chan string, 1)
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		secret := <-receiverSecret
		receiverSecret <- secret
		if err := webhook.Verify(body, r.Header.Get(webhook.SignatureHeader), secret, webhook.DefaultTolerance, time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		delivered <- body
	}))
	defer receiver.Close()
	eventService := events.New(events.Config{Box: box, Jobs: inserter, Render: api.RenderEvent, HTTPClient: receiver.Client()})
	cardVault := vaulttest.Start(t, server.PoolFrom(t, vaulttest.Template), vaulttest.Options{})
	schemeKey := []byte("golden path scheme key")
	var jupiterHandler http.Handler
	jupiter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { jupiterHandler.ServeHTTP(w, r) }))
	defer jupiter.Close()
	network := cardnetwork.New(cardnetwork.Config{
		AuthenticationKey: schemeKey, DisputeEventsURL: jupiter.URL + acquirer.DisputeEventsPath, DisputeEventsSecret: disputeEventsSecret,
		AcquirerToken: networkToken,
	})
	if err := network.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer network.Close()
	networkFiles := httptest.NewServer(network.Handler())
	defer networkFiles.Close()
	connector, err := acquirer.New(acquirer.Config{
		Pool: pool, Addr: network.Addr(), NetworkURL: networkFiles.URL, Cards: cardVault.Client,
		DisputeEventsSecret: disputeEventsSecret, NetworkToken: networkToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connector.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connector.Close() }()
	directory := httptest.NewServer(threedssim.New(threedssim.Config{AuthenticationKey: schemeKey, ResultsSecret: "golden"}).Handler())
	defer directory.Close()
	authenticator, err := authentication.New(authentication.Config{
		Pool: pool, DirectoryURL: directory.URL + "/ds/areq", PublicURL: jupiter.URL, ResultsSecret: "golden", Cards: cardVault.Client,
	})
	if err != nil {
		t.Fatal(err)
	}
	bank, pixConnector := startPixBank(t, pool)
	merchants := merchant.New(nil)
	l := ledger.New()
	recipientService := recipients.New(recipients.Config{Merchants: merchants, Events: eventService})
	// Settlement is months away: the receivables and the SLC run on a clock that can be
	// moved ahead.
	clock := &laterClock{}
	slcSim, slcConnector := startSLC(t, clock.Now)
	registrySim, receivablesService := startRegistry(t, pool, l, merchants, recipientService, eventService, slcConnector, clock.Now)
	riskEngine := risk.New(risk.Config{})
	paymentService := payments.New(payments.Config{
		Ledger: l, Events: eventService, LiveRail: connector, Authenticator: authenticator, Risk: riskEngine,
		TestRail: payments.NewTestRail(pool, nil, nil).WithCards(cardVault.Client), LivePix: pixConnector,
		Receivables: receivablesService, Balances: receivablesService, Recipients: recipientService,
	})
	receivablesService.UsePayments(paymentService)
	disputeService := disputes.New(disputes.Config{Pool: pool, Payments: paymentService, Events: eventService, LiveNetwork: connector})
	jupiterAPI := api.New(api.Deps{
		Pool: pool, Merchants: merchants, Events: eventService, Payments: paymentService, Vault: cardVault.Client, Risk: riskEngine, Box: box,
		Receivables: receivablesService, Recipients: recipientService, Disputes: disputeService,
	})
	authenticator.Attach(paymentService, jupiterAPI.ResumePayment)
	mux := http.NewServeMux()
	mux.Handle("/3ds/", authenticator.Handler())
	mux.Handle("/network/", connector.Handler(paymentService, disputeService))
	mux.Handle("/", jupiterAPI.Handler())
	jupiterHandler = mux

	workers := river.NewWorkers()
	eventService.RegisterWorkers(workers, pool)
	worker, err := jobs.NewWorker(pool, jobs.WorkerConfig{Workers: workers, FetchPollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- jobs.Run(worker)(workerCtx) }()
	defer func() { stopWorker(); <-workerDone }()

	t.Log("1. a marketplace signs up with its CNPJ and receives its live keys")
	var secretKey, publishableKey string
	err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		m, keys, err := merchants.Create(ctx, tx, "Loja do Caminho Dourado", api.CurrentVersion)
		if err != nil {
			return err
		}
		for _, k := range keys {
			switch {
			case !k.Livemode:
			case k.Kind == merchant.Secret:
				secretKey = k.Value
			case k.Kind == merchant.Publishable:
				publishableKey = k.Value
			}
		}
		// Its CNPJ: whom its receivables belong to at the registry.
		return merchants.SetTaxID(ctx, tx, m.ID, merchantCNPJ)
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &apiClient{t: t, base: jupiter.URL, key: secretKey}

	t.Log("2. it registers a webhook endpoint")
	var endpoint struct{ Secret string }
	client.post("/v1/webhook_endpoints", map[string]any{"url": receiver.URL, "enabled_events": []string{"payment_intent.succeeded"}}, &endpoint)
	receiverSecret <- endpoint.Secret

	t.Log("   and onboards a seller, with her CNPJ and Pix key; Jupiter verifies her")
	var seller struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	client.post("/v1/recipients", map[string]any{
		"name": "Vendedora do Caminho", "tax_id": sellerCNPJ, "payout_destination": map[string]any{"type": "pix", "pix_key": sellerKey},
	}, &seller)
	sellerID, err := recipients.Prefix.Parse(seller.ID)
	if err != nil {
		t.Fatal(err)
	}
	marketplace := recipients.Owner{Merchant: merchantOf(t, merchants, pool, secretKey), Livemode: true}
	if err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := recipientService.Verify(ctx, tx, marketplace, sellerID, recipients.Verified)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	t.Log("3. the customer types their card into the checkout page, which sends it to the vault")
	browser := &apiClient{t: t, base: cardVault.PublicURL, key: publishableKey, noIdempotencyKey: true}
	var token struct {
		ID    string `json:"id"`
		Last4 string `json:"last4"`
	}
	browser.post(vault.PublicTokensPath, map[string]any{"number": "4242 4242 4242 4242", "exp_month": 12, "exp_year": 2030, "cvc": "123"}, &token)
	t.Logf("   the page gets back %s, a token for the card ending %s; the number never reaches the merchant or Jupiter's API", token.ID[:8]+"…", token.Last4)

	t.Log("4. the merchant's server saves the card with the token")
	var method struct {
		ID   string `json:"id"`
		Card struct {
			Brand string `json:"brand"`
			BIN   string `json:"bin"`
		} `json:"card"`
	}
	client.post("/v1/payment_methods", map[string]any{"type": "card", "card": map[string]any{"token": token.ID}}, &method)
	if method.Card.Brand != "visa" || method.Card.BIN != "42424242" {
		t.Fatalf("payment method = %+v", method)
	}

	t.Log("5. the customer pays R$ 600.00 in six installments, split 90% to the seller and 10% to the marketplace: the issuer authenticates them with 3-D Secure, frictionless, and authorizes it over ISO 8583")
	var intent struct {
		ID               string `json:"id"`
		Status           string `json:"status"`
		AmountCapturable int64  `json:"amount_capturable"`
		AmountReceived   int64  `json:"amount_received"`
	}
	client.post("/v1/payment_intents", map[string]any{
		"amount": 60000, "currency": "brl", "capture_method": "manual",
		"payment_method": method.ID, "confirm": true,
		"installments": map[string]any{"count": 6, "financed_by": "merchant"}, "request_three_d_secure": "any",
		"customer_ip": "189.40.12.7",
		"split": []map[string]any{
			{"recipient": seller.ID, "percentage": "90.00", "liable": true},
			{"recipient": "me", "percentage": "10.00", "remainder": true, "charge_fee": true},
		},
	}, &intent)
	if intent.Status != "requires_capture" || intent.AmountCapturable != 60000 {
		t.Fatalf("after authorization: %+v", intent)
	}
	owner := payments.Owner{Merchant: merchantOf(t, merchants, pool, secretKey), Livemode: true}
	intentID, err := payments.IntentPrefix.Parse(intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := paymentService.LatestAttempt(ctx, pool, owner, intentID)
	if err != nil || attempt.NetworkTransactionID == "" || attempt.Installments == nil || attempt.Installments.Count != 6 {
		t.Fatalf("the authorization: %+v, %v", attempt, err)
	}
	var eci string
	var liabilityShift bool
	if err := pool.QueryRow(ctx, "SELECT eci, liability_shift FROM payments.attempts WHERE id = $1", attempt.ID.String()).Scan(&eci, &liabilityShift); err != nil || eci != "05" || !liabilityShift {
		t.Fatalf("3-D Secure: eci %q, liability shift %t, %v", eci, liabilityShift, err)
	}
	t.Logf("   authenticated (ECI %s, liability shifted to the issuer); network transaction %s; 6 installments financed by the merchant", eci, attempt.NetworkTransactionID)
	if posted, held := balance(t, paymentService, pool, owner); posted != 0 || held != 60000 {
		t.Fatalf("ledger after authorization: posted %d, held %d; want 0 and 60000", posted, held)
	}
	t.Log("   the ledger holds R$ 600.00 as pending for the merchant")

	t.Log("6. the merchant captures it")
	client.post("/v1/payment_intents/"+intent.ID+"/capture", nil, &intent)
	if intent.Status != "succeeded" || intent.AmountReceived != 60000 {
		t.Fatalf("after capture: %+v", intent)
	}
	if posted, held := balance(t, paymentService, pool, owner); posted != 0 || held != 0 {
		t.Fatalf("the marketplace's balance after capture: posted %d, held %d; want 0 and 0", posted, held)
	}
	sellerBalance := recipientBalance(t, receivablesService, pool, owner, seller.ID)
	ownBalance := recipientBalance(t, receivablesService, pool, owner, "")
	if sellerBalance.Pending != 54000 || ownBalance.Pending != 6000-fee {
		t.Fatalf("pending balances: the seller %+v, the marketplace %+v", sellerBalance, ownBalance)
	}
	t.Log("   the split's typed lines: R$ 540.00 to the seller, R$ 60.00 commission to the marketplace, which pays Jupiter's fee of R$ 20.94 (3.49% for 6 installments); both pending until their installments settle")

	t.Log("7. a signed webhook reports the payment")
	select {
	case body := <-delivered:
		var event struct {
			Type          string              `json:"type"`
			RelatedObject struct{ ID string } `json:"related_object"`
		}
		if err := json.Unmarshal(body, &event); err != nil || event.Type != "payment_intent.succeeded" || event.RelatedObject.ID != intent.ID {
			t.Fatalf("webhook = %s", body)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no webhook arrived")
	}

	t.Log("8. the network closes the day; its clearing file confirms the capture")
	today := time.Now().UTC()
	if err := network.CloseDay(today); err != nil {
		t.Fatal(err)
	}
	cleared, err := connector.ImportClearing(ctx, today, paymentService)
	if err != nil || cleared.Cleared != 1 || cleared.Exceptions != 0 {
		t.Fatalf("clearing: %+v, %v", cleared, err)
	}
	if attempt, err = paymentService.LatestAttempt(ctx, pool, owner, intentID); err != nil || attempt.AmountCleared != 60000 {
		t.Fatalf("after clearing: %+v, %v", attempt, err)
	}
	t.Log("   R$ 600.00 cleared")

	t.Log("9. each one's six receivable units, one per installment, are registered with the registry, and reconcile with it")
	if _, err := receivablesService.Register(ctx, pool); err != nil {
		t.Fatal(err)
	}
	agenda, err := receivablesService.Agenda(ctx, pool, owner, receivables.AgendaQuery{Recipient: seller.ID, From: today, To: today.AddDate(0, 0, 250)})
	if err != nil || len(agenda) != 6 {
		t.Fatalf("the seller's agenda: %+v, %v", agenda, err)
	}
	for _, e := range agenda {
		if e.Value != 9000 || !e.Registered || e.Arrangement != registryapi.ArrangementVisaCredit {
			t.Fatalf("a unit: %+v", e)
		}
	}
	accreditor := registrysim.Participant{TaxID: jupiterCNPJ, Role: registrysim.Accreditor}
	for holder, value := range map[string]int64{sellerCNPJ: 9000, merchantCNPJ: 651} {
		positions, err := registrySim.Positions(accreditor, holder, "", "", nil)
		if err != nil || len(positions) != 6 || positions[0].Value != value {
			t.Fatalf("the registry has %+v for %s, %v", positions, holder, err)
		}
	}
	reconcile(t, receivablesService, pool)
	t.Logf("   R$ 90.00 for the seller and R$ 6.51 for the marketplace settle on each of %s … %s; daily, weekly and fortnightly reconciliations find no divergence",
		agenda[0].SettlementDate, agenda[5].SettlementDate)

	t.Log("10. the seller anticipates her first two units: Jupiter prices them, buys them, and the registry records the transfer")
	var quote, anticipation struct {
		ID     string `json:"id"`
		Amount int64  `json:"amount"`
		Price  int64  `json:"price"`
	}
	client.post("/v1/anticipations/simulate", map[string]any{"recipient": seller.ID, "units": []string{agenda[0].Unit, agenda[1].Unit}}, &quote)
	client.post("/v1/anticipations", map[string]any{"quote": quote.ID}, &anticipation)
	if anticipation.Amount != 18000 || anticipation.Price != quote.Price || quote.Price <= 17000 {
		t.Fatalf("the quote %+v, the anticipation %+v", quote, anticipation)
	}
	sellerBalance = recipientBalance(t, receivablesService, pool, owner, seller.ID)
	if sellerBalance.Pending != 54000-18000 || sellerBalance.Available != anticipation.Price {
		t.Fatalf("the seller's balance: %+v", sellerBalance)
	}
	positions, err := registrySim.Positions(accreditor, sellerCNPJ, "", "", nil)
	if err != nil || len(positions[0].Committed) != 1 || positions[0].Committed[0].Beneficiary != jupiterCNPJ || positions[1].Committed[0].Amount != 9000 {
		t.Fatalf("the registry: %+v, %v", positions, err)
	}
	if _, err := receivablesService.Register(ctx, pool); err != nil {
		t.Fatal(err)
	}
	reconcile(t, receivablesService, pool)
	if n, err := receivablesService.ReportAnticipations(ctx, pool); err != nil || n != 2 {
		t.Fatalf("reporting the anticipations: %d, %v", n, err)
	}
	if reports, late := slcSim.Reports(slcToken); len(reports) != 2 || len(late) != 0 {
		t.Fatalf("the SLC has %+v, late %+v", reports, late)
	}
	t.Logf("   R$ 180.00 settling on %s and %s bought for R$ %d.%02d at 1.99%% a month; both units now settle to Jupiter, which reports them to the SLC; the ledger, the balances and the registry agree",
		agenda[0].SettlementDate, agenda[1].SettlementDate, anticipation.Price/100, anticipation.Price%100)

	t.Log("11. the seller is paid out her available balance, by Pix, to her key")
	var payout struct {
		Status      string `json:"status"`
		EndToEndID  string `json:"end_to_end_id"`
		Destination struct {
			RecipientName string `json:"recipient_name"`
		} `json:"destination"`
	}
	client.post("/v1/payouts", map[string]any{"amount": anticipation.Price, "currency": "brl", "recipient": seller.ID}, &payout)
	if payout.Status != "paid" || payout.Destination.RecipientName != "Vendedora do Caminho" {
		t.Fatalf("the payout: %+v", payout)
	}
	transfers := bank.Transfers()
	want := fmt.Sprintf("%d.%02d", anticipation.Price/100, anticipation.Price%100)
	if len(transfers) != 1 || transfers[0].Valor != want || transfers[0].Chave != sellerKey || transfers[0].EndToEndID != payout.EndToEndID {
		t.Fatalf("the bank sent %+v", transfers)
	}
	if b := recipientBalance(t, receivablesService, pool, owner, seller.ID); b.Available != 0 || b.Pending != 36000 {
		t.Fatalf("the seller's balance after the payout: %+v", b)
	}
	t.Logf("   R$ %s sent through the SPI to %s (%s); her available balance is zero, R$ 360.00 still pending", want, payout.Destination.RecipientName, payout.EndToEndID)

	t.Logf("12. on %s, the third installments' date, the SLC settles the units due: the network pays their gross into Jupiter's settlement account", agenda[2].SettlementDate)
	third, err := time.Parse(time.DateOnly, agenda[2].SettlementDate)
	if err != nil {
		t.Fatal(err)
	}
	clock.Until(third.Add(15 * time.Hour))
	if err := receivablesService.SettleDay(ctx, pool); err != nil {
		t.Fatal(err)
	}
	slcSim.Tick()
	if err := receivablesService.SettleDay(ctx, pool); err != nil {
		t.Fatal(err)
	}
	grade, err := slcSim.GradeOf(slcToken, agenda[2].SettlementDate)
	if err != nil || grade.Status != "settled" || grade.Total != 30000 || grade.Credited != 30000 || len(grade.Entries) != 9 {
		t.Fatalf("the grade: %+v, %v", grade, err)
	}
	sellerBalance = recipientBalance(t, receivablesService, pool, owner, seller.ID)
	ownBalance = recipientBalance(t, receivablesService, pool, owner, "")
	if sellerBalance.Available != 9000 || sellerBalance.Pending != 27000 || ownBalance.Available != 3*651 {
		t.Fatalf("after settlement: the seller %+v, the marketplace %+v", sellerBalance, ownBalance)
	}
	t.Log("   R$ 300.00 for three installments: R$ 180.00 Jupiter bought, R$ 90.00 the seller's, R$ 19.53 the marketplace's and R$ 10.47 of Jupiter's fee; the seller has R$ 90.00 available")

	t.Log("13. the seller is paid out what settled to her, by Pix")
	client.post("/v1/payouts", map[string]any{"amount": 9000, "currency": "brl", "recipient": seller.ID}, &payout)
	if payout.Status != "paid" || len(bank.Transfers()) != 2 {
		t.Fatalf("the second payout: %+v", payout)
	}
	if b := recipientBalance(t, receivablesService, pool, owner, seller.ID); b.Available != 0 || b.Pending != 27000 {
		t.Fatalf("the seller's balance after the second payout: %+v", b)
	}
	t.Logf("   R$ 90.00 sent through the SPI (%s)", payout.EndToEndID)

	t.Log("14. the customer disputes the payment, the goods not received: the network opens a chargeback and tells Jupiter")
	if _, err := network.OpenDispute(ctx, cardnetwork.DisputeParams{
		NetworkTransactionID: attempt.NetworkTransactionID, ReasonCode: "13.1", Issuer: cardnetwork.IssuerAccepts,
	}); err != nil {
		t.Fatal(err)
	}
	var disputeList struct {
		Data []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Funds  string `json:"funds"`
			Amount int64  `json:"amount"`
		} `json:"data"`
	}
	client.get("/v1/disputes?payment_intent="+intent.ID, &disputeList)
	if len(disputeList.Data) != 1 || disputeList.Data[0].Status != "needs_response" || disputeList.Data[0].Funds != "withdrawn" {
		t.Fatalf("the dispute: %+v", disputeList)
	}
	if b := recipientBalance(t, receivablesService, pool, owner, seller.ID); b.Available != -60000 {
		t.Fatalf("the seller, liable, bears the R$ 600.00: %+v", b)
	}
	recovered, err := receivablesService.Recover(ctx, pool)
	if err != nil || recovered != 27000 {
		t.Fatalf("recovered %d, %v", recovered, err)
	}
	if b := recipientBalance(t, receivablesService, pool, owner, seller.ID); b.Available != -33000 || b.Pending != 0 {
		t.Fatalf("the seller after the recovery: %+v", b)
	}
	t.Log("   the seller, liable for the split's chargebacks, had nothing available: R$ 600.00 is taken from her, R$ 270.00 of it at once from her three units still pending, free of any contract")

	t.Log("15. the marketplace answers with the delivery's tracking; Jupiter represents it, the issuer accepts, and it all comes back")
	var won struct {
		Status string `json:"status"`
		Funds  string `json:"funds"`
	}
	client.post("/v1/disputes/"+disputeList.Data[0].ID, map[string]any{
		"evidence": map[string]any{"shipping_tracking_number": "BR123456789", "product_description": "Seis parcelas de um tênis"}, "submit": true,
	}, &won)
	if won.Status != "won" || won.Funds != "reinstated" {
		t.Fatalf("the representment: %+v", won)
	}
	if b := recipientBalance(t, receivablesService, pool, owner, seller.ID); b.Available != 27000 {
		t.Fatalf("the seller after winning: %+v", b)
	}
	if _, err := receivablesService.Register(ctx, pool); err != nil {
		t.Fatal(err)
	}
	reconcile(t, receivablesService, pool)
	t.Log("   won: the R$ 600.00 is the seller's again; her reduced units stay reduced, so R$ 270.00 is available to her now; the registry and Jupiter agree")

	t.Log("16. every invariant holds")
	if _, err := l.ApplyQueued(ctx, pool, 10_000); err != nil {
		t.Fatal(err)
	}
	report, err := l.Check(ctx, pool, ledger.CheckOptions{ClearingGrace: time.Hour, ExpiryGrace: time.Hour})
	if err != nil || len(report.Violations) > 0 {
		t.Fatalf("ledger check: %v %+v", err, report.Violations)
	}
	violations, err := paymentService.Check(ctx, pool)
	if err != nil || len(violations) > 0 {
		t.Fatalf("payments check: %v %+v", err, violations)
	}
	found, err := receivablesService.Check(ctx, pool)
	if err != nil || len(found) > 0 {
		t.Fatalf("receivables check: %v %+v", err, found)
	}
	stuck, err := disputeService.Check(ctx, pool)
	if err != nil || len(stuck) > 0 {
		t.Fatalf("disputes check: %v %+v", err, stuck)
	}
}

const (
	jupiterCNPJ    = "11222333000181"
	merchantCNPJ   = "11444777000161"
	sellerCNPJ     = "11222333000262"
	registryToken  = "golden path registry token"  //nolint:gosec // a test credential for the simulator
	financierToken = "golden path financier token" //nolint:gosec // a test credential for the simulator
	slcToken       = "golden path slc token"       //nolint:gosec // a test credential for the simulator
	// disputeEventsSecret signs the network's dispute events; networkToken is Jupiter's at
	// its dispute system.
	disputeEventsSecret = "golden path dispute events secret"
	networkToken        = "golden path network token"
	jupiterISPB         = "30000001"
	// fee is Jupiter's on R$ 600.00 in 6 installments financed by the merchant: 3.49%.
	fee = 2094
)

func recipientBalance(t *testing.T, s *receivables.Service, pool *pgxpool.Pool, owner payments.Owner, recipientID string) receivables.Balance {
	t.Helper()
	var b receivables.Balance
	err := postgres.InTx(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		b, err = s.Balance(t.Context(), tx, owner, recipientID, money.BRL)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func reconcile(t *testing.T, s *receivables.Service, pool *pgxpool.Pool) {
	t.Helper()
	for _, kind := range []string{receivables.Daily, receivables.Weekly, receivables.Fortnightly} {
		if r, err := s.ReconcileNow(t.Context(), pool, true, kind); err != nil || len(r.Divergences) != 0 {
			t.Fatalf("the %s reconciliation: %+v, %v", kind, r.Divergences, err)
		}
	}
}

// startRegistry runs the registry simulator and the receivables Jupiter registers there,
// as accreditor and as the financier of what it anticipates, and settles through the SLC.
func startRegistry(t *testing.T, pool *pgxpool.Pool, l *ledger.Ledger, merchants *merchant.Service, recs *recipients.Service, e *events.Service,
	settlement *slc.Connector, now func() time.Time,
) (*registrysim.Sim, *receivables.Service) {
	t.Helper()
	sim := registrysim.New(registrysim.Config{Participants: []registrysim.Participant{
		{Token: registryToken, TaxID: jupiterCNPJ, Role: registrysim.Accreditor},
		{Token: financierToken, TaxID: jupiterCNPJ, Role: registrysim.Financier},
	}})
	srv := httptest.NewServer(sim.Handler())
	t.Cleanup(srv.Close)
	connector, err := registry.New(registry.Config{BaseURL: srv.URL, Token: registryToken, FinancierToken: financierToken})
	if err != nil {
		t.Fatal(err)
	}
	return sim, receivables.New(receivables.Config{
		Pool: pool, Ledger: l, Merchants: merchants, Recipients: recs, Events: e, TaxID: jupiterCNPJ, LiveRegistry: connector,
		LiveSettlement: settlement, Domicile: registryapi.Domicile{ISPB: jupiterISPB, Branch: "0001"}, Now: now,
	})
}

// startSLC runs the settlement simulator, with Jupiter taking part, and Jupiter's
// connector to it.
func startSLC(t *testing.T, now func() time.Time) (*slcsim.Sim, *slc.Connector) {
	t.Helper()
	sim := slcsim.New(slcsim.Config{Now: now, Participants: []slcsim.Participant{{Token: slcToken, TaxID: jupiterCNPJ, ISPB: jupiterISPB}}})
	srv := httptest.NewServer(sim.Handler())
	t.Cleanup(srv.Close)
	connector, err := slc.New(slc.Config{BaseURL: srv.URL, Token: slcToken})
	if err != nil {
		t.Fatal(err)
	}
	return sim, connector
}

// laterClock is the time now, or, once moved, a moment later that runs on from there.
type laterClock struct {
	ahead atomic.Int64
}

func (c *laterClock) Now() time.Time { return time.Now().Add(time.Duration(c.ahead.Load())) }

// Until moves the clock ahead to t.
func (c *laterClock) Until(t time.Time) { c.ahead.Store(int64(time.Until(t))) }

const sellerKey = "+5511987654321"

// startPixBank runs the Pix simulator as Jupiter's bank, over mutual TLS, and Jupiter's
// connector to it.
func startPixBank(t *testing.T, pool *pgxpool.Pool) (*pixsim.Sim, *pix.Connector) {
	t.Helper()
	pki, err := mtls.NewPKI("golden-path-pix")
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := pki.Server("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := pki.Client("spiffe://jupiter/pix")
	if err != nil {
		t.Fatal(err)
	}
	var handler http.Handler
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	server.TLS = pixsim.ServerTLS(serverCert.TLS, pki.Pool())
	bank, err := pixsim.New(pixsim.Config{
		Host:    server.Listener.Addr().String(),
		Clients: []pixsim.Client{{ID: "jupiter", Secret: "golden", Name: "Jupiter Pagamentos", TaxID: "11222333000181", Keys: []string{"0b7c1a2d-3e4f-4a5b-8c6d-7e8f9a0b1c2d"}, Balance: 1_000_000_00}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler = bank.Handler()
	server.StartTLS()
	t.Cleanup(server.Close)
	bank.AddKey(pixsim.Entry{Key: sellerKey, ISPB: "30000003", Name: "Vendedora do Caminho", TaxID: "98765432100"})
	connector, err := pix.New(pix.Config{
		BaseURL: server.URL, ClientID: "jupiter", ClientSecret: "golden", Key: "0b7c1a2d-3e4f-4a5b-8c6d-7e8f9a0b1c2d",
		TLS: mtls.ClientConfig(clientCert.TLS, pki.Pool()), Livemode: true, Pool: pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	return bank, connector
}

type apiClient struct {
	t    *testing.T
	base string
	key  string
	// noIdempotencyKey is for the browser, whose body holds the card number: the key
	// derived from the body would carry it too.
	noIdempotencyKey bool
}

func (c *apiClient) post(path string, body, out any) {
	c.t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if !c.noIdempotencyKey {
		sum := sha256.Sum256(append([]byte(path), raw...))
		req.Header.Set("Idempotency-Key", hex.EncodeToString(sum[:]))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("POST %s = %d %s", path, resp.StatusCode, respBody)
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		c.t.Fatal(err)
	}
}

func (c *apiClient) get(path string, out any) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodGet, c.base+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("GET %s = %d %s", path, resp.StatusCode, respBody)
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		c.t.Fatal(err)
	}
}

func merchantOf(t *testing.T, merchants *merchant.Service, q *pgxpool.Pool, key string) id.ID {
	t.Helper()
	p, err := merchants.Authenticate(t.Context(), q, key)
	if err != nil {
		t.Fatal(err)
	}
	return p.Merchant
}

func balance(t *testing.T, s *payments.Service, q *pgxpool.Pool, owner payments.Owner) (posted, held int64) {
	t.Helper()
	b, err := s.MerchantBalance(t.Context(), q, owner, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := b.Posted()
	if err != nil {
		t.Fatal(err)
	}
	return p.Minor(), b.PendingCredits.Minor()
}
