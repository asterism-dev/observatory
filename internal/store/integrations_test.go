package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/store"
)

const (
	dummyGH      = "ghp_dummy0000000000000000"
	dummyLinear  = "lin_api_dummy000000000000"
	dummyAccess  = "sk-ant-oat01-dummyaccess0000"
	dummyRefresh = "sk-ant-ort01-dummyrefresh000"
)

func newIntegrationStore(t *testing.T) (*store.Store, *pgxpool.Pool, uuid.UUID, store.Org, store.Team) {
	t.Helper()
	pool := dbtest.New(t)
	s := store.New(pool).WithMasterKey(testKey(1))
	u := login(t, s, "integ")
	org := mustCreateOrg(t, s, "I", u.User.ID)
	team, err := s.CreateTeam(context.Background(), org.ID, "core")
	if err != nil {
		t.Fatal(err)
	}
	return s, pool, u.User.ID, org, team
}

func integ(org store.Org, by uuid.UUID, kind integrations.Kind, source integrations.Source, scope store.EnvScope, owner *uuid.UUID) store.Integration {
	return store.Integration{OrgID: org.ID, Kind: kind, Source: source, Scope: scope, OwnerID: owner, CreatedBy: by, UpdatedBy: by}
}

func mustCreateIntegration(t *testing.T, s *store.Store, v store.Integration, sec store.IntegrationSecret) store.Integration {
	t.Helper()
	got, err := s.CreateIntegration(context.Background(), v, sec)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func mustIntegration(t *testing.T, s *store.Store, org, id uuid.UUID) store.Integration {
	t.Helper()
	v, err := s.Integration(context.Background(), org, id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func enforcer(t *testing.T, s *store.Store) *authz.Enforcer {
	t.Helper()
	policy, err := s.PolicySnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e, err := authz.New(policy)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func resolveIntegrations(t *testing.T, s *store.Store, e *authz.Enforcer, org uuid.UUID, owner string) map[integrations.Kind]store.ResolvedIntegration {
	t.Helper()
	list, err := s.ResolveVMIntegrations(context.Background(), e, org, owner)
	if err != nil {
		t.Fatal(err)
	}
	out := map[integrations.Kind]store.ResolvedIntegration{}
	for _, r := range list {
		out[r.Kind] = r
	}
	return out
}

func placeholderOfIntegration(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (p string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), "SELECT placeholder FROM integrations WHERE id = $1", id).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIntegrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _, uid, org, _ := newIntegrationStore(t)
	exp := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	v := integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeUser, &uid)
	v.Account, v.ScopesInfo, v.ProviderExpiresAt = "octo", "repo", &exp
	created := mustCreateIntegration(t, s, v, store.IntegrationSecret{Token: dummyGH})
	got := mustIntegration(t, s, org.ID, created.ID)
	if got.ID != created.ID || got.OrgID != org.ID || got.Kind != integrations.GitHub || got.Source != integrations.SourcePAT ||
		got.Scope != store.EnvScopeUser || got.OwnerID == nil || *got.OwnerID != uid || got.Mode != integrations.ModeProxy ||
		got.Account != "octo" || got.ScopesInfo != "repo" || got.ProviderExpiresAt == nil || !got.ProviderExpiresAt.Equal(exp) ||
		got.AccessExpiresAt != nil || got.Status != store.IntegrationOK || got.LastError != "" || got.WasInVM ||
		got.CreatedBy != uid || got.UpdatedBy != uid || !got.CreatedAt.Equal(created.CreatedAt) || !got.UpdatedAt.Equal(created.UpdatedAt) ||
		created.Mode != got.Mode || created.Status != got.Status {
		t.Fatalf("created %+v got %+v", created, got)
	}
	list, err := s.Integrations(ctx, org.ID)
	if err != nil || len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := s.Integration(ctx, org.ID, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	res := resolveIntegrations(t, s, enforcer(t, s), org.ID, authz.UserSubject(uid))
	r, ok := res[integrations.GitHub]
	if !ok || len(res) != 1 || r.Token != dummyGH || !regexp.MustCompile(`^obsph_[a-z2-7]{32}$`).MatchString(r.Placeholder) || r.ID != created.ID {
		t.Fatalf("resolved %d %v", len(res), ok)
	}
	sec := store.IntegrationSecret{Token: dummyGH, Refresh: dummyRefresh}
	for _, out := range []string{fmt.Sprint(r), fmt.Sprintf("%+v", r), fmt.Sprintf("%#v", r), fmt.Sprint(sec), fmt.Sprintf("%#v", sec)} {
		if strings.Contains(out, dummyGH) || strings.Contains(out, dummyRefresh) || strings.Contains(out, r.Placeholder) {
			t.Fatalf("leaked in %q", out)
		}
	}
	for _, x := range []any{r, sec, []store.ResolvedIntegration{r}} {
		b, err := json.Marshal(x)
		if err != nil || bytes.Contains(b, []byte(dummyGH)) || bytes.Contains(b, []byte(dummyRefresh)) || bytes.Contains(b, []byte(r.Placeholder)) {
			t.Fatalf("json leaked: %s %v", b, err)
		}
	}
}

func TestIntegrationUnique(t *testing.T) {
	ctx := context.Background()
	s, _, uid, org, team := newIntegrationStore(t)
	mustCreateIntegration(t, s, integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeUser, &uid), store.IntegrationSecret{Token: dummyGH})
	if _, err := s.CreateIntegration(ctx, integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeUser, &uid), store.IntegrationSecret{Token: dummyGH}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	for _, v := range []store.Integration{
		integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeOrg, nil),
		integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeTeam, &team.ID),
		integ(org, uid, integrations.Linear, integrations.SourceAPIKey, store.EnvScopeUser, &uid),
	} {
		if _, err := s.CreateIntegration(ctx, v, store.IntegrationSecret{Token: dummyGH}); err != nil {
			t.Fatalf("%s %s: %v", v.Kind, v.Scope, err)
		}
	}
}

