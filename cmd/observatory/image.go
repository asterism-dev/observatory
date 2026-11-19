package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

type imageJSON struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Description    string            `json:"description"`
	Ref            string            `json:"ref"`
	Scope          string            `json:"scope"`
	OwnerName      string            `json:"ownerName"`
	Platform       bool              `json:"platform"`
	RecipeRevision *int              `json:"recipeRevision"`
	Tags           map[string]string `json:"tags"`
	LatestDigest   string            `json:"latestDigest"`
	SizeBytes      int64             `json:"sizeBytes"`
	LastBuildState string            `json:"lastBuildState"`
	PushError      string            `json:"pushError"`
	CreatedAt      time.Time         `json:"createdAt"`

	AutoSchedule      string     `json:"autoSchedule"`
	AutoOnBaseChange  bool       `json:"autoOnBaseChange"`
	AutoError         string     `json:"autoError"`
	AutoLastAt        *time.Time `json:"autoLastAt"`
	AutoNeedsApproval bool       `json:"autoNeedsApproval"`
}

type buildJSON struct {
	ID        string    `json:"id"`
	ImageID   string    `json:"imageId"`
	Revision  int       `json:"revision"`
	State     string    `json:"state"`
	Automatic bool      `json:"automatic"`
	QueuedAt  time.Time `json:"queuedAt"`
	Digest    string    `json:"digest"`
	Error     string    `json:"error"`
}

type recipeRefBody struct {
	Kind   string  `json:"kind"`
	Scope  string  `json:"scope"`
	TeamID *string `json:"teamId,omitempty"`
	Name   string  `json:"name"`
}

func imageCommands() []command {
	return []command{
		{"image list", "", "list images", cmdImageList},
		{"image show", "<ref>", "show an image", cmdImageShow},
		{"image create", "<name> [--scope S] [--team T] [--description D]", "create an image", cmdImageCreate},
		{"image update", "<ref> [--name N] [--description D]", "rename an image or change its description", cmdImageUpdate},
		{"image delete", "<ref>", "delete an image and its versions", cmdImageDelete},
		{"image visibility", "<ref> --scope private|team|org [--team T]", "change who can see an image", cmdImageVisibility},
		{"image recipe pull", "<ref> <dir>", "download the recipe into a directory", cmdRecipePull},
		{"image recipe push", "<ref> <dir> [--ref secret:SCOPE/NAME]... [--ref integration:SCOPE/KIND]...", "upload a directory as the next recipe revision", cmdRecipePush},
		{"image build", "<ref> [--follow]", "build the current recipe", cmdImageBuild},
		{"image auto", "<ref> [--schedule off|daily|weekly] [--on-base-change=true|false] [--approve]", "rebuild an image automatically", cmdImageAuto},
		{"image builds", "<ref>", "list the builds of an image", cmdImageBuilds},
		{"image log", "<build-id> [--follow]", "show a build log", cmdImageLog},
		{"image cancel", "<build-id>", "cancel a queued or running build", cmdImageCancel},
		{"image push", "<ref>[:tag] <local-image>", "push a local docker image as a version", cmdImagePush},
		{"image login", "", "let docker authenticate against the hub registry", cmdImageLogin},
		{"image tag", "<ref> <tag> <digest>", "point a tag at a version", cmdImageTag},
		{"image tag delete", "<ref> <tag>", "remove a tag", cmdImageTagDelete},
		{"image versions", "<ref>", "list the versions of an image", cmdImageVersions},
		{"image versions delete", "<ref> <digest>", "delete a version and its tags", cmdImageVersionDelete},
		{"image default set", "<org|user|team:T> <ref>", "set the default image for a level", cmdDefaultSet},
		{"image default show", "<org|user|team:T>", "show the default image for a level", cmdDefaultShow},
		{"image default clear", "<org|user|team:T>", "clear the default image of a level", cmdDefaultClear},
		{"image builders", "", "list running builder VMs (admins)", cmdImageBuilders},
	}
}

