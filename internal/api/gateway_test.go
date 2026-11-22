package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

func gatewayMux(e *env) *http.ServeMux {
	mux := http.NewServeMux()
	GatewayEndpoints(mux, e.net)
	return mux
}

func gatewayDo(mux *http.ServeMux, method, path, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestGatewayStateRequiresToken(t *testing.T) {
	e := newEnv(t)
	mux := gatewayMux(e)
	for _, auth := range []string{"", "Bearer wrong", "Bearer " + gatewayToken[:10], gatewayToken} {
		if rec := gatewayDo(mux, "GET", "/api/gateway/state", auth, ""); rec.Code != 401 {
			t.Fatalf("%q: %d", auth, rec.Code)
		}
	}
	rec := gatewayDo(mux, "GET", "/api/gateway/state", "Bearer "+gatewayToken, "")
	var st proto.GatewayState
	decode(t, rec, &st)
	if rec.Code != 200 || st.UnderlayKey == "" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
}

func TestGatewayStatusSetsHandshakes(t *testing.T) {
	e := newEnv(t)
	u, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	d, err := e.store.CreateDevice(context.Background(), store.Device{OrgID: org, UserID: u.ID, Name: "laptop", PublicKey: key})
	if err != nil {
		t.Fatal(err)
	}
	mux := gatewayMux(e)
	at := time.Now().UTC().Truncate(time.Second)
	body, _ := json.Marshal(proto.GatewayStatus{Handshakes: map[string]time.Time{key: at}})
	if rec := gatewayDo(mux, "POST", "/api/gateway/status", "", string(body)); rec.Code != 401 {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := gatewayDo(mux, "POST", "/api/gateway/status", "Bearer "+gatewayToken, string(body)); rec.Code != 204 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got, err := e.store.Device(context.Background(), org, d.ID)
	if err != nil || got.LastHandshake == nil || !got.LastHandshake.Equal(at) {
		t.Fatalf("%+v %v", got, err)
	}
}
