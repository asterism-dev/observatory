package store_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
)

// sealFor seals plain the way the store does, with the org key unwrapped by testKey(1).
func sealFor(t *testing.T, pool *pgxpool.Pool, org, id uuid.UUID, field, plain string) (nonce, ct []byte) {
	t.Helper()
	var wn, wrapped []byte
	if err := pool.QueryRow(context.Background(), "SELECT nonce, wrapped FROM org_keys WHERE org_id = $1", org).Scan(&wn, &wrapped); err != nil {
		t.Fatal(err)
	}
	raw, err := seal.Open(testKey(1), wn, wrapped, org[:])
	if err != nil {
		t.Fatal(err)
	}
	nonce, ct, err = seal.Seal(seal.Key(raw), []byte(plain), []byte("integration:"+org.String()+":"+id.String()+":"+field))
	if err != nil {
		t.Fatal(err)
	}
	return nonce, ct
}

// insertAccount adds an inactive setup-token account behind v's existing ones.
func insertAccount(t *testing.T, pool *pgxpool.Pool, v store.Integration, email, token, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	nonce, ct := sealFor(t, pool, v.OrgID, id, "access", token)
	execSQL(t, pool, `INSERT INTO integration_accounts (id, integration_id, org_id, position, account, source, nonce, ciphertext, status, created_by, updated_by)
		SELECT $1, $2, $3, coalesce(max(position), -1) + 1, $4, 'setup-token', $5, $6, $7, $8, $8 FROM integration_accounts WHERE integration_id = $2`,
		id, v.ID, v.OrgID, email, nonce, ct, status, v.CreatedBy)
	return id
}

func activate(t *testing.T, pool *pgxpool.Pool, integration, account uuid.UUID) {
	t.Helper()
	execSQL(t, pool, "UPDATE integration_accounts SET active = false WHERE integration_id = $1", integration)
	execSQL(t, pool, "UPDATE integration_accounts SET active = true WHERE id = $1", account)
}

