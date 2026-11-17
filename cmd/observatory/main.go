package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const defaultHub = "http://localhost:8090"

type credentials struct {
	Hub   string `json:"hub"`
	Token string `json:"token"`
}

func credentialsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "observatory", "credentials.json"), nil
}

func saveCredentials(path string, c credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	// WriteFile keeps the mode of an existing file, so chmod explicitly.
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func loadCredentials(path string) (credentials, error) {
	var c credentials
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: observatory login [--hub URL] | whoami")
		os.Exit(2)
	}
	if err := run(context.Background(), os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string, args []string) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	saved, loadErr := loadCredentials(path)
	switch cmd {
	case "login":
		// A corrupt credentials file must not block logging in again, which overwrites it.
		flags := flag.NewFlagSet("login", flag.ContinueOnError)
		hub := flags.String("hub", firstNonEmpty(saved.Hub, defaultHub), "hub URL")
		if err := flags.Parse(args); err != nil {
			return err
		}
		return login(ctx, strings.TrimRight(*hub, "/"), path, os.Stdout)
	case "whoami":
		if loadErr != nil && !errors.Is(loadErr, fs.ErrNotExist) {
			return loadErr
		}
		if saved.Token == "" {
			return errors.New("not logged in, run observatory login")
		}
		return whoami(ctx, saved, os.Stdout)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func login(ctx context.Context, hub, credsPath string, out io.Writer) error {
	var cfg struct {
		Issuer      string `json:"issuer"`
		CLIClientID string `json:"cliClientId"`
	}
	if err := hubJSON(ctx, http.MethodGet, hub+"/api/auth/config", "", nil, &cfg); err != nil {
		return fmt.Errorf("fetch hub config: %w", err)
	}

	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return fmt.Errorf("discover issuer: %w", err)
	}
	endpoint := provider.Endpoint()
	if endpoint.DeviceAuthURL == "" {
		var claims struct {
			DeviceAuthURL string `json:"device_authorization_endpoint"`
		}
		if err := provider.Claims(&claims); err != nil {
			return err
		}
		endpoint.DeviceAuthURL = claims.DeviceAuthURL
	}
	if endpoint.DeviceAuthURL == "" {
		return errors.New("issuer does not support the device flow")
	}
	oc := oauth2.Config{
		ClientID: cfg.CLIClientID,
		Endpoint: endpoint,
		Scopes:   []string{oidc.ScopeOpenID, "profile", "email"},
	}

	da, err := oc.DeviceAuth(ctx)
	if err != nil {
		return fmt.Errorf("start device flow: %w", err)
	}
	fmt.Fprintf(out, "Open %s\nCode: %s\n", firstNonEmpty(da.VerificationURIComplete, da.VerificationURI), da.UserCode)

	token, err := oc.DeviceAccessToken(ctx, da)
	if err != nil {
		return fmt.Errorf("device authorization: %w", err)
	}
	idToken, _ := token.Extra("id_token").(string)
	if idToken == "" {
		return errors.New("issuer returned no id_token")
	}

	var res struct {
		Token string `json:"token"`
	}
	body := map[string]string{"idToken": idToken, "accessToken": token.AccessToken}
	if err := hubJSON(ctx, http.MethodPost, hub+"/api/auth/cli-token", "", body, &res); err != nil {
		return fmt.Errorf("exchange token: %w", err)
	}
	if err := saveCredentials(credsPath, credentials{Hub: hub, Token: res.Token}); err != nil {
		return err
	}
	fmt.Fprintln(out, "Logged in. Run observatory whoami to verify.")
	return nil
}

var errUnauthorized = errors.New("unauthorized")

func hubJSON(ctx context.Context, method, url, bearer string, body, into any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	cl := http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

func whoami(ctx context.Context, c credentials, out io.Writer) error {
	var me struct {
		User struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"user"`
		Orgs []struct {
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"orgs"`
	}
	if err := hubJSON(ctx, http.MethodGet, strings.TrimRight(c.Hub, "/")+"/api/me", c.Token, nil, &me); err != nil {
		if errors.Is(err, errUnauthorized) {
			return errors.New("not logged in, run observatory login")
		}
		return err
	}
	fmt.Fprintf(out, "%s %s\n", me.User.Name, me.User.Email)
	for _, o := range me.Orgs {
		fmt.Fprintf(out, "%s (%s)\n", o.Name, o.Role)
	}
	return nil
}
