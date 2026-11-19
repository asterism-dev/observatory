package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestEnroll(t *testing.T) {
	var calls atomic.Int32
	status := http.StatusOK
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var in struct{ Token string }
		json.NewDecoder(r.Body).Decode(&in)
		if r.Method != http.MethodPost || r.URL.Path != "/api/nodes/enroll" || in.Token != "tok" {
			t.Errorf("bad request %s %s %q", r.Method, r.URL.Path, in.Token)
		}
		if status != http.StatusOK {
			http.Error(w, "nope", status)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"nodeId": "n1", "credential": "cred"})
	}))
	defer hub.Close()

	id, cred, err := Enroll(context.Background(), hub.URL, "tok", hub.Client())
	if err != nil || id != "n1" || cred != "cred" {
		t.Fatalf("got %q %q %v", id, cred, err)
	}
	for _, code := range []int{http.StatusBadRequest, http.StatusNotFound} {
		status, calls = code, atomic.Int32{}
		_, _, err := Enroll(context.Background(), hub.URL, "tok", hub.Client())
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(code)) || calls.Load() != 1 {
			t.Fatalf("status %d: err %v, calls %d", code, err, calls.Load())
		}
	}
}
