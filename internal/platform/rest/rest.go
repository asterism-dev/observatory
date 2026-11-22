// Package rest holds what the services' REST transcoding needs on top of the transcoder.
package rest

import (
	"io"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"connectrpc.com/vanguard"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
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

// Codec is the transcoder's JSON codec, answering an undecodable REST body with invalid_argument (400) instead of unknown.
func Codec(res vanguard.TypeResolver) vanguard.Codec { return badBody{vanguard.NewJSONCodec(res)} }

type badBody struct{ *vanguard.JSONCodec }

func (c badBody) Unmarshal(data []byte, msg proto.Message) error {
	return invalid(c.JSONCodec.Unmarshal(data, msg))
}

func (c badBody) UnmarshalField(data []byte, msg proto.Message, field protoreflect.FieldDescriptor) error {
	return invalid(c.JSONCodec.UnmarshalField(data, msg, field))
}

func invalid(err error) error {
	if err == nil {
		return nil
	}
	return connect.NewError(connect.CodeInvalidArgument, err)
}
