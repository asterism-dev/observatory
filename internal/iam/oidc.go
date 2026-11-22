package iam

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/asterism-dev/observatory/internal/iam/store"
)

type Authenticator interface {
	AuthCodeURL(state, verifier string, register bool) string
	Exchange(ctx context.Context, code, verifier string) (store.Identity, error)
	VerifyCLIToken(ctx context.Context, rawIDToken, accessToken string) (store.Identity, error)
}

type OIDC struct {
	provider    *oidc.Provider
	oauth       oauth2.Config
	webVerifier *oidc.IDTokenVerifier
	cliVerifier *oidc.IDTokenVerifier
	cliClientID string
}

func NewOIDC(ctx context.Context, issuer, webClientID, cliClientID, redirectURL string) (*OIDC, error) {
	if u, err := url.Parse(issuer); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("oidc issuer %q is not an http(s) URL", issuer)
	}
	provider, err := discover(ctx, func() (*oidc.Provider, error) { return oidc.NewProvider(ctx, issuer) }, time.Second, 15*time.Second)
	if err != nil {
		return nil, err
	}
	return &OIDC{
		provider: provider,
		oauth: oauth2.Config{
			ClientID:    webClientID,
			Endpoint:    provider.Endpoint(),
			RedirectURL: redirectURL,
			Scopes:      []string{oidc.ScopeOpenID, "profile", "email"},
		},
		webVerifier: provider.Verifier(&oidc.Config{ClientID: webClientID}),
		cliVerifier: provider.Verifier(&oidc.Config{ClientID: cliClientID}),
		cliClientID: cliClientID,
	}, nil
}

// discover retries fetch with doubling delays until it succeeds or ctx ends; the IdP may still be starting.
func discover(ctx context.Context, fetch func() (*oidc.Provider, error), minDelay, maxDelay time.Duration) (*oidc.Provider, error) {
	delay := minDelay
	for attempt := 1; ; attempt++ {
		p, err := fetch()
		if err == nil {
			return p, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("oidc discovery: %w", err)
		}
		if attempt == 1 || attempt%10 == 0 {
			slog.Warn("oidc discovery failed, retrying", "attempt", attempt, "retry_in", delay, "err", err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("oidc discovery: %w", err)
		case <-time.After(delay):
		}
		delay = min(delay*2, maxDelay)
	}
}

func (o *OIDC) AuthCodeURL(state, verifier string, register bool) string {
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)}
	if register {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "create"))
	}
	return o.oauth.AuthCodeURL(state, opts...)
}

func (o *OIDC) Exchange(ctx context.Context, code, verifier string) (store.Identity, error) {
	tok, err := o.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return store.Identity{}, err
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return store.Identity{}, errors.New("token response has no id_token")
	}
	return o.verify(ctx, o.webVerifier, raw, tok, "")
}

func (o *OIDC) VerifyCLIToken(ctx context.Context, rawIDToken, accessToken string) (store.Identity, error) {
	return o.verify(ctx, o.cliVerifier, rawIDToken, &oauth2.Token{AccessToken: accessToken, TokenType: "Bearer"}, o.cliClientID)
}

// verify checks the ID token; a non-empty azpClientID additionally requires the token to be issued to that client.
func (o *OIDC) verify(ctx context.Context, v *oidc.IDTokenVerifier, rawIDToken string, tok *oauth2.Token, azpClientID string) (store.Identity, error) {
	idt, err := v.Verify(ctx, rawIDToken)
	if err != nil {
		return store.Identity{}, err
	}
	var c idClaims
	if err := idt.Claims(&c); err != nil {
		return store.Identity{}, err
	}
	if azpClientID != "" {
		if err := checkAZP(idt.Audience, c.AZP, azpClientID); err != nil {
			return store.Identity{}, err
		}
	}
	return identityFrom(c, func() (idClaims, error) {
		ui, err := o.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok))
		if err != nil {
			return idClaims{}, fmt.Errorf("userinfo: %w", err)
		}
		var uc idClaims
		err = ui.Claims(&uc)
		return uc, err
	})
}

type idClaims struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	AZP     string `json:"azp"`
}

// checkAZP rejects tokens issued to another client that merely lists clientID among several audiences.
func checkAZP(aud []string, azp, clientID string) error {
	if azp != "" && azp != clientID {
		return errors.New("id token issued to another client")
	}
	if azp == "" && len(aud) > 1 {
		return errors.New("id token has several audiences but no azp")
	}
	return nil
}

// identityFrom builds the identity from ID token claims, filling a missing email or name from userinfo.
func identityFrom(claims idClaims, userinfo func() (idClaims, error)) (store.Identity, error) {
	if claims.Email == "" || claims.Name == "" {
		ui, err := userinfo()
		if err != nil {
			return store.Identity{}, err
		}
		if ui.Subject != claims.Subject {
			return store.Identity{}, errors.New("userinfo subject does not match id token")
		}
		if claims.Email == "" {
			claims.Email = ui.Email
		}
		if claims.Name == "" {
			claims.Name = ui.Name
		}
	}
	if claims.Subject == "" || claims.Email == "" {
		return store.Identity{}, errors.New("identity lacks sub or email")
	}
	return store.Identity{Subject: claims.Subject, Email: claims.Email, Name: claims.Name}, nil
}