func shortDigest(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return orDash(d)
}

func (i imageJSON) visibility() string {
	if i.Platform {
		return "platform"
	}
	return scopeLabel(i.Scope, i.OwnerName)
}

// automation describes the automatic rebuild settings, e.g. "weekly, on base change".
func (i imageJSON) automation() string {
	var parts []string
	if i.AutoSchedule != "" {
		parts = append(parts, i.AutoSchedule)
	}
	if i.AutoOnBaseChange {
		parts = append(parts, "on base change")
	}
	if len(parts) == 0 {
		return "off"
	}
	if i.AutoNeedsApproval {
		parts = append(parts, fmt.Sprintf("revision %s needs approval (image auto --approve)", revLabel(i.RecipeRevision)))
	}
	return strings.Join(parts, ", ")
}

func revLabel(r *int) string {
	if r == nil {
		return "–"
	}
	return fmt.Sprintf("r%d", *r)
}

func (c *cli) imageList() ([]imageJSON, error) {
	var l []imageJSON
	path, err := c.orgPath("/images")
	if err != nil {
		return nil, err
	}
	return l, c.call(http.MethodGet, path, nil, &l)
}

// splitImageRef separates ref[:tag] or ref@digest; the tag or digest is returned without its separator.
func splitImageRef(s string) (name, tag, digest string) {
	if n, d, ok := strings.Cut(s, "@"); ok {
		return n, "", d
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[i:], "/") {
		return s[:i], s[i+1:], ""
	}
	return s, "", ""
}

// findImage matches an ID, a qualified ref or a bare name, and returns the tag or digest that came with it.
func (c *cli) findImage(key string) (img imageJSON, tag, digest string, err error) {
	name, tag, digest := splitImageRef(key)
	l, err := c.imageList()
	if err != nil {
		return img, "", "", err
	}
	var hits []imageJSON
	for _, i := range l {
		switch {
		case i.ID == name || i.Ref == name:
			return i, tag, digest, nil
		case i.Name == name && !i.Platform:
			hits = append(hits, i)
		}
	}
	switch len(hits) {
	case 0:
		return img, "", "", notFoundf("image %q not found", name)
	case 1:
		return hits[0], tag, digest, nil
	}
	refs := make([]string, len(hits))
	for i, h := range hits {
		refs[i] = h.Ref
	}
	return img, "", "", usagef("image %q is ambiguous: %s", name, strings.Join(refs, ", "))
}

func (c *cli) imagePath(id, rest string) (string, error) { return c.orgPath("/images/" + id + rest) }

func cmdImageList(c *cli, args []string) error {
	if _, err := c.parse(c.flags("image list"), args, 0, 0); err != nil {
		return err
	}
	path, err := c.orgPath("/images")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	var l []imageJSON
	if err := json.Unmarshal(raw, &l); err != nil {
		return err
	}
	rows := make([][]string, len(l))
	for i, im := range l {
		size := "–"
		if im.SizeBytes > 0 {
			size = humanBytes(im.SizeBytes)
		}
		rows[i] = []string{im.Ref, im.visibility(), revLabel(im.RecipeRevision), shortDigest(im.LatestDigest), size, orDash(im.LastBuildState)}
	}
	return c.emit(raw, []string{"image", "visibility", "recipe", "latest", "size", "last build"}, rows)
}