func TestIntegrationSealed(t *testing.T) {
	ctx := context.Background()
	s, pool, uid, org, _ := newIntegrationStore(t)
	e := enforcer(t, s)
	gh := mustCreateIntegration(t, s, integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeOrg, nil), store.IntegrationSecret{Token: dummyGH})
	lin := mustCreateIntegration(t, s, integ(org, uid, integrations.Linear, integrations.SourceAPIKey, store.EnvScopeOrg, nil), store.IntegrationSecret{Token: dummyLinear})
	accessExp := time.Now().Add(time.Hour).Truncate(time.Second)
	cl := mustCreateIntegration(t, s, integ(org, uid, integrations.Claude, integrations.SourceOAuth, store.EnvScopeOrg, nil),
		store.IntegrationSecret{Token: dummyAccess, Refresh: dummyRefresh, AccessExpiresAt: &accessExp})
	if cl.AccessExpiresAt == nil || !mustIntegration(t, s, org.ID, cl.ID).AccessExpiresAt.Equal(accessExp) {
		t.Fatalf("access expiry %+v", cl)
	}
	rows, err := pool.Query(ctx, "SELECT ciphertext, coalesce(refresh_ciphertext, '') FROM integrations")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var ct, rct []byte
		if err := rows.Scan(&ct, &rct); err != nil {
			t.Fatal(err)
		}
		for _, tok := range []string{dummyGH, dummyLinear, dummyAccess, dummyRefresh} {
			if bytes.Contains(ct, []byte(tok)) || bytes.Contains(rct, []byte(tok)) {
				t.Fatalf("plain token %q in ciphertext", tok)
			}
		}
	}
	rows.Close()
	if res := resolveIntegrations(t, s, e, org.ID, authz.UserSubject(uid)); res[integrations.Claude].Token != dummyAccess || len(res) != 3 {
		t.Fatalf("claude resolves to %d integrations", len(res))
	}

	execSQL(t, pool, "UPDATE integrations SET nonce = refresh_nonce, ciphertext = refresh_ciphertext WHERE id = $1", cl.ID)
	if _, err := s.ResolveVMIntegrations(ctx, e, org.ID, authz.UserSubject(uid)); err == nil {
		t.Fatal("refresh token opened with the access AAD")
	}
	execSQL(t, pool, "DELETE FROM integrations WHERE id = $1", cl.ID)

	tmp := uuid.New()
	execSQL(t, pool, "UPDATE integrations SET id = $1 WHERE id = $2", tmp, gh.ID)
	execSQL(t, pool, "UPDATE integrations SET id = $1 WHERE id = $2", gh.ID, lin.ID)
	execSQL(t, pool, "UPDATE integrations SET id = $1 WHERE id = $2", lin.ID, tmp)
	if _, err := s.ResolveVMIntegrations(ctx, e, org.ID, authz.UserSubject(uid)); err == nil {
		t.Fatal("swapped rows opened")
	} else if strings.Contains(err.Error(), dummyGH) || strings.Contains(err.Error(), dummyLinear) {
		t.Fatalf("token in error %v", err)
	}
}

