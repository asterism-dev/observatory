package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv1 "github.com/asterism-dev/observatory/gen/go/observatory/audit/v1"
	"github.com/asterism-dev/observatory/internal/version"
)

func cmdLogin(c *cli, args []string) error {
	if _, err := c.parse(c.flags("login"), args, 0, 0); err != nil {
		return err
	}
	var hubCfg struct {
		Issuer      string `json:"issuer"`
		CLIClientID string `json:"cliClientId"`
	}
	if err := c.do(http.MethodGet, "/api/auth/config", "", nil, &hubCfg); err != nil {
		return fmt.Errorf("fetch hub config: %w", err)
	}

	provider, err := oidc.NewProvider(c.ctx, hubCfg.Issuer)
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
		ClientID: hubCfg.CLIClientID,
		Endpoint: endpoint,
		Scopes:   []string{oidc.ScopeOpenID, "profile", "email"},
	}

	da, err := oc.DeviceAuth(c.ctx)
	if err != nil {
		return fmt.Errorf("start device flow: %w", err)
	}
	verify := da.VerificationURIComplete
	if verify == "" {
		verify = da.VerificationURI
	}
	fmt.Fprintf(c.stderr, "Open %s\nCode: %s\n", verify, da.UserCode)

	token, err := oc.DeviceAccessToken(c.ctx, da)
	if err != nil {
		return fmt.Errorf("device authorization: %w", err)
	}
	idToken, _ := token.Extra("id_token").(string)
	if idToken == "" {
		return errors.New("issuer returned no id_token")
	}

	var res struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	body := map[string]string{"idToken": idToken, "accessToken": token.AccessToken}
	if err := c.do(http.MethodPost, "/api/auth/cli-token", "", body, &res); err != nil {
		if ae := (*apiError)(nil); errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			return errors.New("hub refused the login: " + cmp.Or(ae.Msg, "unauthorized"))
		}
		return fmt.Errorf("exchange token: %w", err)
	}
	cfg := c.cfg
	cfg.Hub, cfg.Token, cfg.ExpiresAt = c.hub(), res.Token, res.ExpiresAt
	if err := saveConfig(c.cfgDir, cfg); err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, "Logged in. Run observatory whoami to verify.")
	return nil
}

func cmdLogout(c *cli, args []string) error {
	if _, err := c.parse(c.flags("logout"), args, 0, 0); err != nil {
		return err
	}
	var ae *apiError
	// A 401 means the session is already gone, which is what logout wants.
	if err := c.call(http.MethodPost, "/auth/logout", nil, nil); err != nil && !(errors.As(err, &ae) && ae.Status == http.StatusUnauthorized) {
		return err
	}
	if c.getenv("OBSERVATORY_TOKEN") != "" {
		fmt.Fprintln(c.stderr, "observatory: logged out the OBSERVATORY_TOKEN session; unset it to log out the stored session")
		return c.done(nil, "Logged out.")
	}
	c.cfg.Token, c.cfg.ExpiresAt = "", time.Time{}
	if err := saveConfig(c.cfgDir, c.cfg); err != nil {
		return err
	}
	return c.done(nil, "Logged out.")
}

func cmdWhoami(c *cli, args []string) error {
	if _, err := c.parse(c.flags("whoami"), args, 0, 0); err != nil {
		return err
	}
	var raw json.RawMessage
	me, err := c.me(&raw)
	if err != nil {
		return err
	}
	if c.g.json {
		return c.printJSON(raw)
	}
	pairs := [][2]string{{"user", me.User.Name}, {"email", me.User.Email}}
	if me.User.PlatformAdmin {
		pairs = append(pairs, [2]string{"platform admin", "yes"})
	}
	if c.getenv("OBSERVATORY_TOKEN") != "" {
		pairs = append(pairs, [2]string{"session", "from OBSERVATORY_TOKEN"})
	} else if !c.cfg.ExpiresAt.IsZero() {
		pairs = append(pairs, [2]string{"session expires", c.cfg.ExpiresAt.In(c.now().Location()).Format("2006-01-02 15:04")})
	}
	if err := c.emitKV(nil, pairs); err != nil {
		return err
	}
	fmt.Fprintln(c.stdout)
	return c.emit(nil, []string{"", "org", "id", "role"}, orgRows(c, me))
}