func cmdImageShow(c *cli, args []string) error {
	pos, err := c.parse(c.flags("image show"), args, 1, 1)
	if err != nil {
		return err
	}
	img, _, _, err := c.findImage(pos[0])
	if err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &img); err != nil {
		return err
	}
	tags := make([]string, 0, len(img.Tags))
	for t, d := range img.Tags {
		tags = append(tags, t+"="+shortDigest(d))
	}
	sort.Strings(tags)
	pairs := [][2]string{
		{"ID", img.ID}, {"Ref", img.Ref}, {"Description", orDash(img.Description)}, {"Visibility", img.visibility()},
		{"Recipe", revLabel(img.RecipeRevision)}, {"Latest", orDash(img.LatestDigest)}, {"Size", humanBytes(img.SizeBytes)},
		{"Tags", orDash(strings.Join(tags, ", "))}, {"Last build", orDash(img.LastBuildState)},
	}
	if img.PushError != "" {
		pairs = append(pairs, [2]string{"Push error", img.PushError})
	}
	if !img.Platform {
		pairs = append(pairs, [2]string{"Automatic", img.automation()})
	}
	if img.AutoError != "" {
		pairs = append(pairs, [2]string{"Auto error", "automatic rebuild failed: " + img.AutoError})
	}
	pairs = append(pairs, [2]string{"Created", ago(c.now(), img.CreatedAt)})
	return c.emitKV(raw, pairs)
}

func cmdImageCreate(c *cli, args []string) error {
	fs := c.flags("image create")
	var s scopeFlags
	addScopeFlags(fs, &s, "scope")
	desc := fs.String("description", "", "description")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	scope, teamID, err := c.resolveScope(s)
	if err != nil {
		return err
	}
	path, err := c.orgPath("/images")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	err = c.call(http.MethodPost, path, struct {
		Name        string  `json:"name"`
		Description string  `json:"description"`
		Scope       string  `json:"scope"`
		TeamID      *string `json:"teamId,omitempty"`
	}{pos[0], *desc, scope, teamID}, &raw)
	if err != nil {
		return err
	}
	return c.done(raw, "Image %s created.", pos[0])
}

func cmdImageUpdate(c *cli, args []string) error {
	fs := c.flags("image update")
	name := fs.String("name", "", "new name")
	desc := fs.String("description", "", "new description")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	var body struct {
		Name        *string `json:"name,omitempty"`
		Description *string `json:"description,omitempty"`
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "name":
			body.Name = name
		case "description":
			body.Description = desc
		}
	})
	if body.Name == nil && body.Description == nil {
		return usagef("pass --name or --description")
	}
	img, _, _, err := c.findImage(pos[0])
	if err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPatch, path, body, &raw); err != nil {
		return err
	}
	return c.done(raw, "Image %s updated.", img.Ref)
}

func cmdImageDelete(c *cli, args []string) error {
	pos, err := c.parse(c.flags("image delete"), args, 1, 1)
	if err != nil {
		return err
	}
	img, _, _, err := c.findImage(pos[0])
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete image %s with all its versions and builds?", img.Ref)); err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "")
	if err != nil {
		return err
	}
	if err := c.call(http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	return c.done(nil, "Image %s deleted.", img.Ref)
}

func cmdImageVisibility(c *cli, args []string) error {
	fs := c.flags("image visibility")
	var s scopeFlags
	addScopeFlags(fs, &s, "scope")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	scope, teamID, err := c.resolveScope(s)
	if err != nil {
		return err
	}
	img, _, _, err := c.findImage(pos[0])
	if err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "")
	if err != nil {
		return err
	}
	body := struct {
		Scope                 string  `json:"scope"`
		TeamID                *string `json:"teamId,omitempty"`
		ConfirmDeleteVersions bool    `json:"confirmDeleteVersions,omitempty"`
	}{Scope: scope, TeamID: teamID}
	var raw json.RawMessage
	err = c.call(http.MethodPatch, path, body, &raw)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusConflict && ae.Code == "widen_deletes_versions" {
		if err := c.confirm(ae.Msg + ". Continue?"); err != nil {
			return err
		}
		body.ConfirmDeleteVersions = true
		err = c.call(http.MethodPatch, path, body, &raw)
	}
	if err != nil {
		return err
	}
	return c.done(raw, "Image %s is now visible to %s.", img.Name, scopeLabel(scope, s.team))
}

// rawCall sends body as is and returns the 2xx response; the caller closes it.
func (c *cli) rawCall(hc *http.Client, method, path string, hdr map[string]string, body io.Reader) (*http.Response, error) {
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(c.ctx, method, c.hub()+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct{ Error, Code string }
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return nil, &apiError{resp.StatusCode, e.Error, e.Code}
	}
	return resp, nil
}