func TestIntegrationPlaceholderStable(t *testing.T) {
	ctx := context.Background()
	s, pool, uid, org, _ := newIntegrationStore(t)
	v := mustCreateIntegration(t, s, integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeOrg, nil), store.IntegrationSecret{Token: dummyGH})
	p := placeholderOfIntegration(t, pool, v.ID)
	same := func(step string) {
		t.Helper()
		if got := placeholderOfIntegration(t, pool, v.ID); got != p {
			t.Fatalf("%s: placeholder changed", step)
		}
	}
	replace := func() store.Integration {
		t.Helper()
		old := mustIntegration(t, s, org.ID, v.ID)
		n := old
		n.Account = "new"
		if err := s.ReplaceIntegrationToken(ctx, old, n, store.IntegrationSecret{Token: dummyGH + "2"}); err != nil {
			t.Fatal(err)
		}
		return mustIntegration(t, s, org.ID, v.ID)
	}
	setMode := func(m integrations.Mode) store.Integration {
		t.Helper()
		if err := s.SetIntegrationMode(ctx, mustIntegration(t, s, org.ID, v.ID), m, uid); err != nil {
			t.Fatal(err)
		}
		return mustIntegration(t, s, org.ID, v.ID)
	}
	if got := replace(); got.Account != "new" || got.WasInVM {
		t.Fatalf("replace %+v", got)
	}
	same("replace")
	if got := setMode(integrations.ModeVM); got.Mode != integrations.ModeVM || !got.WasInVM {
		t.Fatalf("vm %+v", got)
	}
	same("vm")
	if got := setMode(integrations.ModeProxy); got.Mode != integrations.ModeProxy || !got.WasInVM {
		t.Fatalf("proxy %+v", got)
	}
	same("proxy")
	if got := replace(); got.WasInVM {
		t.Fatalf("replace in proxy mode keeps WasInVM: %+v", got)
	}
	same("replace again")
	res := resolveIntegrations(t, s, enforcer(t, s), org.ID, authz.UserSubject(uid))
	if res[integrations.GitHub].Token != dummyGH+"2" || res[integrations.GitHub].Placeholder != p {
		t.Fatal("resolution after replace")
	}
}

