package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type integrationJSON struct {
	ID                string     `json:"id"`
	Kind              string     `json:"kind"`
	Source            string     `json:"source"`
	Scope             string     `json:"scope"`
	TeamID            string     `json:"teamId,omitempty"`
	TeamName          string     `json:"teamName,omitempty"`
	Mode              string     `json:"mode"`
	Account           string     `json:"account"`
	Scopes            string     `json:"scopes"`
	QuotaProject      string     `json:"quotaProject,omitempty"`
	ProviderExpiresAt *time.Time `json:"providerExpiresAt"`
	Status            string     `json:"status"`
	LastError         string     `json:"lastError"`
	WasInVM           bool       `json:"wasInVm"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

func (v integrationJSON) scopeLabel() string { return scopeLabel(v.Scope, v.TeamName) }

const rotateHint = "The token was inside VMs; rotate it at the provider."

var integrationKinds = map[string]struct{ source, title string }{
	"github": {"pat", "GitHub"},
	"linear": {"api-key", "Linear"},
	"claude": {"setup-token", "Claude"},
	"gcloud": {"adc", "gcloud"},
}

func adcPath() string {
	if dir := os.Getenv("CLOUDSDK_CONFIG"); dir != "" {
		return filepath.Join(dir, "application_default_credentials.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
}

func defaultSource(kind string) (string, error) {
	k, ok := integrationKinds[kind]
	if !ok {
		return "", usagef("unknown integration %q: use github, linear, claude or gcloud", kind)
	}
	return k.source, nil
}

func checkMode(mode string) error {
	if mode != "proxy" && mode != "vm" {
		return usagef("unknown mode %q: use proxy or vm", mode)
	}
	return nil
}

func integrationCommands() []command {
	return []command{
		{"integration list", "", "list integrations", cmdIntegrationList},
		{"integration connect", "github|linear|claude|gcloud [--scope S] [--team T] [--mode proxy|vm] [--from-file F] [--quota-project P]", "connect an integration (token from stdin or --from-file)", cmdIntegrationConnect},
		{"integration sign-in", "claude [--scope S] [--team T] [--mode proxy|vm] [--replace]", "connect Claude by signing in with your browser", cmdIntegrationSignIn},
		{"integration replace", "<kind> [--scope S] [--team T] [--from-file F]", "replace an integration's token (from stdin or --from-file)", cmdIntegrationReplace},
		{"integration mode", "<kind> proxy|vm [--scope S] [--team T]", "change how the token reaches VMs", cmdIntegrationMode},
		{"integration disconnect", "<kind> [--scope S] [--team T]", "disconnect an integration", cmdIntegrationDisconnect},
	}
}

func (c *cli) integrationList() ([]integrationJSON, json.RawMessage, error) {
	path, err := c.orgPath("/integrations")
	if err != nil {
		return nil, nil, err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return nil, nil, err
	}
	var l struct{ Integrations []integrationJSON }
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, nil, err
	}
	return l.Integrations, raw, nil
}

func (c *cli) findIntegration(kind string, s scopeFlags) (integrationJSON, error) {
	if _, err := defaultSource(kind); err != nil {
		return integrationJSON{}, err
	}
	apiScope, teamID, err := c.resolveScope(s)
	if err != nil {
		return integrationJSON{}, err
	}
	l, _, err := c.integrationList()
	if err != nil {
		return integrationJSON{}, err
	}
	for _, v := range l {
		if v.Kind == kind && v.Scope == apiScope && (teamID == nil || v.TeamID == *teamID) {
			return v, nil
		}
	}
	return integrationJSON{}, notFoundf("integration %s not found (%s)", kind, scopeLabel(apiScope, s.team))
}

func integrationStatus(now time.Time, v integrationJSON) string {
	if v.Kind == "gcloud" && v.Status == "expired" {
		return "expired: run gcloud auth application-default login, then observatory integration connect gcloud"
	}
	if v.Status != "ok" || v.ProviderExpiresAt == nil {
		return v.Status
	}
	days := max(0, int(math.Ceil(v.ProviderExpiresAt.Sub(now).Hours()/24)))
	return fmt.Sprintf("ok · expires in %d days", days)
}

func cmdIntegrationList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("integration list"), args, 0, 0); err != nil {
		return err
	}
	l, raw, err := c.integrationList()
	if err != nil {
		return err
	}
	var rows [][]string
	for _, v := range l {
		account := v.Account
		if account == "" {
			account = "—"
		}
		if v.QuotaProject != "" {
			account += " (quota project " + v.QuotaProject + ")"
		}
		rows = append(rows, []string{v.Kind, v.scopeLabel(), account, v.Mode, integrationStatus(c.now(), v)})
	}
	return c.emit(raw, []string{"kind", "scope", "account", "delivery", "status"}, rows)
}

func cmdIntegrationConnect(c *cli, args []string) error {
	fs := c.flags("integration connect")
	var s scopeFlags
	addScopeFlags(fs, &s, "scope")
	mode := fs.String("mode", "proxy", "how the token reaches VMs: proxy|vm")
	fromFile := fs.String("from-file", "", "read the token from file `F` (- = stdin)")
	quotaProject := fs.String("quota-project", "", "gcloud quota project `P` (default: from the login file)")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	kind := pos[0]
	if kind == "gcloud" {
		return c.connectGcloud(s, *mode, *fromFile, *quotaProject)
	}
	source, err := defaultSource(kind)
	if err != nil {
		return err
	}
	if err := checkMode(*mode); err != nil {
		return err
	}
	apiScope, teamID, err := c.resolveScope(s)
	if err != nil {
		return err
	}
	path, err := c.orgPath("/integrations")
	if err != nil {
		return err
	}
	token, err := c.secret(integrationKinds[kind].title+" token: ", *fromFile)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, path, struct {
		Kind   string  `json:"kind"`
		Source string  `json:"source"`
		Scope  string  `json:"scope"`
		TeamID *string `json:"teamId,omitempty"`
		Mode   string  `json:"mode"`
		Token  string  `json:"token"`
	}{kind, source, apiScope, teamID, *mode, token}, &raw); err != nil {
		return err
	}
	var res struct{ Account string }
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	return c.done(raw, "Connected %s as %s (%s, %s).", kind, res.Account, scopeLabel(apiScope, s.team), *mode)
}

func (c *cli) connectGcloud(s scopeFlags, mode, fromFile, quotaProject string) error {
	if s.scope != "private" || s.team != "" || mode != "proxy" {
		return usagef("gcloud is only available as a private integration in proxy mode")
	}
	if fromFile == "" {
		fromFile = adcPath()
		if _, err := os.Stat(fromFile); err != nil {
			return fmt.Errorf("no gcloud login found at %s: run gcloud auth application-default login", fromFile)
		}
	}
	token, err := c.secret("", fromFile)
	if err != nil {
		return err
	}
	var qp *string
	if quotaProject != "" {
		qp = &quotaProject
	}
	var raw json.RawMessage
	existing, err := c.findIntegration("gcloud", s)
	var nf *exitError
	switch {
	case err == nil:
		path, perr := c.orgPath("/integrations/" + existing.ID + "/token")
		if perr != nil {
			return perr
		}
		err = c.call(http.MethodPut, path, struct {
			Token        string  `json:"token"`
			Source       string  `json:"source"`
			QuotaProject *string `json:"quotaProject,omitempty"`
		}{token, "adc", qp}, &raw)
		if qp == nil {
			quotaProject = existing.QuotaProject
		}
	case errors.As(err, &nf) && nf.code == 5:
		path, perr := c.orgPath("/integrations")
		if perr != nil {
			return perr
		}
		err = c.call(http.MethodPost, path, struct {
			Kind         string  `json:"kind"`
			Source       string  `json:"source"`
			Scope        string  `json:"scope"`
			Mode         string  `json:"mode"`
			Token        string  `json:"token"`
			QuotaProject *string `json:"quotaProject,omitempty"`
		}{"gcloud", "adc", "user", "proxy", token, qp}, &raw)
	default:
		return err
	}
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusBadRequest {
			fmt.Fprintln(c.stderr, "Only files from gcloud auth application-default login (authorized_user) are supported, not service account files.")
		}
		return err
	}
	var res struct{ Account string }
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	detail := "private, proxy"
	if quotaProject != "" {
		detail += ", quota project " + quotaProject
	}
	return c.done(raw, "Connected gcloud as %s (%s).", res.Account, detail)
}

func cmdIntegrationSignIn(c *cli, args []string) error {
	fs := c.flags("integration sign-in")
	var s scopeFlags
	addScopeFlags(fs, &s, "scope")
	mode := fs.String("mode", "proxy", "how the token reaches VMs: proxy|vm")
	replace := fs.Bool("replace", false, "sign in again for the existing Claude integration in this scope")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if pos[0] != "claude" {
		return usagef("sign-in is only available for claude")
	}
	if err := checkMode(*mode); err != nil {
		return err
	}
	apiScope, teamID, err := c.resolveScope(s)
	if err != nil {
		return err
	}
	label := scopeLabel(apiScope, s.team)
	var replaceID *string
	if *replace {
		old, err := c.findIntegration("claude", s)
		if err != nil {
			return err
		}
		replaceID, label, *mode = &old.ID, old.scopeLabel(), old.Mode
	}
	path, err := c.orgPath("/integrations/claude/sign-in")
	if err != nil {
		return err
	}
	var start struct{ URL, State string }
	if err := c.call(http.MethodPost, path, struct {
		Scope     string  `json:"scope"`
		TeamID    *string `json:"teamId,omitempty"`
		Mode      string  `json:"mode"`
		ReplaceID *string `json:"replaceId,omitempty"`
	}{apiScope, teamID, *mode, replaceID}, &start); err != nil {
		return err
	}
	fmt.Fprintf(c.stderr, "Open this URL, sign in, then paste the code shown:\n%s\n", start.URL)
	code, err := c.secret("Code: ", "")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, path+"/finish", struct {
		State string `json:"state"`
		Code  string `json:"code"`
	}{start.State, code}, &raw); err != nil {
		return err
	}
	var res struct{ Account string }
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	return c.done(raw, "Connected claude as %s (%s, %s).", res.Account, label, *mode)
}

func cmdIntegrationReplace(c *cli, args []string) error {
	fs := c.flags("integration replace")
	var s scopeFlags
	addScopeFlags(fs, &s, "scope")
	fromFile := fs.String("from-file", "", "read the token from file `F` (- = stdin)")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	v, err := c.findIntegration(pos[0], s)
	if err != nil {
		return err
	}
	path, err := c.orgPath("/integrations/" + v.ID + "/token")
	if err != nil {
		return err
	}
	token, err := c.secret(integrationKinds[v.Kind].title+" token: ", *fromFile)
	if err != nil {
		return err
	}
	source, _ := defaultSource(v.Kind)
	var raw json.RawMessage
	// The source is sent so a pasted token can replace a signed-in (oauth) Claude integration.
	if err := c.call(http.MethodPut, path, struct {
		Token  string `json:"token"`
		Source string `json:"source"`
	}{token, source}, &raw); err != nil {
		return err
	}
	var res struct{ Account string }
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	return c.done(raw, "Replaced the %s token (%s), now %s.", v.Kind, v.scopeLabel(), res.Account)
}

func cmdIntegrationMode(c *cli, args []string) error {
	fs := c.flags("integration mode")
	var s scopeFlags
	addScopeFlags(fs, &s, "scope")
	pos, err := c.parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	mode := pos[1]
	if err := checkMode(mode); err != nil {
		return err
	}
	v, err := c.findIntegration(pos[0], s)
	if err != nil {
		return err
	}
	path, err := c.orgPath("/integrations/" + v.ID)
	if err != nil {
		return err
	}
	if err := c.call(http.MethodPatch, path, struct {
		Mode string `json:"mode"`
	}{mode}, nil); err != nil {
		return err
	}
	if v.Mode == "vm" && mode == "proxy" && v.WasInVM {
		fmt.Fprintln(c.stderr, rotateHint)
	}
	return c.done(nil, "Set %s (%s) to %s.", v.Kind, v.scopeLabel(), mode)
}

func cmdIntegrationDisconnect(c *cli, args []string) error {
	fs := c.flags("integration disconnect")
	var s scopeFlags
	addScopeFlags(fs, &s, "scope")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	v, err := c.findIntegration(pos[0], s)
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Disconnect %s (%s)?", v.Kind, v.scopeLabel())); err != nil {
		return err
	}
	path, err := c.orgPath("/integrations/" + v.ID)
	if err != nil {
		return err
	}
	if err := c.call(http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	if v.WasInVM {
		fmt.Fprintln(c.stderr, rotateHint)
	}
	return c.done(nil, "Disconnected %s (%s).", v.Kind, v.scopeLabel())
}