func (c *cli) rawBytes(method, path string) ([]byte, error) {
	resp, err := c.rawCall(c.http, method, path, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func cmdRecipePull(c *cli, args []string) error {
	pos, err := c.parse(c.flags("image recipe pull"), args, 2, 2)
	if err != nil {
		return err
	}
	img, _, _, err := c.findImage(pos[0])
	if err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "/recipe")
	if err != nil {
		return err
	}
	var rev struct {
		Revision *int `json:"revision"`
		Files    []struct {
			Path    string  `json:"path"`
			Mode    uint32  `json:"mode"`
			Content *string `json:"content"`
		} `json:"files"`
		Refs []recipeRefBody `json:"refs"`
	}
	if err := c.call(http.MethodGet, path, nil, &rev); err != nil {
		return err
	}
	if rev.Revision == nil {
		return usagef("image %s has no recipe yet", img.Ref)
	}
	for _, f := range rev.Files {
		rel := filepath.FromSlash(f.Path)
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("the hub sent the unsafe path %q", f.Path)
		}
		var content []byte
		if f.Content != nil {
			content = []byte(*f.Content)
		} else {
			segs := strings.Split(f.Path, "/")
			for i, s := range segs {
				segs[i] = url.PathEscape(s)
			}
			if content, err = c.rawBytes(http.MethodGet, path+"/files/"+strings.Join(segs, "/")); err != nil {
				return err
			}
		}
		dst := filepath.Join(pos[1], rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, content, os.FileMode(f.Mode&0o777)); err != nil {
			return err
		}
	}
	fmt.Fprintf(c.stdout, "Pulled revision %d (%d files) to %s.\n", *rev.Revision, len(rev.Files), pos[1])
	for _, r := range rev.Refs {
		scope := r.Scope
		if r.TeamID != nil {
			scope = "team:" + *r.TeamID
		}
		fmt.Fprintf(c.stdout, "  --ref %s:%s/%s\n", r.Kind, scope, r.Name)
	}
	return nil
}

func (c *cli) parseRecipeRef(s string) (recipeRefBody, error) {
	kind, rest, _ := strings.Cut(s, ":")
	scope, name, ok := strings.Cut(rest, "/")
	if !ok || name == "" || (kind != "secret" && kind != "integration") {
		return recipeRefBody{}, usagef("--ref %q: use secret:<scope>/<NAME> or integration:<scope>/<kind>", s)
	}
	var sf scopeFlags
	switch {
	case scope == "org":
		sf = scopeFlags{"org", ""}
	case scope == "private" || scope == "user" || scope == "me":
		sf = scopeFlags{"private", ""}
	case strings.HasPrefix(scope, "team:") && len(scope) > 5:
		sf = scopeFlags{"team", scope[5:]}
	default:
		return recipeRefBody{}, usagef("--ref %q: scope must be org, private or team:<team>", s)
	}
	apiScope, teamID, err := c.resolveScope(sf)
	if err != nil {
		return recipeRefBody{}, err
	}
	return recipeRefBody{kind, apiScope, teamID, name}, nil
}

func cmdRecipePush(c *cli, args []string) error {
	fs := c.flags("image recipe push")
	var refFlags stringList
	fs.Var(&refFlags, "ref", "secret or integration the build may use, `KIND:SCOPE/NAME` (repeatable)")
	pos, err := c.parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	refs := []recipeRefBody{}
	for _, s := range refFlags {
		r, err := c.parseRecipeRef(s)
		if err != nil {
			return err
		}
		refs = append(refs, r)
	}
	img, _, _, err := c.findImage(pos[0])
	if err != nil {
		return err
	}
	archive, err := tarRecipeDir(pos[1], c.stderr)
	if err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "/recipe")
	if err != nil {
		return err
	}
	hdr := map[string]string{"Content-Type": "application/gzip"}
	if len(refs) > 0 {
		b, _ := json.Marshal(refs)
		hdr["X-Observatory-Refs"] = string(b)
	}
	hc := *c.http
	hc.Timeout = 0
	resp, err := c.rawCall(&hc, http.MethodPut, path, hdr, bytes.NewReader(archive))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct{ Revision int }
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	return c.done(raw, "Recipe revision %d saved for %s.", out.Revision, img.Ref)
}

