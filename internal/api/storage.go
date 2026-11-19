package api

import (
	"net/http"
	"strings"

	"github.com/asterism-dev/observatory/internal/disks"
	"github.com/asterism-dev/observatory/internal/proto"
)

// StorageEndpoints registers the routes the storage server calls; they authenticate with the storage token, not sessions.
func StorageEndpoints(mux *http.ServeMux, h *disks.Hub) {
	mux.HandleFunc("GET /api/storage/state", storageAuth(h, func(w http.ResponseWriter, r *http.Request) {
		st, err := h.State(r.Context())
		if err != nil {
			serverError(w, "storage state", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, st)
	}))
	mux.HandleFunc("POST /api/storage/status", storageAuth(h, func(w http.ResponseWriter, r *http.Request) {
		var st proto.StorageStatus
		if !readJSON(w, r, &st) {
			return
		}
		if err := h.SetStatus(r.Context(), st); err != nil {
			serverError(w, "storage status", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func storageAuth(h *disks.Hub, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !h.TokenOK(token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}
