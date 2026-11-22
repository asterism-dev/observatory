package iam

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

func TestDiscoverRetriesUntilIdPIsUp(t *testing.T) {
	calls := 0
	want := &oidc.Provider{}
	p, err := discover(context.Background(), func() (*oidc.Provider, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("404")
		}
		return want, nil
	}, time.Millisecond, 2*time.Millisecond)
	if err != nil || p != want || calls != 3 {
		t.Fatalf("p=%v err=%v calls=%d", p, err, calls)
	}
}

func TestDiscoverStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := discover(ctx, func() (*oidc.Provider, error) { return nil, errors.New("down") }, time.Millisecond, 2*time.Millisecond)
	if err == nil {
		t.Fatal("want error")
	}
}

func TestNewOIDCRejectsMalformedIssuer(t *testing.T) {
	for _, issuer := range []string{"", "zitadel:8080", "ftp://x", "http://"} {
		if _, err := NewOIDC(context.Background(), issuer, "w", "c", "http://h/auth/callback"); err == nil {
			t.Errorf("issuer %q accepted", issuer)
		}
	}
}