func cmdImageBuild(c *cli, args []string) error {
	fs := c.flags("image build")
	follow := fs.Bool("follow", false, "stream the build log until it ends")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	img, _, _, err := c.findImage(pos[0])
	if err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "/builds")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, path, nil, &raw); err != nil {
		return err
	}
	var out struct {
		BuildID string `json:"buildId"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	if !*follow {
		return c.done(raw, "Build %s started.", out.BuildID)
	}
	fmt.Fprintf(c.stderr, "Build %s started.\n", out.BuildID)
	state, err := c.followLog(path + "/" + out.BuildID + "/log?follow=1")
	if err != nil {
		return err
	}
	var b buildJSON
	if err := c.call(http.MethodGet, path+"/"+out.BuildID, nil, &b); err != nil {
		return err
	}
	if state != "succeeded" {
		return &exitError{1, fmt.Sprintf("build %s: %s", state, b.Error)}
	}
	return c.done(nil, "Build %s succeeded: %s", out.BuildID, b.Digest)
}

func cmdImageAuto(c *cli, args []string) error {
	fs := c.flags("image auto")
	schedule := fs.String("schedule", "", "off, daily or weekly")
	onBase := fs.Bool("on-base-change", false, "rebuild when a base image changes")
	approve := fs.Bool("approve", false, "let automatic builds run the current recipe revision after reviewing it")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	var body struct {
		AutoSchedule     *string `json:"autoSchedule,omitempty"`
		AutoOnBaseChange *bool   `json:"autoOnBaseChange,omitempty"`
		ApproveRevision  *int    `json:"approveRevision,omitempty"`
	}
	var bad bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "schedule":
			bad = !slices.Contains([]string{"off", "daily", "weekly"}, *schedule)
			if *schedule == "off" {
				*schedule = ""
			}
			body.AutoSchedule = schedule
		case "on-base-change":
			body.AutoOnBaseChange = onBase
		}
	})
	if bad {
		return usagef("--schedule must be off, daily or weekly")
	}
	img, _, _, err := c.findImage(pos[0])
	if err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "")
	if err != nil {
		return err
	}
	wasOff := img.AutoSchedule == "" && !img.AutoOnBaseChange
	turnsOn := body.AutoSchedule != nil && *body.AutoSchedule != "" || body.AutoOnBaseChange != nil && *body.AutoOnBaseChange
	// Turning automation on approves the revision shown here; a later one would need its own approval.
	if *approve && img.RecipeRevision == nil {
		return usagef("image %s has no recipe", img.Ref)
	}
	if *approve || wasOff && turnsOn {
		body.ApproveRevision = img.RecipeRevision
	}
	var raw json.RawMessage
	if body.AutoSchedule != nil || body.AutoOnBaseChange != nil || body.ApproveRevision != nil {
		err = c.call(http.MethodPatch, path, body, &raw)
	} else {
		err = c.call(http.MethodGet, path, nil, &raw)
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &img); err != nil {
		return err
	}
	return c.done(raw, "Automatic rebuilds of %s: %s.", img.Ref, img.automation())
}

func cmdImageBuilds(c *cli, args []string) error {
	pos, err := c.parse(c.flags("image builds"), args, 1, 1)
	if err != nil {
		return err
	}
	img, _, _, err := c.findImage(pos[0])
	if err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "/builds")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	var l []buildJSON
	if err := json.Unmarshal(raw, &l); err != nil {
		return err
	}
	rows := make([][]string, len(l))
	for i, b := range l {
		state := b.State
		if b.Automatic {
			state += " (auto)"
		}
		rows[i] = []string{b.ID, fmt.Sprintf("r%d", b.Revision), state, ago(c.now(), b.QueuedAt), shortDigest(b.Digest), b.Error}
	}
	return c.emit(raw, []string{"build", "recipe", "state", "queued", "digest", "error"}, rows)
}

// cleanLog keeps build output from sending escape sequences to the terminal.
func cleanLog(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' && r != '\t' && r != '\n' || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

// followLog prints the SSE log stream and returns the state of the end event.
func (c *cli) followLog(path string) (string, error) {
	hc := *c.http
	hc.Timeout = 0
	resp, err := c.rawCall(&hc, http.MethodGet, path, map[string]string{"Accept": "text/event-stream"}, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	var event, data string
	for sc.Scan() {
		switch line := sc.Text(); {
		case line == "":
			if event == "end" {
				return data, nil
			}
			fmt.Fprintln(c.stdout, cleanLog(data))
			event, data = "", ""
		case strings.HasPrefix(line, "event: "):
			event = line[len("event: "):]
		case strings.HasPrefix(line, "data: "):
			data = line[len("data: "):]
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", &exitError{1, "the log stream ended before the build did"}
}

// imageOfBuild finds the image a build belongs to, as the log route is per image.
func (c *cli) imageOfBuild(id string) (imageJSON, error) {
	l, err := c.imageList()
	if err != nil {
		return imageJSON{}, err
	}
	for _, img := range l {
		if img.Platform {
			continue
		}
		path, err := c.imagePath(img.ID, "/builds")
		if err != nil {
			return img, err
		}
		var bs []buildJSON
		if err := c.call(http.MethodGet, path, nil, &bs); err != nil {
			return img, err
		}
		if slices.ContainsFunc(bs, func(b buildJSON) bool { return b.ID == id }) {
			return img, nil
		}
	}
	return imageJSON{}, notFoundf("build %q not found", id)
}

func cmdImageLog(c *cli, args []string) error {
	fs := c.flags("image log")
	follow := fs.Bool("follow", false, "stream the log until the build ends")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	img, err := c.imageOfBuild(pos[0])
	if err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "/builds/"+pos[0]+"/log")
	if err != nil {
		return err
	}
	if *follow {
		_, err = c.followLog(path + "?follow=1")
		return err
	}
	b, err := c.rawBytes(http.MethodGet, path)
	if err != nil {
		return err
	}
	_, err = io.WriteString(c.stdout, cleanLog(string(b)))
	return err
}

func cmdImageCancel(c *cli, args []string) error {
	pos, err := c.parse(c.flags("image cancel"), args, 1, 1)
	if err != nil {
		return err
	}
	img, err := c.imageOfBuild(pos[0])
	if err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "/builds/"+url.PathEscape(pos[0])+"/cancel")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodPost, path, nil, &raw); err != nil {
		return err
	}
	return c.done(raw, "Build %s cancelled.", pos[0])
}

func cmdImageTagDelete(c *cli, args []string) error {
	pos, err := c.parse(c.flags("image tag delete"), args, 2, 2)
	if err != nil {
		return err
	}
	img, _, _, err := c.findImage(pos[0])
	if err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "/tags/"+url.PathEscape(pos[1]))
	if err != nil {
		return err
	}
	if err := c.call(http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	return c.done(nil, "Tag %s of %s removed.", pos[1], img.Ref)
}

func cmdImageVersionDelete(c *cli, args []string) error {
	pos, err := c.parse(c.flags("image versions delete"), args, 2, 2)
	if err != nil {
		return err
	}
	img, _, _, err := c.findImage(pos[0])
	if err != nil {
		return err
	}
	if err := c.confirm(fmt.Sprintf("Delete version %s of %s with its tags?", pos[1], img.Ref)); err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "/versions/"+url.PathEscape(pos[1]))
	if err != nil {
		return err
	}
	if err := c.call(http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	return c.done(nil, "Version %s of %s deleted.", shortDigest(pos[1]), img.Ref)
}

func cmdImageTag(c *cli, args []string) error {
	pos, err := c.parse(c.flags("image tag"), args, 3, 3)
	if err != nil {
		return err
	}
	img, _, _, err := c.findImage(pos[0])
	if err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "/tags/"+url.PathEscape(pos[1]))
	if err != nil {
		return err
	}
	if err := c.call(http.MethodPut, path, struct {
		Digest string `json:"digest"`
	}{pos[2]}, nil); err != nil {
		return err
	}
	return c.done(nil, "Tag %s of %s now points to %s.", pos[1], img.Ref, shortDigest(pos[2]))
}

func cmdImageVersions(c *cli, args []string) error {
	pos, err := c.parse(c.flags("image versions"), args, 1, 1)
	if err != nil {
		return err
	}
	img, _, _, err := c.findImage(pos[0])
	if err != nil {
		return err
	}
	path, err := c.imagePath(img.ID, "/versions")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	var l []struct {
		Digest    string    `json:"digest"`
		Source    string    `json:"source"`
		Revision  *int      `json:"revision"`
		SizeBytes int64     `json:"sizeBytes"`
		CreatedAt time.Time `json:"createdAt"`
		Tags      []string  `json:"tags"`
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return err
	}
	rows := make([][]string, len(l))
	for i, v := range l {
		rows[i] = []string{shortDigest(v.Digest), v.Source, revLabel(v.Revision), humanBytes(v.SizeBytes), ago(c.now(), v.CreatedAt), orDash(strings.Join(v.Tags, ", "))}
	}
	return c.emit(raw, []string{"digest", "source", "recipe", "size", "created", "tags"}, rows)
}

func cmdImageBuilders(c *cli, args []string) error {
	if _, err := c.parse(c.flags("image builders"), args, 0, 0); err != nil {
		return err
	}
	path, err := c.orgPath("/builder-vms")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	var l vmList
	if err := json.Unmarshal(raw, &l); err != nil {
		return err
	}
	rows := make([][]string, len(l.VMs))
	for i, v := range l.VMs {
		rows[i] = []string{v.Name, v.State, v.Node.Name, v.size()}
	}
	return c.emit(raw, []string{"name", "state", "node", "size"}, rows)
}

func (c *cli) defaultPath(level string) (string, error) {
	switch team, isTeam := strings.CutPrefix(level, "team:"); {
	case level == "org":
		return "/image-defaults/org", nil
	case level == "user" || level == "private" || level == "me":
		return "/image-defaults/user", nil
	case isTeam && team != "":
		t, err := c.findTeam(team)
		if err != nil {
			return "", err
		}
		return "/image-defaults/team/" + t.ID, nil
	}
	return "", usagef("level must be org, user or team:<team>")
}

func cmdDefaultSet(c *cli, args []string) error {
	pos, err := c.parse(c.flags("image default set"), args, 2, 2)
	if err != nil {
		return err
	}
	rest, err := c.defaultPath(pos[0])
	if err != nil {
		return err
	}
	if err := c.simpleCall(http.MethodPut, rest, struct {
		Ref string `json:"ref"`
	}{pos[1]}); err != nil {
		return err
	}
	return c.done(nil, "Default image for %s set to %s.", pos[0], pos[1])
}

func cmdDefaultShow(c *cli, args []string) error {
	pos, err := c.parse(c.flags("image default show"), args, 1, 1)
	if err != nil {
		return err
	}
	rest, err := c.defaultPath(pos[0])
	if err != nil {
		return err
	}
	path, err := c.orgPath(rest)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := c.call(http.MethodGet, path, nil, &raw); err != nil {
		return err
	}
	var d struct {
		Configured *struct {
			Ref     string `json:"ref"`
			Removed bool   `json:"removed"`
		} `json:"configured"`
		Effective struct {
			Ref    string `json:"ref"`
			Source string `json:"source"`
		} `json:"effective"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	configured := "–"
	if d.Configured != nil {
		configured = d.Configured.Ref
		if d.Configured.Removed {
			configured += " (version removed, ignored)"
		}
	}
	return c.emitKV(raw, [][2]string{{"Configured", configured}, {"Effective", d.Effective.Ref}, {"Source", orDash(d.Effective.Source)}})
}

