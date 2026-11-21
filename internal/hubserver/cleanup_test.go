package hubserver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/db/dbtest"
	"github.com/asterism-dev/observatory/internal/integrations"
	"github.com/asterism-dev/observatory/internal/platform/bus"
	"github.com/asterism-dev/observatory/internal/seal"
	"github.com/asterism-dev/observatory/internal/store"
)

type cleanupEnv struct {
	t    *testing.T
	pool *pgxpool.Pool
	s    *store.Store
	js   jetstream.JetStream
	org  store.Org

	mu       sync.Mutex
	notified []uuid.UUID
}

func newCleanupEnv(t *testing.T) *cleanupEnv {
	t.Helper()
	ctx := context.Background()
	url, token, stop, err := bus.Embedded(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	js, closeConn, err := bus.Connect(ctx, url, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeConn)
	if err := bus.EnsureStream(ctx, js); err != nil {
		t.Fatal(err)
	}
	pool := dbtest.New(t)
	e := &cleanupEnv{t: t, pool: pool, s: store.New(pool).WithMasterKey(seal.Key{1}), js: js}
	e.org, err = e.s.CreateOrg(ctx, "acme", e.user("owner"))
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- consumeIAMEvents(cctx, js, e.s, func(org uuid.UUID) {
			e.mu.Lock()
			e.notified = append(e.notified, org)
			e.mu.Unlock()
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return e
}

func (e *cleanupEnv) user(sub string) uuid.UUID {
	e.t.Helper()
	res, err := e.s.Login(context.Background(), store.Identity{Subject: sub, Email: sub + "@x.io", Name: sub})
	if err != nil {
		e.t.Fatal(err)
	}
	return res.User.ID
}

func (e *cleanupEnv) member(sub string) uuid.UUID {
	e.t.Helper()
	id := e.user(sub)
	e.exec("INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'member')", e.org.ID, id)
	return id
}

func (e *cleanupEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *cleanupEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// seed gives the user- or team-scoped owner one row of every kind a purge removes.
func (e *cleanupEnv) seed(scope store.EnvScope, owner uuid.UUID, by uuid.UUID) {
	e.t.Helper()
	ctx := context.Background()
	org := e.org.ID
	subject := authz.UserSubject(owner)
	if scope == store.EnvScopeTeam {
		subject = authz.TeamSubject(owner)
	}
	if _, err := e.s.CreateEnvVar(ctx, store.EnvVar{OrgID: org, Kind: store.EnvPlain, Scope: scope, OwnerID: &owner, Name: "V", CreatedBy: by, UpdatedBy: by}, "v"); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.CreateIntegration(ctx, store.Integration{OrgID: org, Kind: integrations.GitHub, Source: integrations.SourcePAT, Scope: scope, OwnerID: &owner, CreatedBy: by, UpdatedBy: by},
		store.IntegrationSecret{Token: "ghp_dummy0000000000000000"}); err != nil {
		e.t.Fatal(err)
	}
	img, err := e.s.CreateImage(ctx, store.Image{OrgID: org, Scope: scope, OwnerID: &owner, Name: "img-" + owner.String(), CreatedBy: by})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.s.SetImageDefault(ctx, store.ImageDefault{OrgID: org, Scope: scope, OwnerID: &owner, ImageID: img.ID, Tag: "v1"}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.CreateDashboard(ctx, store.Dashboard{OrgID: org, Scope: scope, OwnerID: &owner, Name: "d", CreatedBy: by}); err != nil {
		e.t.Fatal(err)
	}
	one := 1
	if err := e.s.SetLimits(ctx, org, subject, store.Limits{VMCount: &one}, func(string) bool { return true }); err != nil {
		e.t.Fatal(err)
	}
	if scope == store.EnvScopeUser {
		if _, err := e.s.CreateDevice(ctx, store.Device{OrgID: org, UserID: owner, Name: "laptop", PublicKey: "key-" + owner.String()}); err != nil {
			e.t.Fatal(err)
		}
	}
}

// rows counts the scoped rows owner still has, by table.
func (e *cleanupEnv) rows(scope store.EnvScope, owner uuid.UUID) map[string]int {
	e.t.Helper()
	col, subject := "user_id", authz.UserSubject(owner)
	if scope == store.EnvScopeTeam {
		col, subject = "team_id", authz.TeamSubject(owner)
	}
	out := map[string]int{}
	for _, table := range []string{"env_vars", "integrations", "images", "image_defaults", "dashboards"} {
		out[table] = e.count(fmt.Sprintf("SELECT count(*) FROM %s WHERE org_id = $1 AND scope = $2 AND %s = $3", table, col), e.org.ID, scope, owner)
	}
	out["limits"] = e.count("SELECT count(*) FROM limits WHERE org_id = $1 AND scope = $2", e.org.ID, subject)
	if scope == store.EnvScopeUser {
		out["devices"] = e.count("SELECT count(*) FROM devices WHERE org_id = $1 AND user_id = $2", e.org.ID, owner)
	}
	return out
}

func total(rows map[string]int) int {
	n := 0
	for _, c := range rows {
		n += c
	}
	return n
}

func (e *cleanupEnv) publish(typ string, org *uuid.UUID, resource string, result eventsv1.Result) {
	e.t.Helper()
	ev := &eventsv1.Event{Id: uuid.Must(uuid.NewV7()).String(), Time: timestamppb.Now(), Type: typ, Resource: resource, Result: result}
	scope := "platform"
	if org != nil {
		ev.Org, scope = org.String(), org.String()
	}
	subject := "events." + scope + "." + typ
	data, err := proto.Marshal(ev)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.js.Publish(context.Background(), subject, data); err != nil {
		e.t.Fatal(err)
	}
}

func (e *cleanupEnv) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settled waits until the consumer has acked everything published so far.
func (e *cleanupEnv) settled() {
	e.t.Helper()
	e.waitFor("the consumer to ack every event", func() bool {
		c, err := e.js.Consumer(context.Background(), bus.Stream, "hub")
		if err != nil {
			return false
		}
		info, err := c.Info(context.Background())
		return err == nil && info.NumPending == 0 && info.NumAckPending == 0
	})
}

func (e *cleanupEnv) notifications() []uuid.UUID {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.notified)
}

func memberResource(id uuid.UUID) string { return "org/members/" + id.String() }

func TestMemberDeleteEventPurges(t *testing.T) {
	e := newCleanupEnv(t)
	bob, carol := e.member("bob"), e.member("carol")
	e.seed(store.EnvScopeUser, bob, bob)
	e.seed(store.EnvScopeUser, carol, carol)
	before := e.rows(store.EnvScopeUser, bob)
	for table, n := range before {
		if n != 1 {
			t.Fatalf("seed %s: %d rows", table, n)
		}
	}

	e.publish(store.ActionMemberRemove, &e.org.ID, memberResource(bob), eventsv1.Result_RESULT_ALLOWED)
	e.waitFor("bob's rows to go", func() bool { return total(e.rows(store.EnvScopeUser, bob)) == 0 })
	if got := e.rows(store.EnvScopeUser, carol); total(got) != len(before) {
		t.Fatalf("carol's rows: %v", got)
	}
	e.waitFor("the org to be notified", func() bool { return slices.Contains(e.notifications(), e.org.ID) })
}

func TestLeaveEventPurges(t *testing.T) {
	e := newCleanupEnv(t)
	bob := e.member("bob")
	e.seed(store.EnvScopeUser, bob, bob)
	e.publish(store.ActionMemberLeave, &e.org.ID, memberResource(bob), eventsv1.Result_RESULT_ALLOWED)
	e.waitFor("bob's rows to go", func() bool { return total(e.rows(store.EnvScopeUser, bob)) == 0 })
}

func TestTeamDeleteEventPurges(t *testing.T) {
	e := newCleanupEnv(t)
	ctx := context.Background()
	owner := e.user("owner")
	core, err := e.s.CreateTeam(ctx, e.org.ID, "core")
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.s.CreateTeam(ctx, e.org.ID, "other")
	if err != nil {
		t.Fatal(err)
	}
	e.seed(store.EnvScopeTeam, core.ID, owner)
	e.seed(store.EnvScopeTeam, other.ID, owner)
	e.exec("INSERT INTO disks (org_id, team_id, name, size_gb, deleted_at) VALUES ($1, $2, 'gone', 1, now())", e.org.ID, core.ID)
	e.exec(`INSERT INTO vm_history (id, org_id, team_id, name, cpus, memory_mb, disk_gb, created_at, reason)
		VALUES ($1, $2, $3, 'old', 1, 512, 10, now(), 'user')`, uuid.New(), e.org.ID, core.ID)

	e.publish(store.ActionTeamDelete, &e.org.ID, authz.TeamObject(core.ID), eventsv1.Result_RESULT_ALLOWED)
	e.waitFor("core's rows to go", func() bool { return total(e.rows(store.EnvScopeTeam, core.ID)) == 0 })
	if n := e.count("SELECT count(*) FROM disks WHERE team_id = $1", core.ID) + e.count("SELECT count(*) FROM vm_history WHERE team_id = $1", core.ID); n != 0 {
		t.Fatalf("%d disk or history rows still name the team", n)
	}
	if got := e.rows(store.EnvScopeTeam, other.ID); total(got) != 6 {
		t.Fatalf("other team's rows: %v", got)
	}
	e.waitFor("the org to be notified", func() bool { return slices.Contains(e.notifications(), e.org.ID) })
}

func TestDeniedEventIgnored(t *testing.T) {
	e := newCleanupEnv(t)
	bob, carol := e.member("bob"), e.member("carol")
	e.seed(store.EnvScopeUser, bob, bob)
	e.seed(store.EnvScopeUser, carol, carol)
	for _, res := range []eventsv1.Result{eventsv1.Result_RESULT_DENIED, eventsv1.Result_RESULT_FAILED, eventsv1.Result_RESULT_UNSPECIFIED} {
		e.publish(store.ActionMemberRemove, &e.org.ID, memberResource(bob), res)
	}
	// Unparsable resources and an event whose org disagrees with its subject are dropped, not retried.
	e.publish(store.ActionMemberRemove, &e.org.ID, "org/members/"+strings.ToUpper(bob.String()), eventsv1.Result_RESULT_ALLOWED)
	e.publish(store.ActionMemberRemove, &e.org.ID, "org/teams/"+bob.String(), eventsv1.Result_RESULT_ALLOWED)
	e.publish(store.ActionMemberRemove, nil, memberResource(bob), eventsv1.Result_RESULT_ALLOWED)
	e.publish(store.ActionMemberRemove, &e.org.ID, memberResource(carol), eventsv1.Result_RESULT_ALLOWED)
	e.waitFor("carol's rows to go", func() bool { return total(e.rows(store.EnvScopeUser, carol)) == 0 })
	e.settled()
	if got := e.rows(store.EnvScopeUser, bob); total(got) != len(got) {
		t.Fatalf("bob's rows after ignored events: %v", got)
	}
}

func TestDuplicateEventHarmless(t *testing.T) {
	e := newCleanupEnv(t)
	bob := e.member("bob")
	e.seed(store.EnvScopeUser, bob, bob)
	for range 2 {
		e.publish(store.ActionMemberRemove, &e.org.ID, memberResource(bob), eventsv1.Result_RESULT_ALLOWED)
	}
	e.settled()
	if got := e.rows(store.EnvScopeUser, bob); total(got) != 0 {
		t.Fatalf("bob's rows: %v", got)
	}
	c, err := e.js.Consumer(context.Background(), bus.Stream, "hub")
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Info(context.Background())
	if err != nil || info.NumRedelivered != 0 || info.Delivered.Consumer != 2 {
		t.Fatalf("consumer: %+v %v", info, err)
	}
}

func TestSessionRevokeDropsRegistryCredentials(t *testing.T) {
	e := newCleanupEnv(t)
	ctx := context.Background()
	bob := e.user("bob")
	sessions := map[string]uuid.UUID{}
	for _, action := range []string{store.ActionSessionRevoke, store.ActionAuthLogout, "keep"} {
		_, sess, err := e.s.CreateSession(ctx, bob, store.SessionCLI, time.Hour, "test", "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.s.CreateRegistryCredential(ctx, sess.ID, time.Hour); err != nil {
			t.Fatal(err)
		}
		sessions[action] = sess.ID
	}
	creds := func(action string) int {
		return e.count("SELECT count(*) FROM registry_credentials WHERE session_id = $1", sessions[action])
	}
	e.publish(store.ActionSessionRevoke, nil, "session/"+sessions[store.ActionSessionRevoke].String(), eventsv1.Result_RESULT_ALLOWED)
	e.publish(store.ActionAuthLogout, nil, "session/"+sessions[store.ActionAuthLogout].String(), eventsv1.Result_RESULT_ALLOWED)
	e.waitFor("the credentials to go", func() bool { return creds(store.ActionSessionRevoke)+creds(store.ActionAuthLogout) == 0 })
	if creds("keep") != 1 {
		t.Fatal("another session's credential was dropped")
	}
}