func TestResolveIntegrationsNarrowest(t *testing.T) {
	ctx := context.Background()
	s, pool, uid, org, team := newIntegrationStore(t)
	member := login(t, s, "integ-member").User.ID
	execSQL(t, pool, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`, org.ID, member)
	if err := s.SetTeamMember(ctx, org.ID, team.ID, member, false); err != nil {
		t.Fatal(err)
	}
	pat := func(scope store.EnvScope, owner *uuid.UUID, tok string) store.Integration {
		return mustCreateIntegration(t, s, integ(org, uid, integrations.GitHub, integrations.SourcePAT, scope, owner), store.IntegrationSecret{Token: tok})
	}
	pat(store.EnvScopeOrg, nil, "ghp_orgA000000000000")
	pat(store.EnvScopeUser, &uid, "ghp_privB00000000000")
	pat(store.EnvScopeUser, &member, "ghp_memberC00000000")
	mustCreateIntegration(t, s, integ(org, uid, integrations.Linear, integrations.SourceAPIKey, store.EnvScopeOrg, nil), store.IntegrationSecret{Token: dummyLinear})
	e := enforcer(t, s)

	res := resolveIntegrations(t, s, e, org.ID, authz.UserSubject(uid))
	if res[integrations.GitHub].Token != "ghp_privB00000000000" || res[integrations.Linear].Token != dummyLinear || len(res) != 2 {
		t.Fatalf("user: %d", len(res))
	}
	list, err := s.ResolveVMIntegrations(ctx, e, org.ID, authz.UserSubject(uid))
	if err != nil || list[0].Kind != integrations.GitHub || list[1].Kind != integrations.Linear {
		t.Fatalf("order: %v", err)
	}

	res = resolveIntegrations(t, s, e, org.ID, authz.TeamSubject(team.ID))
	if res[integrations.GitHub].Token != "ghp_orgA000000000000" {
		t.Fatal("team without own github must get org's, never a member's private one")
	}
	pat(store.EnvScopeTeam, &team.ID, "ghp_teamD0000000000")
	res = resolveIntegrations(t, s, enforcer(t, s), org.ID, authz.TeamSubject(team.ID))
	if res[integrations.GitHub].Token != "ghp_teamD0000000000" || res[integrations.Linear].Token != dummyLinear || len(res) != 2 {
		t.Fatalf("team: %d", len(res))
	}

	if _, err := s.ResolveVMIntegrations(ctx, e, org.ID, "vm:"+uuid.NewString()); err == nil {
		t.Fatal("vm owner accepted")
	}
}

func TestResolveIntegrationsUseDenied(t *testing.T) {
	ctx := context.Background()
	s, _, uid, org, _ := newIntegrationStore(t)
	mustCreateIntegration(t, s, integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeOrg, nil), store.IntegrationSecret{Token: "ghp_orgA000000000000"})
	mustCreateIntegration(t, s, integ(org, uid, integrations.Linear, integrations.SourceAPIKey, store.EnvScopeUser, &uid), store.IntegrationSecret{Token: dummyLinear})
	policy, err := s.PolicySnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	policy.Roles = []authz.CustomRole{{ID: uuid.New(), OrgID: org.ID, Name: "no-org-integrations",
		Rules: []authz.Rule{{Effect: authz.Deny, Object: authz.ObjIntegrations, Action: "integration:use"}}}}
	policy.Bindings = []authz.Binding{{OrgID: org.ID, Subject: authz.UserSubject(uid), RoleID: policy.Roles[0].ID}}
	e, err := authz.New(policy)
	if err != nil {
		t.Fatal(err)
	}
	res := resolveIntegrations(t, s, e, org.ID, authz.UserSubject(uid))
	if _, ok := res[integrations.GitHub]; ok || res[integrations.Linear].Token != dummyLinear || len(res) != 1 {
		t.Fatalf("denied: %d", len(res))
	}
}

func TestIntegrationCascade(t *testing.T) {
	ctx := context.Background()
	s, pool, uid, org, team := newIntegrationStore(t)
	member := login(t, s, "integ-member").User.ID
	execSQL(t, pool, `INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`, org.ID, member)
	teamInt := mustCreateIntegration(t, s, integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeTeam, &team.ID), store.IntegrationSecret{Token: dummyGH})
	memberInt := mustCreateIntegration(t, s, integ(org, member, integrations.GitHub, integrations.SourcePAT, store.EnvScopeUser, &member), store.IntegrationSecret{Token: dummyGH})
	keep := []store.Integration{
		mustCreateIntegration(t, s, integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeOrg, nil), store.IntegrationSecret{Token: dummyGH}),
		mustCreateIntegration(t, s, integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeUser, &uid), store.IntegrationSecret{Token: dummyGH}),
	}
	if err := s.DeleteTeam(ctx, org.ID, team.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMember(ctx, org.ID, member); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []store.Integration{teamInt, memberInt} {
		if _, err := s.Integration(ctx, org.ID, gone.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: %v", gone.Scope, err)
		}
	}
	for _, k := range keep {
		mustIntegration(t, s, org.ID, k.ID)
	}
	outsider := login(t, s, "integ-outsider").User.ID
	if _, err := s.CreateIntegration(ctx, integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeUser, &outsider), store.IntegrationSecret{Token: dummyGH}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-member private: %v", err)
	}
}

func TestIntegrationStaleWrite(t *testing.T) {
	ctx := context.Background()
	s, pool, uid, org, team := newIntegrationStore(t)
	v := mustCreateIntegration(t, s, integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeOrg, nil), store.IntegrationSecret{Token: dummyGH})
	execSQL(t, pool, "UPDATE integrations SET scope = 'team', team_id = $1 WHERE id = $2", team.ID, v.ID)
	if err := s.SetIntegrationMode(ctx, v, integrations.ModeVM, uid); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale mode: %v", err)
	}
	if err := s.ReplaceIntegrationToken(ctx, v, v, store.IntegrationSecret{Token: dummyGH}); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale replace: %v", err)
	}
	if err := s.DeleteIntegration(ctx, v); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale delete: %v", err)
	}
	if got := mustIntegration(t, s, org.ID, v.ID); got.Mode != integrations.ModeProxy {
		t.Fatalf("stale write changed it: %+v", got)
	}
	cur := mustIntegration(t, s, org.ID, v.ID)
	if err := s.DeleteIntegration(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIntegrationMode(ctx, cur, integrations.ModeVM, uid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
}

func TestIntegrationRefreshTokenOnlyForOAuth(t *testing.T) {
	ctx := context.Background()
	s, _, uid, org, _ := newIntegrationStore(t)
	if _, err := s.CreateIntegration(ctx, integ(org, uid, integrations.Claude, integrations.SourceOAuth, store.EnvScopeOrg, nil),
		store.IntegrationSecret{Token: dummyAccess}); !errors.Is(err, store.ErrIntegrationRefresh) {
		t.Fatalf("oauth without refresh: %v", err)
	}
	if _, err := s.CreateIntegration(ctx, integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeOrg, nil),
		store.IntegrationSecret{Token: dummyGH, Refresh: dummyRefresh}); !errors.Is(err, store.ErrIntegrationRefresh) {
		t.Fatalf("pat with refresh: %v", err)
	}
	v := mustCreateIntegration(t, s, integ(org, uid, integrations.Claude, integrations.SourceSetupToken, store.EnvScopeOrg, nil), store.IntegrationSecret{Token: dummyAccess})
	n := v
	n.Source = integrations.SourceOAuth
	if err := s.ReplaceIntegrationToken(ctx, v, n, store.IntegrationSecret{Token: dummyAccess}); !errors.Is(err, store.ErrIntegrationRefresh) || strings.Contains(err.Error(), "obsph_") {
		t.Fatalf("replace oauth without refresh: %v", err)
	}
}

func TestIntegrationAccessExpiryOnlyForOAuth(t *testing.T) {
	ctx := context.Background()
	s, _, uid, org, _ := newIntegrationStore(t)
	exp := time.Now().Add(time.Hour)
	v := mustCreateIntegration(t, s, integ(org, uid, integrations.GitHub, integrations.SourcePAT, store.EnvScopeOrg, nil), store.IntegrationSecret{Token: dummyGH, AccessExpiresAt: &exp})
	if v.AccessExpiresAt != nil || mustIntegration(t, s, org.ID, v.ID).AccessExpiresAt != nil {
		t.Fatal("pat create stored an access expiry")
	}
	if err := s.ReplaceIntegrationToken(ctx, v, v, store.IntegrationSecret{Token: dummyGH, AccessExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	if mustIntegration(t, s, org.ID, v.ID).AccessExpiresAt != nil {
		t.Fatal("pat replace stored an access expiry")
	}
}

type fakeRefresh struct {
	mu      sync.Mutex
	got     []string
	n       int
	err     error
	block   chan struct{}
	entered chan struct{}
	after   func()
}

func (f *fakeRefresh) refresh(_ context.Context, rt string) (integrations.OAuthTokens, error) {
	f.mu.Lock()
	f.got = append(f.got, rt)
	f.n++
	n, err, block, entered, after := f.n, f.err, f.block, f.entered, f.after
	f.mu.Unlock()
	if after != nil {
		defer after()
	}
	if entered != nil {
		entered <- struct{}{}
	}
	if block != nil {
		<-block
	}
	if err != nil {
		return integrations.OAuthTokens{}, err
	}
	return integrations.OAuthTokens{Access: fmt.Sprintf("at-new-%d", n), Refresh: fmt.Sprintf("rt-new-%d", n), ExpiresAt: time.Now().Add(30 * time.Minute)}, nil
}

func (f *fakeRefresh) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

func dueClaude(t *testing.T, s *store.Store, org store.Org, uid uuid.UUID, in time.Duration) store.IntegrationRef {
	t.Helper()
	exp := time.Now().Add(in)
	v := mustCreateIntegration(t, s, integ(org, uid, integrations.Claude, integrations.SourceOAuth, store.EnvScopeUser, &uid),
		store.IntegrationSecret{Token: dummyAccess, Refresh: dummyRefresh, AccessExpiresAt: &exp})
	return store.IntegrationRef{OrgID: org.ID, ID: v.ID}
}

func TestRefreshIntegrationRotates(t *testing.T) {
	ctx := context.Background()
	s, _, uid, org, _ := newIntegrationStore(t)
	ref := dueClaude(t, s, org, uid, 10*time.Minute)
	before := time.Now().Add(time.Hour)
	due, err := s.DueOAuthRefreshes(ctx, before)
	if err != nil || len(due) != 1 || due[0] != ref {
		t.Fatalf("due %v %v", due, err)
	}
	f := &fakeRefresh{}
	if ok, err := s.RefreshIntegration(ctx, ref, before, f.refresh); !ok || err != nil {
		t.Fatalf("refresh %v %v", ok, err)
	}
	if res := resolveIntegrations(t, s, enforcer(t, s), org.ID, authz.UserSubject(uid)); res[integrations.Claude].Token != "at-new-1" {
		t.Fatal("resolution after refresh")
	}
	if ok, err := s.RefreshIntegration(ctx, ref, before, f.refresh); !ok || err != nil {
		t.Fatalf("second refresh %v %v", ok, err)
	}
	if got := f.calls(); !slices.Equal(got, []string{dummyRefresh, "rt-new-1"}) {
		t.Fatalf("refresh tokens passed: %d calls, rotation stored: %v", len(got), len(got) == 2 && got[1] == "rt-new-1")
	}
	if got := mustIntegration(t, s, org.ID, ref.ID); got.Status != store.IntegrationOK || got.LastError != "" || got.AccessExpiresAt == nil || time.Until(*got.AccessExpiresAt) < 20*time.Minute {
		t.Fatalf("after refresh %+v", got)
	}
}

func TestRefreshIntegrationNoDoubleRun(t *testing.T) {
	ctx := context.Background()
	s, _, uid, org, _ := newIntegrationStore(t)
	ref := dueClaude(t, s, org, uid, 10*time.Minute)
	before := time.Now().Add(time.Hour)
	f := &fakeRefresh{block: make(chan struct{}), entered: make(chan struct{}, 2)}
	type result struct {
		ok  bool
		err error
	}
	first := make(chan result, 1)
	go func() {
		ok, err := s.RefreshIntegration(ctx, ref, before, f.refresh)
		first <- result{ok, err}
	}()
	<-f.entered
	ok, err := s.RefreshIntegration(ctx, ref, before, f.refresh)
	close(f.block)
	if ok || err != nil {
		t.Fatalf("second while locked: %v %v", ok, err)
	}
	if r := <-first; !r.ok || r.err != nil {
		t.Fatalf("first: %+v", r)
	}
	if n := len(f.calls()); n != 1 {
		t.Fatalf("refresh called %d times", n)
	}
}

func TestRefreshIntegrationInvalidGrant(t *testing.T) {
	ctx := context.Background()
	s, _, uid, org, _ := newIntegrationStore(t)
	ref := dueClaude(t, s, org, uid, 10*time.Minute)
	f := &fakeRefresh{err: integrations.ErrInvalidGrant}
	if ok, err := s.RefreshIntegration(ctx, ref, time.Now().Add(time.Hour), f.refresh); ok || !errors.Is(err, integrations.ErrInvalidGrant) {
		t.Fatalf("invalid grant: %v %v", ok, err)
	}
	if got := mustIntegration(t, s, org.ID, ref.ID); got.Status != store.IntegrationExpired || got.LastError != "sign in again" {
		t.Fatalf("after invalid grant %+v", got)
	}
	if res := resolveIntegrations(t, s, enforcer(t, s), org.ID, authz.UserSubject(uid)); res[integrations.Claude].Token != dummyAccess {
		t.Fatal("old access token no longer resolved")
	}
	if due, err := s.DueOAuthRefreshes(ctx, time.Now().Add(time.Hour)); err != nil || len(due) != 0 {
		t.Fatalf("expired still due: %v %v", due, err)
	}
}

func TestRefreshIntegrationFailureRecorded(t *testing.T) {
	ctx := context.Background()
	s, _, uid, org, _ := newIntegrationStore(t)
	ref := dueClaude(t, s, org, uid, 10*time.Minute)
	f := &fakeRefresh{err: integrations.ErrUnreachable}
	if ok, err := s.RefreshIntegration(ctx, ref, time.Now().Add(time.Hour), f.refresh); ok || !errors.Is(err, integrations.ErrUnreachable) {
		t.Fatalf("unreachable: %v %v", ok, err)
	}
	if got := mustIntegration(t, s, org.ID, ref.ID); got.Status != store.IntegrationOK || got.LastError != "refresh failed" {
		t.Fatalf("after failure %+v", got)
	}
}

func TestRefreshIntegrationExpiresAfterFailures(t *testing.T) {
	ctx := context.Background()
	s, _, uid, org, _ := newIntegrationStore(t)
	ref := dueClaude(t, s, org, uid, -time.Minute)
	f := &fakeRefresh{err: integrations.ErrUnreachable}
	if ok, err := s.RefreshIntegration(ctx, ref, time.Now().Add(time.Hour), f.refresh); ok || !errors.Is(err, integrations.ErrUnreachable) {
		t.Fatalf("unreachable: %v %v", ok, err)
	}
	if got := mustIntegration(t, s, org.ID, ref.ID); got.Status != store.IntegrationExpired || got.LastError != "sign in again" {
		t.Fatalf("after failure past expiry %+v", got)
	}
}

func TestRefreshIntegrationCancelNeverExpires(t *testing.T) {
	s, _, uid, org, _ := newIntegrationStore(t)
	ref := dueClaude(t, s, org, uid, -time.Minute)
	for _, f := range []*fakeRefresh{{err: context.Canceled}, {err: context.DeadlineExceeded}} {
		if ok, err := s.RefreshIntegration(context.Background(), ref, time.Now().Add(time.Hour), f.refresh); ok || !errors.Is(err, f.err) {
			t.Fatalf("%v: %v %v", f.err, ok, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeRefresh{err: integrations.ErrUnreachable, after: cancel}
	if ok, err := s.RefreshIntegration(ctx, ref, time.Now().Add(time.Hour), f.refresh); ok || !errors.Is(err, integrations.ErrUnreachable) {
		t.Fatalf("cancelled ctx: %v %v", ok, err)
	}
	if got := mustIntegration(t, s, org.ID, ref.ID); got.Status != store.IntegrationOK || got.LastError != "refresh failed" {
		t.Fatalf("after cancel past expiry %+v", got)
	}
}

func TestRefreshIntegrationSurvivesCancel(t *testing.T) {
	s, _, uid, org, _ := newIntegrationStore(t)
	ref := dueClaude(t, s, org, uid, 10*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeRefresh{after: cancel}
	if ok, err := s.RefreshIntegration(ctx, ref, time.Now().Add(time.Hour), f.refresh); !ok || err != nil {
		t.Fatalf("refresh %v %v", ok, err)
	}
	if res := resolveIntegrations(t, s, enforcer(t, s), org.ID, authz.UserSubject(uid)); res[integrations.Claude].Token != "at-new-1" {
		t.Fatal("rotation rolled back after cancel")
	}
}

func TestRefreshIntegrationNotDue(t *testing.T) {
	ctx := context.Background()
	s, _, uid, org, _ := newIntegrationStore(t)
	ref := dueClaude(t, s, org, uid, 2*time.Hour)
	before := time.Now().Add(60 * time.Minute)
	if due, err := s.DueOAuthRefreshes(ctx, before); err != nil || len(due) != 0 {
		t.Fatalf("due %v %v", due, err)
	}
	f := &fakeRefresh{}
	if ok, err := s.RefreshIntegration(ctx, ref, before, f.refresh); ok || err != nil {
		t.Fatalf("not due: %v %v", ok, err)
	}
	if len(f.calls()) != 0 {
		t.Fatal("refresh called for a row not due")
	}
}
