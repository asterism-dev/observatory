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

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
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
	Code   string
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

// do sends body as JSON with the extra headers hdr and decodes a 2xx response into into (a struct pointer, *json.RawMessage or nil).
func (c *cli) do(method, path, bearer string, hdr http.Header, body, into any) error {
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
	for k, v := range hdr {
		req.Header[k] = v
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
		// The hub answers {"error"}, Connect services {"code","message"}.
		var e struct{ Error, Code, Message string }
		if json.Unmarshal(b, &e) == nil && e.Error == "" {
			e.Error = e.Message
		}
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b[:min(len(b), 512)]))
		}
		return &apiError{resp.StatusCode, e.Error, e.Code}
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
	return c.do(method, path, tok, nil, body, into)
}

const iamService = "/api/iam/v1/observatory.iam.v1.IAMService/"

// Without it the iam transcoder takes a JSON POST for REST and finds no route.
var connectHeader = http.Header{"Connect-Protocol-Version": {"1"}}

func (c *cli) rpc(method string, req, resp proto.Message) error {
	return c.connectCall(iamService+method, req, resp)
}

// connectCall calls the Connect procedure at path with Connect's JSON protocol.
func (c *cli) connectCall(path string, req, resp proto.Message) error {
	body, err := protojson.Marshal(req)
	if err != nil {
		return err
	}
	tok, err := c.token()
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.do(http.MethodPost, path, tok, connectHeader, json.RawMessage(body), &raw); err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, resp)
}

// pj is the protojson form --json prints.
func pj(m proto.Message) json.RawMessage {
	b, _ := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(m)
	return b
}

func (c *cli) me() (*iamv1.GetMeResponse, error) {
	me := &iamv1.GetMeResponse{}
	return me, c.rpc("GetMe", &iamv1.GetMeRequest{}, me)
}

func findOrg(orgs []*iamv1.OrgInfo, key string) (*iamv1.OrgInfo, error) {
	for _, o := range orgs {
		if o.GetId() == key {
			return o, nil
		}
	}
	var ids []string
	var found *iamv1.OrgInfo
	for _, o := range orgs {
		if o.GetName() == key {
			found, ids = o, append(ids, o.GetId())
		}
	}
	switch len(ids) {
	case 0:
		return found, notFoundf("org %q not found", key)
	case 1:
		return found, nil
	}
	return nil, usagef("several orgs are named %q, pass the ID: %s", key, strings.Join(ids, ", "))
}

// pickOrg applies --org > OBSERVATORY_ORG > config > the only org.
func (c *cli) pickOrg(me *iamv1.GetMeResponse) (*iamv1.OrgInfo, error) {
	for _, key := range []string{c.g.org, c.getenv("OBSERVATORY_ORG"), c.cfg.Org} {
		if key != "" {
			return findOrg(me.GetOrgs(), key)
		}
	}
	switch len(me.GetOrgs()) {
	case 0:
		return nil, errors.New("you are not a member of any org, run observatory org create <name>")
	case 1:
		return me.GetOrgs()[0], nil
	}
	names := make([]string, len(me.GetOrgs()))
	for i, o := range me.GetOrgs() {
		names[i] = o.GetName()
	}
	return nil, usagef("several orgs; pass --org or run observatory org use <org>: %s", strings.Join(names, ", "))
}

func (c *cli) currentOrg() (*iamv1.OrgInfo, error) {
	if c.org != nil {
		return c.org, nil
	}
	me, err := c.me()
	if err != nil {
		return nil, err
	}
	o, err := c.pickOrg(me)
	if err != nil {
		return nil, err
	}
	c.org, c.userID = o, me.GetUser().GetId()
	return o, nil
}

func (c *cli) orgPath(rest string) (string, error) {
	o, err := c.currentOrg()
	if err != nil {
		return "", err
	}
	return "/api/orgs/" + o.GetId() + rest, nil
}
