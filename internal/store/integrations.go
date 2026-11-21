package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/egress"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/seal"
)

const (
	IntegrationOK      = "ok"
	IntegrationExpired = "expired"
)

// ErrIntegrationRefresh is checked before sealing so a CHECK violation, whose detail would carry the row, never surfaces.
var ErrIntegrationRefresh = errors.New("a refresh token is required for oauth and adc sign-ins and not allowed otherwise")

var ErrInvalidQuotaProject = errors.New("invalid quota project")

// AccountRef names an oauth account; an integration's first account shares its id.
type AccountRef struct{ OrgID, ID uuid.UUID }

// Integration is the metadata of a connected provider account; the token and placeholder never leave the store this way.
type Integration struct {
	ID, OrgID                          uuid.UUID
	Kind                               integrations.Kind
	Source                             integrations.Source
	Scope                              EnvScope
	OwnerID                            *uuid.UUID
	Mode                               integrations.Mode
	Account, ScopesInfo                string
	ProviderExpiresAt, AccessExpiresAt *time.Time
	Status                             string
	LastError                          string
	WasInVM                            bool
	CreatedBy, UpdatedBy               uuid.UUID
	CreatedAt, UpdatedAt               time.Time
	LastSwitchAt                       *time.Time
	LastSwitchFrom, LastSwitchTo       *uuid.UUID // account ids of the last automatic switch
	AutoSwitch                         bool
	QuotaProject                       string
}

// IntegrationSecret is the token to store; Refresh is set only for oauth and adc sources.
type IntegrationSecret struct {
	Token, Refresh  string
	AccessExpiresAt *time.Time
}

// ResolvedIntegration carries the decrypted access token for a VM's desired state.
type ResolvedIntegration struct {
	Integration
	Placeholder, Token string
	Accounts           []ResolvedAccount // Claude only: active first, then the other working ones in position order
}

func (IntegrationSecret) String() string               { return "IntegrationSecret{redacted}" }
func (IntegrationSecret) GoString() string             { return "store.IntegrationSecret{redacted}" }
func (IntegrationSecret) MarshalJSON() ([]byte, error) { return []byte(`"redacted"`), nil }
func (r ResolvedIntegration) String() string {
	return fmt.Sprintf("ResolvedIntegration{%s %s %s}", r.ID, r.Kind, r.Scope)
}
func (r ResolvedIntegration) GoString() string           { return r.String() }
func (ResolvedIntegration) MarshalJSON() ([]byte, error) { return []byte(`"redacted"`), nil }

// A Claude integration shows its active account's source, account, expiry and status.
const integrationCols = `i.id, i.org_id, i.kind, coalesce(a.source, i.source), i.scope, coalesce(i.team_id, i.user_id), i.mode, coalesce(a.account, i.account),
	i.scopes_info, i.provider_expires_at, coalesce(a.access_expires_at, i.access_expires_at), coalesce(a.status, i.status), coalesce(a.last_error, i.last_error),
	i.was_in_vm, i.created_by, i.updated_by, i.created_at, i.updated_at, i.last_switch_at, i.last_switch_from, i.last_switch_to, i.auto_switch, i.quota_project`

const integrationFrom = " FROM integrations i LEFT JOIN integration_accounts a ON a.integration_id = i.id AND a.active "

func scanIntegration(r pgx.Row, extra ...any) (v Integration, err error) {
	dest := append([]any{&v.ID, &v.OrgID, &v.Kind, &v.Source, &v.Scope, &v.OwnerID, &v.Mode, &v.Account, &v.ScopesInfo, &v.ProviderExpiresAt,
		&v.AccessExpiresAt, &v.Status, &v.LastError, &v.WasInVM, &v.CreatedBy, &v.UpdatedBy, &v.CreatedAt, &v.UpdatedAt,
		&v.LastSwitchAt, &v.LastSwitchFrom, &v.LastSwitchTo, &v.AutoSwitch, &v.QuotaProject}, extra...)
	err = r.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return v, err
}