func mustAccounts(t *testing.T, s *store.Store, org, integration uuid.UUID) []store.IntegrationAccount {
	t.Helper()
	list, err := s.IntegrationAccounts(context.Background(), org, integration)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestMigrationMovesClaudeTokenToAccount(t *testing.T) {
	ctx := context.Background()
	s, pool, uid, org, _ := newIntegrationStore(t)
	// Creates the org key while the schema is current.
	mustCreateEnv(t, s, store.EnvVar{OrgID: org.ID, Kind: store.EnvSecret, Scope: store.EnvScopeOrg, Name: "K", CreatedBy: uid, UpdatedBy: uid}, "v")
	conn := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer conn.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, conn, os.DirFS("../db/migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 25); err != nil {
		t.Fatal(err)
	}
	claudeID, ghID := uuid.New(), uuid.New()
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	n, ct := sealFor(t, pool, org.ID, claudeID, "access", dummyAccess)
	rn, rct := sealFor(t, pool, org.ID, claudeID, "refresh", dummyRefresh)
	gn, gct := sealFor(t, pool, org.ID, ghID, "access", dummyGH)
	execSQL(t, pool, `INSERT INTO integrations (id, org_id, kind, source, scope, user_id, account, nonce, ciphertext, refresh_nonce, refresh_ciphertext,
		access_expires_at, placeholder, status, last_error, created_by, updated_by)
		VALUES ($1, $2, 'claude', 'oauth', 'user', $3, 'me@example.com', $4, $5, $6, $7, $8, 'obsph_c', 'expired', 'sign in again', $3, $3)`,
		claudeID, org.ID, uid, n, ct, rn, rct, exp)
	execSQL(t, pool, `INSERT INTO integrations (id, org_id, kind, source, scope, account, nonce, ciphertext, placeholder, created_by, updated_by)
		VALUES ($1, $2, 'github', 'pat', 'org', 'octo', $3, $4, 'obsph_g', $5, $5)`, ghID, org.ID, gn, gct, uid)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}

	accts := mustAccounts(t, s, org.ID, claudeID)
	if len(accts) != 1 {
		t.Fatalf("accounts %+v", accts)
	}
	a := accts[0]
	if a.ID != claudeID || a.IntegrationID != claudeID || a.OrgID != org.ID || !a.Active || a.Position != 0 || a.Account != "me@example.com" ||
		a.Source != integrations.SourceOAuth || a.Status != store.IntegrationExpired || a.LastError != "sign in again" ||
		a.AccessExpiresAt == nil || !a.AccessExpiresAt.Equal(exp) || a.CreatedBy != uid {
		t.Fatalf("account %+v", a)
	}
	cl := mustIntegration(t, s, org.ID, claudeID)
	if cl.Account != "me@example.com" || cl.Status != store.IntegrationExpired || cl.Source != integrations.SourceOAuth {
		t.Fatalf("integration %+v", cl)
	}
	if tok, err := s.IntegrationToken(ctx, cl); err != nil || tok != dummyAccess {
		t.Fatalf("claude token opens: %v", err)
	}
	var claudeNull, ghKept bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT ciphertext IS NULL AND refresh_ciphertext IS NULL FROM integrations WHERE id = $1),
		(SELECT ciphertext = $3 FROM integrations WHERE id = $2)`, claudeID, ghID, gct).Scan(&claudeNull, &ghKept); err != nil {
		t.Fatal(err)
	}
	if !claudeNull || !ghKept {
		t.Fatalf("claude ciphertext null %v, github unchanged %v", claudeNull, ghKept)
	}
	if tok, err := s.IntegrationToken(ctx, mustIntegration(t, s, org.ID, ghID)); err != nil || tok != dummyGH {
		t.Fatalf("github token opens: %v", err)
	}

	if _, err := provider.DownTo(ctx, 25); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if tok, err := s.IntegrationToken(ctx, cl); err != nil || tok != dummyAccess {
		t.Fatalf("claude token after round trip: %v", err)
	}

	insertAccount(t, pool, cl, "two@example.com", dummyAccess+"2", store.IntegrationOK)
	if _, err := provider.DownTo(ctx, 25); err == nil {
		t.Fatal("down with two accounts succeeded")
	}
}

func TestCreateClaudeIntegrationCreatesActiveAccount(t *testing.T) {
	s, pool, uid, org, _ := newIntegrationStore(t)
	v := integ(org, uid, integrations.Claude, integrations.SourceSetupToken, store.EnvScopeUser, &uid)
	v.Account = "me@example.com"
	cl := mustCreateIntegration(t, s, v, store.IntegrationSecret{Token: dummyAccess})
	accts := mustAccounts(t, s, org.ID, cl.ID)
	if len(accts) != 1 || !accts[0].Active || accts[0].Position != 0 || accts[0].Account != "me@example.com" ||
		accts[0].Source != integrations.SourceSetupToken || accts[0].Status != store.IntegrationOK {
		t.Fatalf("accounts %+v", accts)
	}
	var null bool
	if err := pool.QueryRow(context.Background(), "SELECT ciphertext IS NULL FROM integrations WHERE id = $1", cl.ID).Scan(&null); err != nil || !null {
		t.Fatalf("token kept on the integration: %v", err)
	}
	all, err := s.OrgIntegrationAccounts(context.Background(), org.ID)
	if err != nil || len(all) != 1 || len(all[cl.ID]) != 1 || all[cl.ID][0].ID != accts[0].ID {
		t.Fatalf("org accounts %+v %v", all, err)
	}
}

func TestResolveClaudeUsesActiveAccount(t *testing.T) {
	s, pool, uid, org, _ := newIntegrationStore(t)
	cl := mustCreateIntegration(t, s, integ(org, uid, integrations.Claude, integrations.SourceSetupToken, store.EnvScopeUser, &uid),
		store.IntegrationSecret{Token: dummyAccess})
	first := mustAccounts(t, s, org.ID, cl.ID)[0].ID
	second := insertAccount(t, pool, cl, "two@example.com", dummyAccess+"2", store.IntegrationOK)
	activate(t, pool, cl.ID, second)
	r := resolveIntegrations(t, s, enforcer(t, s), org.ID, authz.UserSubject(uid))[integrations.Claude]
	if r.Token != dummyAccess+"2" || len(r.Accounts) != 2 || r.Accounts[0].ID != second || r.Accounts[1].ID != first ||
		r.Accounts[0].Token != dummyAccess+"2" || r.Accounts[1].Token != dummyAccess {
		t.Fatalf("resolved token ok %v, %d accounts", r.Token == dummyAccess+"2", len(r.Accounts))
	}
	if got := mustIntegration(t, s, org.ID, cl.ID); got.Account != "two@example.com" {
		t.Fatalf("integration account %q, want the active one", got.Account)
	}
	if tok, err := s.IntegrationToken(context.Background(), cl); err != nil || tok != dummyAccess+"2" {
		t.Fatalf("token of the active account: %v", err)
	}
	if bytes.Contains([]byte(r.Accounts[0].String()), []byte(dummyAccess)) {
		t.Fatal("resolved account leaks its token")
	}
}

func TestResolveClaudeSkipsExpiredAlternates(t *testing.T) {
	s, pool, uid, org, _ := newIntegrationStore(t)
	cl := mustCreateIntegration(t, s, integ(org, uid, integrations.Claude, integrations.SourceSetupToken, store.EnvScopeUser, &uid),
		store.IntegrationSecret{Token: dummyAccess})
	insertAccount(t, pool, cl, "old@example.com", dummyAccess+"x", store.IntegrationExpired)
	ok := insertAccount(t, pool, cl, "ok@example.com", dummyAccess+"2", store.IntegrationOK)
	r := resolveIntegrations(t, s, enforcer(t, s), org.ID, authz.UserSubject(uid))[integrations.Claude]
	if r.Token != dummyAccess || len(r.Accounts) != 2 || r.Accounts[1].ID != ok {
		t.Fatalf("%d accounts", len(r.Accounts))
	}
}

func TestOneActiveAccountEnforced(t *testing.T) {
	s, pool, uid, org, _ := newIntegrationStore(t)
	cl := mustCreateIntegration(t, s, integ(org, uid, integrations.Claude, integrations.SourceSetupToken, store.EnvScopeUser, &uid),
		store.IntegrationSecret{Token: dummyAccess})
	insertAccount(t, pool, cl, "two@example.com", dummyAccess+"2", store.IntegrationOK)
	if _, err := pool.Exec(context.Background(), "UPDATE integration_accounts SET active = true WHERE integration_id = $1", cl.ID); err == nil {
		t.Fatal("two active accounts accepted")
	}
}

func newClaude(t *testing.T, s *store.Store, org store.Org, uid uuid.UUID, scope store.EnvScope, mode integrations.Mode) store.Integration {
	t.Helper()
	var owner *uuid.UUID
	if scope == store.EnvScopeUser {
		owner = &uid
	}
	v := integ(org, uid, integrations.Claude, integrations.SourceSetupToken, scope, owner)
	v.Mode = mode
	v.Account = "one@example.com"
	return mustCreateIntegration(t, s, v, store.IntegrationSecret{Token: dummyAccess})
}

func newAccount(email string, by uuid.UUID) store.IntegrationAccount {
	return store.IntegrationAccount{Account: email, Source: integrations.SourceSetupToken, CreatedBy: by}
}

func TestAddAccountOnlyPrivateProxy(t *testing.T) {
	ctx := context.Background()
	s, _, uid, org, team := newIntegrationStore(t)
	sec := store.IntegrationSecret{Token: dummyAccess + "2"}
	if _, err := s.AddIntegrationAccount(ctx, newClaude(t, s, org, uid, store.EnvScopeOrg, ""), newAccount("two@example.com", uid), sec); !errors.Is(err, store.ErrMultiAccountNotAllowed) {
		t.Fatalf("org scope: %v", err)
	}
	teamClaude := integ(org, uid, integrations.Claude, integrations.SourceSetupToken, store.EnvScopeTeam, &team.ID)
	teamClaude.Account = "one@example.com"
	teamClaude = mustCreateIntegration(t, s, teamClaude, store.IntegrationSecret{Token: dummyAccess})
	if _, err := s.AddIntegrationAccount(ctx, teamClaude, newAccount("two@example.com", uid), sec); !errors.Is(err, store.ErrMultiAccountNotAllowed) {
		t.Fatalf("team scope: %v", err)
	}
	user := newClaude(t, s, org, uid, store.EnvScopeUser, integrations.ModeVM)
	if _, err := s.AddIntegrationAccount(ctx, user, newAccount("two@example.com", uid), sec); !errors.Is(err, store.ErrMultiAccountNotAllowed) {
		t.Fatalf("vm mode: %v", err)
	}
	if err := s.SetIntegrationMode(ctx, user, integrations.ModeProxy, uid); err != nil {
		t.Fatal(err)
	}
	got, err := s.AddIntegrationAccount(ctx, user, newAccount("two@example.com", uid), sec)
	if err != nil || got.Position != 1 || got.Active || got.Account != "two@example.com" || got.Status != store.IntegrationOK {
		t.Fatalf("user proxy: %+v %v", got, err)
	}
}

func TestAddAccountDuplicateEmail(t *testing.T) {
	ctx := context.Background()
	s, _, uid, org, _ := newIntegrationStore(t)
	cl := newClaude(t, s, org, uid, store.EnvScopeUser, "")
	sec := store.IntegrationSecret{Token: dummyAccess + "2"}
	if _, err := s.AddIntegrationAccount(ctx, cl, newAccount("two@example.com", uid), sec); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddIntegrationAccount(ctx, cl, newAccount("two@example.com", uid), sec); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	if n := len(mustAccounts(t, s, org.ID, cl.ID)); n != 2 {
		t.Fatalf("%d accounts", n)
	}
}

func TestReplaceAccountToken(t *testing.T) {
	ctx := context.Background()
	s, pool, uid, org, _ := newIntegrationStore(t)
	cl := newClaude(t, s, org, uid, store.EnvScopeUser, "")
	b := insertAccount(t, pool, cl, "b@example.com", "old", store.IntegrationExpired)
	execSQL(t, pool, "UPDATE integration_accounts SET last_error = 'sign in again' WHERE id = $1", b)
	var acct store.IntegrationAccount
	for _, x := range mustAccounts(t, s, org.ID, cl.ID) {
		if x.ID == b {
			acct = x
		}
	}
	if err := s.ReplaceAccountToken(ctx, acct, integrations.SourceSetupToken, "b@example.com", store.IntegrationSecret{Token: "fresh"}, uid); err != nil {
		t.Fatal(err)
	}
	for _, x := range mustAccounts(t, s, org.ID, cl.ID) {
		if x.ID == b && (x.Status != store.IntegrationOK || x.LastError != "") {
			t.Fatalf("account %+v", x)
		}
	}
	acct.ID = uuid.New()
	if err := s.ReplaceAccountToken(ctx, acct, integrations.SourceSetupToken, "b@example.com", store.IntegrationSecret{Token: "x"}, uid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
}

func TestActivateAccountSwapsActive(t *testing.T) {
	ctx := context.Background()
	s, pool, uid, org, _ := newIntegrationStore(t)
	cl := newClaude(t, s, org, uid, store.EnvScopeUser, "")
	b := insertAccount(t, pool, cl, "b@example.com", "b", store.IntegrationOK)
	accts := mustAccounts(t, s, org.ID, cl.ID)
	if err := s.ActivateAccount(ctx, accts[1], true, uid); err != nil {
		t.Fatal(err)
	}
	got := mustAccounts(t, s, org.ID, cl.ID)
	if got[0].Active || !got[1].Active || got[1].ID != b || !got[1].ActivatedManually {
		t.Fatalf("accounts %+v", got)
	}
}

func TestReorderAccounts(t *testing.T) {
	ctx := context.Background()
	s, pool, uid, org, _ := newIntegrationStore(t)
	cl := newClaude(t, s, org, uid, store.EnvScopeUser, "")
	a := mustAccounts(t, s, org.ID, cl.ID)[0].ID
	b := insertAccount(t, pool, cl, "b@example.com", "b", store.IntegrationOK)
	if err := s.ReorderAccounts(ctx, cl, []uuid.UUID{b, a}, uid); err != nil {
		t.Fatal(err)
	}
	got := mustAccounts(t, s, org.ID, cl.ID)
	if got[0].ID != b || got[1].ID != a || !got[1].Active || got[0].Active {
		t.Fatalf("accounts %+v", got)
	}
	for _, bad := range [][]uuid.UUID{{a}, {a, a}, {a, b, uuid.New()}, {a, uuid.New()}} {
		if err := s.ReorderAccounts(ctx, cl, bad, uid); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("%v: %v", bad, err)
		}
	}
}

func TestDeleteActiveAccountActivatesNext(t *testing.T) {
	ctx := context.Background()
	s, pool, uid, org, _ := newIntegrationStore(t)
	cl := newClaude(t, s, org, uid, store.EnvScopeUser, "")
	b := insertAccount(t, pool, cl, "b@example.com", "b", store.IntegrationOK)
	c := insertAccount(t, pool, cl, "c@example.com", "c", store.IntegrationOK)
	execSQL(t, pool, "UPDATE integration_accounts SET limited_until = now() + interval '1 hour' WHERE id = $1", b)
	last, err := s.DeleteIntegrationAccount(ctx, mustAccounts(t, s, org.ID, cl.ID)[0])
	if err != nil || last {
		t.Fatalf("last %v: %v", last, err)
	}
	got := mustAccounts(t, s, org.ID, cl.ID)
	if len(got) != 2 || got[0].ID != b || got[0].Active || got[1].ID != c || !got[1].Active {
		t.Fatalf("accounts %+v", got)
	}
}

func TestDeleteActiveAccountFallsBackWhenNoneEligible(t *testing.T) {
	ctx := context.Background()
	s, pool, uid, org, _ := newIntegrationStore(t)
	cl := newClaude(t, s, org, uid, store.EnvScopeUser, "")
	b := insertAccount(t, pool, cl, "b@example.com", "b", store.IntegrationExpired)
	insertAccount(t, pool, cl, "c@example.com", "c", store.IntegrationExpired)
	if _, err := s.DeleteIntegrationAccount(ctx, mustAccounts(t, s, org.ID, cl.ID)[0]); err != nil {
		t.Fatal(err)
	}
	got := mustAccounts(t, s, org.ID, cl.ID)
	if !got[0].Active || got[0].ID != b || got[1].Active {
		t.Fatalf("accounts %+v", got)
	}
	if v := mustIntegration(t, s, org.ID, cl.ID); v.Account != "b@example.com" {
		t.Fatalf("integration %+v", v)
	}
}

func TestDeleteInactiveAccountKeepsActive(t *testing.T) {
	ctx := context.Background()
	s, pool, uid, org, _ := newIntegrationStore(t)
	cl := newClaude(t, s, org, uid, store.EnvScopeUser, "")
	insertAccount(t, pool, cl, "b@example.com", "b", store.IntegrationOK)
	if _, err := s.DeleteIntegrationAccount(ctx, mustAccounts(t, s, org.ID, cl.ID)[1]); err != nil {
		t.Fatal(err)
	}
	if got := mustAccounts(t, s, org.ID, cl.ID); len(got) != 1 || !got[0].Active {
		t.Fatalf("accounts %+v", got)
	}
}

func TestDeleteLastAccountDeletesIntegration(t *testing.T) {
	ctx := context.Background()
	s, _, uid, org, _ := newIntegrationStore(t)
	cl := newClaude(t, s, org, uid, store.EnvScopeUser, "")
	last, err := s.DeleteIntegrationAccount(ctx, mustAccounts(t, s, org.ID, cl.ID)[0])
	if err != nil || !last {
		t.Fatalf("last %v: %v", last, err)
	}
	if _, err := s.Integration(ctx, org.ID, cl.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("integration: %v", err)
	}
}

func TestModeVMRefusedWithTwoAccounts(t *testing.T) {
	ctx := context.Background()
	s, pool, uid, org, _ := newIntegrationStore(t)
	cl := newClaude(t, s, org, uid, store.EnvScopeUser, "")
	if err := s.SetIntegrationMode(ctx, cl, integrations.ModeVM, uid); err != nil {
		t.Fatalf("one account: %v", err)
	}
	if err := s.SetIntegrationMode(ctx, cl, integrations.ModeProxy, uid); err != nil {
		t.Fatal(err)
	}
	insertAccount(t, pool, cl, "b@example.com", "b", store.IntegrationOK)
	if err := s.SetIntegrationMode(ctx, cl, integrations.ModeVM, uid); !errors.Is(err, store.ErrMultiAccountNotAllowed) {
		t.Fatalf("two accounts: %v", err)
	}
}