func cmdDefaultClear(c *cli, args []string) error {
	pos, err := c.parse(c.flags("image default clear"), args, 1, 1)
	if err != nil {
		return err
	}
	rest, err := c.defaultPath(pos[0])
	if err != nil {
		return err
	}
	if err := c.simpleCall(http.MethodDelete, rest, nil); err != nil {
		return err
	}
	return c.done(nil, "Default image for %s cleared.", pos[0])
}

type ignoreRule struct {
	re     *regexp.Regexp
	negate bool
}

func parseDockerignore(r io.Reader) []ignoreRule {
	var rules []ignoreRule
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := strings.HasPrefix(line, "!")
		line = strings.TrimSpace(strings.TrimPrefix(line, "!"))
		line = path.Clean(strings.TrimPrefix(line, "/"))
		var re strings.Builder
		re.WriteString("^")
		for i := 0; i < len(line); i++ {
			switch {
			case strings.HasPrefix(line[i:], "**/"):
				re.WriteString("(?:.*/)?")
				i += 2
			case strings.HasPrefix(line[i:], "**"):
				re.WriteString(".*")
				i++
			case line[i] == '*':
				re.WriteString("[^/]*")
			case line[i] == '?':
				re.WriteString("[^/]")
			default:
				re.WriteString(regexp.QuoteMeta(line[i : i+1]))
			}
		}
		re.WriteString("$")
		if rx, err := regexp.Compile(re.String()); err == nil {
			rules = append(rules, ignoreRule{rx, negate})
		}
	}
	return rules
}