func integrationAAD(org, id uuid.UUID, field string) []byte {
	return []byte("integration:" + org.String() + ":" + id.String() + ":" + field)
}

func refreshes(source integrations.Source) bool {
	return source == integrations.SourceOAuth || source == integrations.SourceADC
}

// checkIntegrationSecret drops an access expiry for sources without a refresh token.
func checkIntegrationSecret(source integrations.Source, sec IntegrationSecret) (IntegrationSecret, error) {
	if refreshes(source) != (sec.Refresh != "") {
		return sec, ErrIntegrationRefresh
	}
	if !refreshes(source) {
		sec.AccessExpiresAt = nil
	}
	return sec, nil
}

// sealIntegration returns nil refresh columns when sec has no refresh token.
func sealIntegration(key seal.Key, org, id uuid.UUID, sec IntegrationSecret) (nonce, ct, rnonce, rct []byte, err error) {
	if nonce, ct, err = seal.Seal(key, []byte(sec.Token), integrationAAD(org, id, "access")); err != nil || sec.Refresh == "" {
		return nonce, ct, nil, nil, err
	}
	rnonce, rct, err = seal.Seal(key, []byte(sec.Refresh), integrationAAD(org, id, "refresh"))
	return nonce, ct, rnonce, rct, err
}

func integrationOwner(v Integration) EnvVar {
	return EnvVar{OrgID: v.OrgID, Scope: v.Scope, OwnerID: v.OwnerID}
}

func (s *Store) Integrations(ctx context.Context, org uuid.UUID) ([]Integration, error) {
	rows, err := s.q.Query(ctx, "SELECT "+integrationCols+integrationFrom+"WHERE i.org_id = $1 ORDER BY i.kind, i.scope, i.id", org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Integration, error) { return scanIntegration(r) })
}

func (s *Store) Integration(ctx context.Context, org, id uuid.UUID) (Integration, error) {
	return scanIntegration(s.q.QueryRow(ctx, "SELECT "+integrationCols+integrationFrom+"WHERE i.org_id = $1 AND i.id = $2", org, id))
}

// IntegrationToken returns the decrypted access token of v, for Claude that of its active account.
func (s *Store) IntegrationToken(ctx context.Context, v Integration) (string, error) {
	key, err := s.orgKey(ctx, v.OrgID)
	if err != nil {
		return "", err
	}
	var id uuid.UUID
	var nonce, ct []byte
	err = s.q.QueryRow(ctx, "SELECT coalesce(a.id, i.id), coalesce(a.nonce, i.nonce), coalesce(a.ciphertext, i.ciphertext)"+integrationFrom+
		"WHERE i.org_id = $1 AND i.id = $2", v.OrgID, v.ID).Scan(&id, &nonce, &ct)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	} else if err != nil {
		return "", err
	}
	plain, err := seal.Open(key, nonce, ct, integrationAAD(v.OrgID, id, "access"))
	return string(plain), err
}

// MarkIntegrationInVM records that v's real token reached a VM, as vm mode does.
func (s *Store) MarkIntegrationInVM(ctx context.Context, v Integration) error {
	_, err := s.q.Exec(ctx, "UPDATE integrations SET was_in_vm = true WHERE org_id = $1 AND id = $2", v.OrgID, v.ID)
	return err
}

