// Package rest holds what the services' REST transcoding needs on top of the transcoder.
package rest

import (
	"io"
	"net/http"
	"strings"
)

// NoQuery refuses REST requests with a query string and passes Connect and gRPC requests (paths under rpcPath) through.
func NoQuery(rpcPath string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ponytail: the transcoder lets a query field (e.g. ?org=) override the path's, so all REST queries are refused; allow named fields once an RPC needs one.
		if r.URL.RawQuery != "" && !strings.HasPrefix(r.URL.Path, rpcPath) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":3,"message":"query parameters are not supported"}`)
			return
		}
		h.ServeHTTP(w, r)
	})
}