// orgRows marks the current org with "*"; with no org chosen none is marked.
func orgRows(c *cli, me meJSON) [][]string {
	cur, _ := c.pickOrg(me)
	rows := make([][]string, 0, len(me.Orgs))
	for _, o := range me.Orgs {
		mark := ""
		if o.ID == cur.ID {
			mark = "*"
		}
		name := o.Name
		if o.Personal {
			name += " (personal)"
		}
		rows = append(rows, []string{mark, name, o.ID, o.Role})
	}
	return rows
}

func cmdVersion(c *cli, args []string) error {
	if _, err := c.parse(c.flags("version"), args, 0, 0); err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, version.Version)
	return nil
}

func cmdOrgList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("org list"), args, 0, 0); err != nil {
		return err
	}
	var raw json.RawMessage
	me, err := c.me(&raw)
	if err != nil {
		return err
	}
	return c.emit(raw, []string{"", "name", "id", "role"}, orgRows(c, me))
}

func cmdOrgUse(c *cli, args []string) error {
	pos, err := c.parse(c.flags("org use"), args, 1, 1)
	if err != nil {
		return err
	}
	me, err := c.me(nil)
	if err != nil {
		return err
	}
	o, err := findOrg(me.Orgs, pos[0])
	if err != nil {
		return err
	}
	c.cfg.Org = o.ID
	if err := saveConfig(c.cfgDir, c.cfg); err != nil {
		return err
	}
	return c.done(nil, "Default org is now %s.", o.Name)
}

func cmdOrgShow(c *cli, args []string) error {
	if _, err := c.parse(c.flags("org show"), args, 0, 0); err != nil {
		return err
	}
	path, err := c.orgPath("")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	var o struct {
		orgJSON
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return err
	}
	return c.emitKV(raw, [][2]string{
		{"name", o.Name}, {"id", o.ID}, {"role", o.Role},
		{"personal", strconv.FormatBool(o.Personal)},
		{"permissions", strings.Join(o.Permissions, ", ")},
	})
}

func cmdOrgCreate(c *cli, args []string) error {
	pos, err := c.parse(c.flags("org create"), args, 1, 1)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, "/api/orgs", map[string]string{"name": pos[0]}, &raw); err != nil {
		return err
	}
	var o orgJSON
	if err := json.Unmarshal(raw, &o); err != nil {
		return err
	}
	return c.done(raw, "Org %s created (%s).", o.Name, o.ID)
}

func cmdOrgRename(c *cli, args []string) error {
	pos, err := c.parse(c.flags("org rename"), args, 1, 1)
	if err != nil {
		return err
	}
	path, err := c.orgPath("")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPatch, path, map[string]string{"name": pos[0]}, &raw); err != nil {
		return err
	}
	return c.done(raw, "Org renamed to %s.", pos[0])
}

func cmdOrgAudit(c *cli, args []string) error {
	fs := c.flags("org audit")
	limit := fs.Int("limit", 0, "at most `N` entries (default 50, max 200)")
	action := fs.String("action", "", "only this action, e.g. compute.instances.delete")
	actor := fs.String("actor", "", "only this actor `ID`")
	since := fs.Duration("since", 0, "only entries newer than this, e.g. 24h")
	if _, err := c.parse(fs, args, 0, 0); err != nil {
		return err
	}
	o, err := c.currentOrg()
	if err != nil {
		return err
	}
	req := &auditv1.ListEventsRequest{Org: o.ID, PageSize: int32(min(max(*limit, 0), 200)), Filter: &auditv1.EventFilter{Type: *action, ActorId: *actor}}
	if *since > 0 {
		req.Filter.From = timestamppb.New(c.now().Add(-*since))
	}
	body, err := protojson.Marshal(req)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, "/api/audit/v1/observatory.audit.v1.AuditService/ListEvents", json.RawMessage(body), &raw); err != nil {
		return err
	}
	var resp auditv1.ListEventsResponse
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, &resp); err != nil {
		return err
	}
	rows := make([][]string, 0, len(resp.GetEvents()))
	for _, e := range resp.GetEvents() {
		actor := e.GetActor().GetId()
		if actor == "" {
			actor = strings.ToLower(strings.TrimPrefix(e.GetActor().GetKind().String(), "ACTOR_KIND_"))
		}
		rows = append(rows, []string{ago(c.now(), e.GetTime().AsTime()), actor, e.GetType(), e.GetResource(), strings.ToLower(strings.TrimPrefix(e.GetResult().String(), "RESULT_"))})
	}
	return c.emit(raw, []string{"time", "actor", "action", "resource", "result"}, rows)
}