func (s *Store) CreateIntegration(ctx context.Context, v Integration, sec IntegrationSecret) (Integration, error) {
	if v.Mode == "" {
		v.Mode = integrations.ModeProxy
	}
	if !integrations.ValidQuotaProject(v.QuotaProject) {
		return Integration{}, ErrInvalidQuotaProject
	}
	sec, err := checkIntegrationSecret(v.Source, sec)
	if err != nil {
		return Integration{}, err
	}
	err = s.InTx(ctx, func(tx *Store) error {
		if err := tx.lockEnvOwner(ctx, integrationOwner(v)); err != nil {
			return err
		}
		key, err := tx.orgKey(ctx, v.OrgID)
		if err != nil {
			return err
		}
		v.ID = uuid.New()
		placeholder, err := egress.NewPlaceholder()
		if err != nil {
			return err
		}
		// Claude tokens live in its accounts; the first account shares the integration's id.
		var source any = v.Source
		account, accessExpiresAt := v.Account, sec.AccessExpiresAt
		var nonce, ct, rnonce, rct []byte
		if v.Kind == integrations.Claude {
			source, account, accessExpiresAt = nil, "", nil
		} else if nonce, ct, rnonce, rct, err = sealIntegration(key, v.OrgID, v.ID, sec); err != nil {
			return err
		}
		teamID, userID := envOwnerColumns(integrationOwner(v))
		err = tx.q.QueryRow(ctx, `
			INSERT INTO integrations (id, org_id, kind, source, scope, team_id, user_id, mode, account, scopes_info, provider_expires_at,
			  nonce, ciphertext, refresh_nonce, refresh_ciphertext, access_expires_at, placeholder, was_in_vm, created_by, updated_by, quota_project)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $8 = 'vm', $18, $18, $19)
			RETURNING status, last_error, was_in_vm, created_at, updated_at`,
			v.ID, v.OrgID, v.Kind, source, v.Scope, teamID, userID, v.Mode, account, v.ScopesInfo, v.ProviderExpiresAt,
			nonce, ct, rnonce, rct, accessExpiresAt, placeholder, v.CreatedBy, v.QuotaProject).Scan(&v.Status, &v.LastError, &v.WasInVM, &v.CreatedAt, &v.UpdatedAt)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		if err != nil || v.Kind != integrations.Claude {
			return err
		}
		return tx.insertAccount(ctx, key, v, v.ID, 0, true, sec)
	})
	if err != nil {
		return Integration{}, err
	}
	v.UpdatedBy, v.AccessExpiresAt = v.CreatedBy, sec.AccessExpiresAt
	return v, nil
}

func integrationGuardArgs(old Integration, rest ...any) []any {
	teamID, userID := envOwnerColumns(integrationOwner(old))
	return append([]any{old.OrgID, old.ID, old.Scope, teamID, userID, old.Kind}, rest...)
}

func (s *Store) integrationWriteResult(ctx context.Context, old Integration, rows int64, err error) error {
	return s.guardedWriteResult(ctx, "integrations", old.OrgID, old.ID, rows, err)
}

// ReplaceIntegrationToken stores a new verified token from sec with v's source and account details, for Claude in its active account; the placeholder stays.
func (s *Store) ReplaceIntegrationToken(ctx context.Context, old, v Integration, sec IntegrationSecret) error {
	sec, err := checkIntegrationSecret(v.Source, sec)
	if err != nil {
		return err
	}
	key, err := s.orgKey(ctx, old.OrgID)
	if err != nil {
		return err
	}
	if old.Kind == integrations.Claude {
		return s.InTx(ctx, func(tx *Store) error { return tx.replaceActiveAccountToken(ctx, key, old, v, sec) })
	}
	nonce, ct, rnonce, rct, err := sealIntegration(key, old.OrgID, old.ID, sec)
	if err != nil {
		return err
	}
	// envGuard fits: integrations has the same org, id, scope, owner and kind columns.
	tag, err := s.q.Exec(ctx, `
		UPDATE integrations SET source = $7, account = $8, scopes_info = $9, provider_expires_at = $10,
		  nonce = $11, ciphertext = $12, refresh_nonce = $13, refresh_ciphertext = $14, access_expires_at = $15,
		  status = 'ok', last_error = '', was_in_vm = (mode = 'vm'), updated_by = $16, updated_at = now()
		WHERE `+envGuard, integrationGuardArgs(old, v.Source, v.Account, v.ScopesInfo, v.ProviderExpiresAt,
		nonce, ct, rnonce, rct, sec.AccessExpiresAt, v.UpdatedBy)...)
	return s.integrationWriteResult(ctx, old, tag.RowsAffected(), err)
}

