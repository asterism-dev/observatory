package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

const defaultHub = "http://localhost:8090"

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return &exitError{2, fmt.Sprintf(format, a...)} }

func notFoundf(format string, a ...any) error { return &exitError{5, fmt.Sprintf(format, a...)} }

var errNotLoggedIn = &exitError{3, "not logged in, run observatory login"}

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return "session expired or revoked, run observatory login"
	}
	if e.Msg != "" {
		return e.Msg
	}
	return http.StatusText(e.Status)
}

func exitCode(err error) int {
	var ee *exitError
	var ae *apiError
	var xe *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.code
	case errors.As(err, &ae):
		switch ae.Status {
		case http.StatusUnauthorized:
			return 3
		case http.StatusForbidden:
			return 4
		case http.StatusNotFound:
			return 5
		}
	case errors.As(err, &xe):
		return xe.ExitCode()
	}
	return 1
}

func (c *cli) hub() string {
	for _, h := range []string{c.g.hub, c.getenv("OBSERVATORY_HUB"), c.cfg.Hub} {
		if h != "" {
			return strings.TrimRight(h, "/")
		}
	}
	return defaultHub
}

func (c *cli) token() (string, error) {
	if t := c.getenv("OBSERVATORY_TOKEN"); t != "" {
		return t, nil
	}
	if c.cfg.Token == "" {
		return "", errNotLoggedIn
	}
	if h := c.hub(); h != strings.TrimRight(cmp.Or(c.cfg.Hub, defaultHub), "/") {
		return "", &exitError{3, "not logged in to " + h + ", run observatory login --hub " + h}
	}
	if left := c.cfg.ExpiresAt.Sub(c.now()); !c.cfg.ExpiresAt.IsZero() && left > 0 && left < 72*time.Hour && !c.warnedExpiry {
		c.warnedExpiry = true
		fmt.Fprintf(c.stderr, "observatory: session expires in %d days, run observatory login\n", int(left.Round(24*time.Hour)/(24*time.Hour)))
	}
	return c.cfg.Token, nil
}

// do sends body as JSON and decodes a 2xx response into into (a struct pointer, *json.RawMessage or nil).
func (c *cli) do(method, path, bearer string, body, into any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(c.ctx, method, c.hub()+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b[:min(len(b), 512)]))
		}
		return &apiError{resp.StatusCode, e.Error}
	}
	if into == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	return json.Unmarshal(b, into)
}

func (c *cli) call(method, path string, body, into any) error {
	tok, err := c.token()
	if err != nil {
		return err
	}
	return c.do(method, path, tok, body, into)
}

type orgJSON struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Personal bool   `json:"personal"`
	Role     string `json:"role"`
}

type meJSON struct {
	User struct {
		ID            string `json:"id"`
		Email         string `json:"email"`
		Name          string `json:"name"`
		PlatformAdmin bool   `json:"platformAdmin"`
	} `json:"user"`
	Orgs []orgJSON `json:"orgs"`
}

func (c *cli) me(raw *json.RawMessage) (meJSON, error) {
	var me meJSON
	var b json.RawMessage
	if err := c.call(http.MethodGet, "/api/me", nil, &b); err != nil {
		return me, err
	}
	if raw != nil {
		*raw = b
	}
	if err := json.Unmarshal(b, &me); err != nil {
		return me, err
	}
	return me, nil
}

func findOrg(orgs []orgJSON, key string) (orgJSON, error) {
	for _, o := range orgs {
		if o.ID == key {
			return o, nil
		}
	}
	var ids []string
	var found orgJSON
	for _, o := range orgs {
		if o.Name == key {
			found, ids = o, append(ids, o.ID)
		}
	}
	switch len(ids) {
	case 0:
		return found, notFoundf("org %q not found", key)
	case 1:
		return found, nil
	}
	return orgJSON{}, usagef("several orgs are named %q, pass the ID: %s", key, strings.Join(ids, ", "))
}

// pickOrg applies --org > OBSERVATORY_ORG > config > the only org.
func (c *cli) pickOrg(me meJSON) (orgJSON, error) {
	for _, key := range []string{c.g.org, c.getenv("OBSERVATORY_ORG"), c.cfg.Org} {
		if key != "" {
			return findOrg(me.Orgs, key)
		}
	}
	switch len(me.Orgs) {
	case 0:
		return orgJSON{}, errors.New("you are not a member of any org, run observatory org create <name>")
	case 1:
		return me.Orgs[0], nil
	}
	names := make([]string, len(me.Orgs))
	for i, o := range me.Orgs {
		names[i] = o.Name
	}
	return orgJSON{}, usagef("several orgs; pass --org or run observatory org use <org>: %s", strings.Join(names, ", "))
}

func (c *cli) currentOrg() (orgJSON, error) {
	if c.org != nil {
		return *c.org, nil
	}
	me, err := c.me(nil)
	if err != nil {
		return orgJSON{}, err
	}
	o, err := c.pickOrg(me)
	if err != nil {
		return o, err
	}
	c.org, c.userID = &o, me.User.ID
	return o, nil
}

func (c *cli) orgPath(rest string) (string, error) {
	o, err := c.currentOrg()
	if err != nil {
		return "", err
	}
	return "/api/orgs/" + o.ID + rest, nil
}
