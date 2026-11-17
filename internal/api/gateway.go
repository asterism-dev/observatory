package api

import (
	"net/http"
	"strings"

	"github.com/asterism-dev/observatory/internal/network"
	"github.com/asterism-dev/observatory/internal/proto"
)

// GatewayEndpoints registers the routes the gateway calls; they authenticate with the shared gateway token, not sessions.
func GatewayEndpoints(mux *http.ServeMux, n *network.Hub) {
	mux.HandleFunc("GET /api/gateway/state", gatewayAuth(n, func(w http.ResponseWriter, r *http.Request) {
		st, err := n.State(r.Context())
		if err != nil {
			serverError(w, "gateway state", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, st)
	}))
	mux.HandleFunc("POST /api/gateway/status", gatewayAuth(n, func(w http.ResponseWriter, r *http.Request) {
		var st proto.GatewayStatus
		if !readJSON(w, r, &st) {
			return
		}
		if err := n.SetStatus(r.Context(), st); err != nil {
			serverError(w, "gateway status", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func gatewayAuth(n *network.Hub, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !n.TokenOK(token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}
