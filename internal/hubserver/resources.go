package hubserver

import (
	"net/http"

	"github.com/asterism-dev/observatory/gen/go/observatory/hub/v1/hubv1connect"
)

// internalHub serves /internal/hub/*; the caller wraps it in servicetoken.Middleware.
func internalHub(rs hubv1connect.ResourceServiceHandler) http.Handler {
	path, h := hubv1connect.NewResourceServiceHandler(rs)
	mux := http.NewServeMux()
	mux.Handle("/internal/hub"+path, http.StripPrefix("/internal/hub", h))
	return mux
}
