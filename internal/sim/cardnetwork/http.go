package cardnetwork

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

// Handler serves the network's files to acquirers, and an operator's control:
//
//	GET  /v1/acquirers/{acquirer}/clearing/{YYYY-MM-DD}  a closed day's clearing file, signed
//	POST /admin/close-day?date=YYYY-MM-DD                close a business day now
//	POST /v1/tokens                                      provision a network token for a card
//	POST /v1/tokens/{reference}/cryptograms              a one-time cryptogram for a payment
//	POST /admin/cards/replace                            replace a card: its tokens follow
//	POST /admin/tokens/{reference}/suspend               suspend a token
//
// and disputes, as disputeRoutes lists them. The operator's controls (/admin/) answer on
// loopback only, to JSON requests, so no web page can drive them.
func (n *Network) Handler() http.Handler {
	mux := http.NewServeMux()
	n.tokenRoutes(mux)
	n.disputeRoutes(mux)
	mux.HandleFunc("GET /v1/acquirers/{acquirer}/clearing/{date}", n.acquirerOnly(func(w http.ResponseWriter, r *http.Request) {
		date, err := time.Parse(time.DateOnly, r.PathValue("date"))
		if err != nil {
			http.Error(w, "the date must be YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		file, ok := n.ClearingFile(r.PathValue("acquirer"), date)
		if !ok {
			http.Error(w, "no clearing file for that acquirer and day", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=us-ascii")
		if n.cfg.ClearingSecret != "" {
			w.Header().Set(cardnet.EventSignatureHeader, cardnet.SignEvent(file, n.cfg.ClearingSecret, n.cfg.Now()))
		}
		_, _ = w.Write(file)
	}))
	mux.HandleFunc("POST /admin/close-day", func(w http.ResponseWriter, r *http.Request) {
		date := n.cfg.Now().UTC()
		if s := r.URL.Query().Get("date"); s != "" {
			parsed, err := time.Parse(time.DateOnly, s)
			if err != nil {
				http.Error(w, "the date must be YYYY-MM-DD", http.StatusBadRequest)
				return
			}
			date = parsed
		}
		if err := n.CloseDay(date); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return adminOnLoopback(mux)
}

// adminOnLoopback lets requests to /admin/ through only when addressed to this machine
// and, for a POST, sent as JSON: a browser cannot send one from a web page without asking
// first, and a rebound DNS name does not pass for loopback.
func adminOnLoopback(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/admin/") {
			next.ServeHTTP(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		// The Host header is the caller's to write: the connection must come from this
		// machine too.
		peer, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); (host != "localhost" && (ip == nil || !ip.IsLoopback())) || !net.ParseIP(peer).IsLoopback() {
			http.Error(w, "the admin controls answer on loopback only", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "a JSON request is required", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}
