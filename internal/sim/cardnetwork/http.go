package cardnetwork

import (
	"net/http"
	"time"
)

// Handler serves the network's files to acquirers, and an operator's control:
//
//	GET  /v1/acquirers/{acquirer}/clearing/{YYYY-MM-DD}  a closed day's clearing file
//	POST /admin/close-day?date=YYYY-MM-DD                close a business day now
//	POST /v1/tokens                                      provision a network token for a card
//	POST /v1/tokens/{reference}/cryptograms              a one-time cryptogram for a payment
//	POST /admin/cards/replace                            replace a card: its tokens follow
//	POST /admin/tokens/{reference}/suspend               suspend a token
func (n *Network) Handler() http.Handler {
	mux := http.NewServeMux()
	n.tokenRoutes(mux)
	mux.HandleFunc("GET /v1/acquirers/{acquirer}/clearing/{date}", func(w http.ResponseWriter, r *http.Request) {
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
		_, _ = w.Write(file)
	})
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
	return mux
}