// ignored lets the last matching rule win; a rule matching a parent directory covers its files.
func ignored(rules []ignoreRule, rel string) bool {
	res := false
	for _, r := range rules {
		for p := rel; p != "."; p = path.Dir(p) {
			if r.re.MatchString(p) {
				res = !r.negate
				break
			}
		}
	}
	return res
}

func tarRecipeDir(dir string, warn io.Writer) ([]byte, error) {
	var rules []ignoreRule
	if f, err := os.Open(filepath.Join(dir, ".dockerignore")); err == nil {
		rules = parseDockerignore(f)
		f.Close()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	hasNegation := slices.ContainsFunc(rules, func(r ignoreRule) bool { return r.negate })
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel != "Dockerfile" && ignored(rules, rel) {
			if d.IsDir() && !hasNegation {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			fmt.Fprintf(warn, "observatory: skipping %s, not a regular file\n", rel)
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: int64(info.Mode().Perm()), Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (c *cli) registryHost() (string, error) {
	u, err := url.Parse(c.hub())
	if err != nil || u.Host == "" {
		return "", usagef("invalid hub URL %q", c.hub())
	}
	return u.Host, nil
}

func cmdImagePush(c *cli, args []string) error {
	pos, err := c.parse(c.flags("image push"), args, 2, 2)
	if err != nil {
		return err
	}
	img, tag, digest, err := c.findImage(pos[0])
	if err != nil {
		return err
	}
	switch {
	case img.Platform:
		return usagef("the platform image is read-only")
	case digest != "":
		return usagef("push to a tag, not a digest")
	case tag == "":
		tag = "latest"
	}
	host, err := c.registryHost()
	if err != nil {
		return err
	}
	docker, err := c.lookPath("docker")
	if err != nil {
		return usagef("docker not found in PATH")
	}
	target := host + "/i/" + img.ID + ":" + tag
	for _, argv := range [][]string{{"docker", "tag", pos[1], target}, {"docker", "push", target}} {
		if err := c.runProg(docker, argv); err != nil {
			return err
		}
	}
	return c.done(nil, "Pushed %s as %s:%s.", pos[1], img.Ref, tag)
}