func (s *Store) replaceActiveAccountToken(ctx context.Context, key seal.Key, old, v Integration, sec IntegrationSecret) error {
	tag, err := s.q.Exec(ctx, `
		UPDATE integrations SET scopes_info = $7, provider_expires_at = $8, was_in_vm = (mode = 'vm'), updated_by = $9, updated_at = now()
		WHERE `+envGuard, integrationGuardArgs(old, v.ScopesInfo, v.ProviderExpiresAt, v.UpdatedBy)...)
	if err := s.integrationWriteResult(ctx, old, tag.RowsAffected(), err); err != nil {
		return err
	}
	var id uuid.UUID
	if err := s.q.QueryRow(ctx, "SELECT id FROM integration_accounts WHERE integration_id = $1 AND active FOR UPDATE", old.ID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	nonce, ct, rnonce, rct, err := sealIntegration(key, old.OrgID, id, sec)
	if err != nil {
		return err
	}
	_, err = s.q.Exec(ctx, `
		UPDATE integration_accounts SET source = $2, account = $3, nonce = $4, ciphertext = $5, refresh_nonce = $6, refresh_ciphertext = $7,
		  access_expires_at = $8, status = 'ok', last_error = '', updated_by = $9, updated_at = now()
		WHERE id = $1`, id, v.Source, v.Account, nonce, ct, rnonce, rct, sec.AccessExpiresAt, v.UpdatedBy)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// DueOAuthRefreshes lists working oauth accounts whose access token expires before before.
func (s *Store) DueOAuthRefreshes(ctx context.Context, before time.Time) ([]AccountRef, error) {
	return s.dueRefreshes(ctx, "integration_accounts", integrations.SourceOAuth, before)
}

// DueADCRefreshes lists working adc integrations whose access token expires before before.
func (s *Store) DueADCRefreshes(ctx context.Context, before time.Time) ([]AccountRef, error) {
	return s.dueRefreshes(ctx, "integrations", integrations.SourceADC, before)
}

func (s *Store) dueRefreshes(ctx context.Context, table string, source integrations.Source, before time.Time) ([]AccountRef, error) {
	rows, err := s.q.Query(ctx, `SELECT org_id, id FROM `+table+`
		WHERE source = $1 AND status = 'ok' AND access_expires_at < $2 ORDER BY access_expires_at`, source, before)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[AccountRef])
}

// RefreshIntegration runs refresh under the row lock, since the provider rotates refresh tokens; false, nil when the row is locked elsewhere or no longer due.
func (s *Store) RefreshIntegration(ctx context.Context, ref AccountRef, before time.Time,
	refresh func(ctx context.Context, refreshToken string) (integrations.OAuthTokens, error)) (bool, error) {
	return s.refreshRow(ctx, "integration_accounts", integrations.SourceOAuth, ref, before, refresh)
}

// RefreshADCIntegration is RefreshIntegration for a gcloud integration; the sealed ADC blob is kept, since Google does not rotate it.
func (s *Store) RefreshADCIntegration(ctx context.Context, ref AccountRef, before time.Time,
	refresh func(ctx context.Context, adc string) (integrations.OAuthTokens, error)) (bool, error) {
	return s.refreshRow(ctx, "integrations", integrations.SourceADC, ref, before, refresh)
}

func (s *Store) refreshRow(ctx context.Context, table string, source integrations.Source, ref AccountRef, before time.Time,
	refresh func(ctx context.Context, refreshToken string) (integrations.OAuthTokens, error)) (bool, error) {
	var refreshed bool
	var refreshErr error
	// Once the provider has rotated the refresh token, cancelling ctx must not roll back the write; it gets its own 10 s instead.
	dbctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	var deadline *time.Timer
	err := s.InTx(dbctx, func(tx *Store) error {
		var rnonce, rct []byte
		err := tx.q.QueryRow(ctx, `SELECT refresh_nonce, refresh_ciphertext FROM `+table+`
			WHERE org_id = $1 AND id = $2 AND source = $4 AND status = 'ok' AND access_expires_at < $3
			FOR UPDATE SKIP LOCKED`, ref.OrgID, ref.ID, before, source).Scan(&rnonce, &rct)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		key, err := tx.orgKey(ctx, ref.OrgID)
		if err != nil {
			return err
		}
		old, err := seal.Open(key, rnonce, rct, integrationAAD(ref.OrgID, ref.ID, "refresh"))
		if err != nil {
			return fmt.Errorf("decrypt integration %s: %w", ref.ID, err)
		}
		t, err := refresh(ctx, string(old))
		deadline = time.AfterFunc(10*time.Second, cancel)
		if source == integrations.SourceADC {
			t.Refresh = string(old)
		}
		var sec IntegrationSecret
		if err == nil {
			sec, err = checkIntegrationSecret(source, IntegrationSecret{Token: t.Access, Refresh: t.Refresh, AccessExpiresAt: &t.ExpiresAt})
		}
		if refreshErr = err; err != nil {
			// Our own shutdown or timeout must never make the one-way switch to expired.
			ours := ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
			_, err := tx.q.Exec(dbctx, `UPDATE `+table+` SET
				status = CASE WHEN $3 OR ($4 AND access_expires_at <= now()) THEN 'expired' ELSE 'ok' END,
				last_error = CASE WHEN $3 OR ($4 AND access_expires_at <= now()) THEN 'sign in again' ELSE 'refresh failed' END
				WHERE org_id = $1 AND id = $2`, ref.OrgID, ref.ID, errors.Is(err, integrations.ErrInvalidGrant), !ours)
			return err
		}
		nonce, ct, rnonce, rct, err := sealIntegration(key, ref.OrgID, ref.ID, sec)
		if err != nil {
			return err
		}
		_, err = tx.q.Exec(dbctx, `UPDATE `+table+` SET nonce = $3, ciphertext = $4, refresh_nonce = $5, refresh_ciphertext = $6,
			access_expires_at = $7, last_error = '' WHERE org_id = $1 AND id = $2`, ref.OrgID, ref.ID, nonce, ct, rnonce, rct, sec.AccessExpiresAt)
		refreshed = err == nil
		return err
	})
	if deadline != nil {
		deadline.Stop()
	}
	if err != nil {
		return false, err
	}
	return refreshed, refreshErr
}

// SetIntegrationMode remembers a past vm mode, since the real token may still be in the guest.
func (s *Store) SetIntegrationMode(ctx context.Context, old Integration, mode integrations.Mode, by uuid.UUID) error {
	return s.InTx(ctx, func(tx *Store) error {
		if mode == integrations.ModeVM {
			if err := tx.lockIntegration(ctx, old.OrgID, old.ID); err != nil {
				return err
			}
			var n int
			if err := tx.q.QueryRow(ctx, "SELECT count(*) FROM integration_accounts WHERE integration_id = $1", old.ID).Scan(&n); err != nil {
				return err
			}
			if n > 1 {
				return ErrMultiAccountNotAllowed
			}
		}
		tag, err := tx.q.Exec(ctx, `
			UPDATE integrations SET mode = $7, was_in_vm = was_in_vm OR $7 = 'vm', updated_by = $8, updated_at = now()
			WHERE `+envGuard, integrationGuardArgs(old, mode, by)...)
		return tx.integrationWriteResult(ctx, old, tag.RowsAffected(), err)
	})
}

// SetIntegrationAutoSwitch turns on or off whether the hub and the proxy switch old's accounts on a limit.
func (s *Store) SetIntegrationAutoSwitch(ctx context.Context, old Integration, on bool, by uuid.UUID) error {
	tag, err := s.q.Exec(ctx, "UPDATE integrations SET auto_switch = $7, updated_by = $8, updated_at = now() WHERE "+envGuard,
		integrationGuardArgs(old, on, by)...)
	return s.integrationWriteResult(ctx, old, tag.RowsAffected(), err)
}

// SetIntegrationQuotaProject sets the project gcloud bills API quota to; empty leaves it to the client.
func (s *Store) SetIntegrationQuotaProject(ctx context.Context, old Integration, project string, by uuid.UUID) error {
	if !integrations.ValidQuotaProject(project) {
		return ErrInvalidQuotaProject
	}
	tag, err := s.q.Exec(ctx, "UPDATE integrations SET quota_project = $7, updated_by = $8, updated_at = now() WHERE "+envGuard,
		integrationGuardArgs(old, project, by)...)
	return s.integrationWriteResult(ctx, old, tag.RowsAffected(), err)
}

func (s *Store) DeleteIntegration(ctx context.Context, old Integration) error {
	tag, err := s.q.Exec(ctx, "DELETE FROM integrations WHERE "+envGuard, integrationGuardArgs(old)...)
	return s.integrationWriteResult(ctx, old, tag.RowsAffected(), err)
}

// ResolveVMIntegrations returns, per kind sorted, the integration owner may use with its decrypted access token; owner's own scope beats org.
func (s *Store) ResolveVMIntegrations(ctx context.Context, e *authz.Enforcer, org uuid.UUID, owner string) ([]ResolvedIntegration, error) {
	scope, idStr, ok := strings.Cut(owner, ":")
	id, idErr := uuid.Parse(idStr)
	if !ok || idErr != nil || (scope != "user" && scope != "team") {
		return nil, fmt.Errorf("unsupported integration owner %q", owner)
	}
	ownScope, ownObj := EnvScopeUser, authz.UserIntegrationsObject(id)
	if scope == "team" {
		ownScope, ownObj = EnvScopeTeam, authz.TeamIntegrationsObject(id)
	}
	allowed := map[EnvScope]bool{
		EnvScopeOrg: e.Allowed(owner, org, authz.ObjIntegrations, "secrets.integrations.use"),
		ownScope:    e.Allowed(owner, org, ownObj, "secrets.integrations.use"),
	}
	if !allowed[EnvScopeOrg] && !allowed[ownScope] {
		return nil, nil
	}
	rows, err := s.q.Query(ctx, "SELECT "+integrationCols+", i.placeholder, i.nonce, i.ciphertext"+integrationFrom+
		"WHERE i.org_id = $1 AND (i.scope = 'org' OR (i.scope = $2 AND coalesce(i.team_id, i.user_id) = $3))", org, ownScope, id)
	if err != nil {
		return nil, err
	}
	type row struct {
		ResolvedIntegration
		nonce, ct []byte
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (x row, err error) {
		x.Integration, err = scanIntegration(r, &x.Placeholder, &x.nonce, &x.ct)
		return
	})
	if err != nil {
		return nil, err
	}
	winners := map[integrations.Kind]row{}
	for _, r := range list {
		if !allowed[r.Scope] {
			continue
		}
		if w, ok := winners[r.Kind]; !ok || w.Scope == EnvScopeOrg {
			winners[r.Kind] = r
		}
	}
	if len(winners) == 0 {
		return nil, nil
	}
	key, err := s.orgKey(ctx, org)
	if err != nil {
		return nil, err
	}
	var out []ResolvedIntegration
	for _, k := range slices.Sorted(maps.Keys(winners)) {
		w := winners[k]
		if k == integrations.Claude {
			if w.Accounts, err = s.resolvedAccounts(ctx, key, org, w.ID); err != nil {
				return nil, err
			}
			if len(w.Accounts) == 0 {
				return nil, fmt.Errorf("integration %s has no active account", w.ID)
			}
			// Without automatic switching the proxy must not retry with, or even hold, the other accounts' tokens.
			if !w.AutoSwitch {
				w.Accounts = w.Accounts[:1]
			}
			w.Token = w.Accounts[0].Token
			out = append(out, w.ResolvedIntegration)
			continue
		}
		plain, err := seal.Open(key, w.nonce, w.ct, integrationAAD(org, w.ID, "access"))
		if err != nil {
			return nil, fmt.Errorf("decrypt integration %s: %w", w.ID, err)
		}
		w.Token = string(plain)
		out = append(out, w.ResolvedIntegration)
	}
	return out, nil
}
