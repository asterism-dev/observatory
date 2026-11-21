package iam

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/internal/store"
)

func TestGetMeOrgsFromPolicy(t *testing.T) {
	e := newEnv(t)
	u, tok := e.user("alice")
	bob, bobTok := e.user("bob")
	org := e.createOrg(bobTok, "Acme")

	me := must[iamv1.GetMeResponse](t)(e.client.GetMe(context.Background(), req(tok, &iamv1.GetMeRequest{})))
	if me.User.Id != u.ID.String() || me.User.Email != "alice@x.io" || me.User.Name != "alice" || !me.User.PlatformAdmin {
		t.Fatalf("user: %+v", me.User)
	}
	if len(me.Orgs) != 1 || !me.Orgs[0].Personal || me.Orgs[0].Role != "owner" || me.Orgs[0].Id != e.personalOrg(u).String() {
		t.Fatalf("orgs: %+v", me.Orgs)
	}
	if bobMe := must[iamv1.GetMeResponse](t)(e.client.GetMe(context.Background(), req(bobTok, &iamv1.GetMeRequest{}))); bobMe.User.PlatformAdmin || bobMe.User.Id != bob.ID.String() {
		t.Fatalf("bob: %+v", bobMe.User)
	}

	// A membership the policy does not have yet is not listed.
	e.exec(`INSERT INTO memberships (org_id, user_id, role) VALUES ($1, $2, 'admin')`, org, u.ID)
	if me := must[iamv1.GetMeResponse](t)(e.client.GetMe(context.Background(), req(tok, &iamv1.GetMeRequest{}))); len(me.Orgs) != 1 {
		t.Fatalf("before reload: %+v", me.Orgs)
	}
	e.reload()
	me = must[iamv1.GetMeResponse](t)(e.client.GetMe(context.Background(), req(tok, &iamv1.GetMeRequest{})))
	if len(me.Orgs) != 2 || !me.Orgs[0].Personal || me.Orgs[1].Id != org.String() || me.Orgs[1].Name != "Acme" || me.Orgs[1].Role != "admin" {
		t.Fatalf("after reload: %+v", me.Orgs)
	}

	_, err := e.client.GetMe(context.Background(), req("nope", &iamv1.GetMeRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
}

func TestListAndRevokeSessions(t *testing.T) {
	e := newEnv(t)
	u, tok1 := e.user("alice")
	_, sessTok2 := e.session(u)

	list := must[iamv1.ListSessionsResponse](t)(e.client.ListSessions(context.Background(), req(tok1, &iamv1.ListSessionsRequest{}))).Sessions
	if len(list) != 2 {
		t.Fatalf("sessions: %+v", list)
	}
	var current int
	var other string
	for _, s := range list {
		if s.Current {
			current++
		} else {
			other = s.Id
		}
		if s.Kind != "browser" || s.UserAgent != "ua" || s.Ip != "1.2.3.4" || s.CreatedAt == nil || s.LastUsedAt == nil || s.ExpiresAt == nil {
			t.Fatalf("session: %+v", s)
		}
	}
	if current != 1 || other == "" {
		t.Fatalf("current flags: %+v", list)
	}

	must[iamv1.RevokeSessionResponse](t)(e.client.RevokeSession(context.Background(), req(tok1, &iamv1.RevokeSessionRequest{Session: other})))
	if es := e.audit(store.ActionSessionRevoke); len(es) != 1 || es[0].Resource != "session/"+other || es[0].ActorID != u.ID.String() || es[0].Result != store.ResultAllowed {
		t.Fatalf("audit: %+v", es)
	}
	if _, err := e.store.SessionByToken(context.Background(), sessTok2); err == nil {
		t.Fatal("revoked session still valid")
	}
	_, err := e.client.RevokeSession(context.Background(), req(tok1, &iamv1.RevokeSessionRequest{Session: other}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestRevokeOtherUsersSessionNotFound(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	bob, bobTok := e.user("bob")
	_, bobSess := e.session(bob)
	bobSessions := must[iamv1.ListSessionsResponse](t)(e.client.ListSessions(context.Background(), req(bobTok, &iamv1.ListSessionsRequest{}))).Sessions
	for _, s := range bobSessions {
		_, err := e.client.RevokeSession(context.Background(), req(tok, &iamv1.RevokeSessionRequest{Session: s.Id}))
		wantCode(t, err, connect.CodeNotFound)
	}
	if _, err := e.store.SessionByToken(context.Background(), bobSess); err != nil {
		t.Fatalf("bob's session gone: %v", err)
	}
	if es := e.audit(store.ActionSessionRevoke); len(es) != 0 {
		t.Fatalf("audit: %+v", es)
	}
	_, err := e.client.RevokeSession(context.Background(), req(tok, &iamv1.RevokeSessionRequest{Session: "not-a-uuid"}))
	wantCode(t, err, connect.CodeInvalidArgument)
}

func newPublicKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp)))
}

func TestSSHKeys(t *testing.T) {
	e := newEnv(t)
	alice, tok := e.user("alice")
	_, bobTok := e.user("bob")
	key := newPublicKey(t)
	ctx := context.Background()

	_, err := e.client.CreateSSHKey(ctx, req(tok, &iamv1.CreateSSHKeyRequest{Name: "x", PublicKey: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = e.client.CreateSSHKey(ctx, req(tok, &iamv1.CreateSSHKeyRequest{Name: "", PublicKey: key}))
	wantCode(t, err, connect.CodeInvalidArgument)
	added := must[iamv1.CreateSSHKeyResponse](t)(e.client.CreateSSHKey(ctx, req(tok, &iamv1.CreateSSHKeyRequest{Name: "laptop", PublicKey: key}))).Key
	if added.Name != "laptop" || !strings.HasPrefix(added.Fingerprint, "SHA256:") || added.CreatedAt == nil {
		t.Fatalf("%+v", added)
	}
	_, err = e.client.CreateSSHKey(ctx, req(tok, &iamv1.CreateSSHKeyRequest{Name: "laptop", PublicKey: key}))
	wantCode(t, err, connect.CodeFailedPrecondition)

	list := must[iamv1.IAMServiceListSSHKeysResponse](t)(e.client.ListSSHKeys(ctx, req(tok, &iamv1.IAMServiceListSSHKeysRequest{}))).Keys
	if len(list) != 1 || list[0].Id != added.Id {
		t.Fatalf("list: %+v", list)
	}
	if list := must[iamv1.IAMServiceListSSHKeysResponse](t)(e.client.ListSSHKeys(ctx, req(bobTok, &iamv1.IAMServiceListSSHKeysRequest{}))).Keys; len(list) != 0 {
		t.Fatalf("bob sees %+v", list)
	}
	must[iamv1.DeleteSSHKeyResponse](t)(e.client.DeleteSSHKey(ctx, req(tok, &iamv1.DeleteSSHKeyRequest{Key: added.Id})))

	keyBody := strings.Fields(key)[1]
	for _, action := range []string{store.ActionSSHKeyAdd, store.ActionSSHKeyDelete} {
		es := e.audit(action)
		if len(es) != 1 || es[0].OrgID != nil || es[0].ActorID != alice.ID.String() || len(es[0].Details) != 2 ||
			es[0].Resource != "user:"+alice.ID.String()+"/ssh-keys/"+added.Id ||
			es[0].Details["name"] != "laptop" || es[0].Details["fingerprint"] != added.Fingerprint {
			t.Fatalf("%s audit: %+v", action, es)
		}
		if strings.Contains(es[0].Resource+es[0].Details["name"].(string), keyBody) {
			t.Fatalf("%s audit leaks the key", action)
		}
	}
}

func TestDeleteOtherUsersSSHKeyNotFound(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	_, bobTok := e.user("bob")
	ctx := context.Background()
	key := must[iamv1.CreateSSHKeyResponse](t)(e.client.CreateSSHKey(ctx, req(tok, &iamv1.CreateSSHKeyRequest{Name: "laptop", PublicKey: newPublicKey(t)}))).Key
	_, err := e.client.DeleteSSHKey(ctx, req(bobTok, &iamv1.DeleteSSHKeyRequest{Key: key.Id}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = e.client.DeleteSSHKey(ctx, req(tok, &iamv1.DeleteSSHKeyRequest{Key: uuid.NewString()}))
	wantCode(t, err, connect.CodeNotFound)
	if list := must[iamv1.IAMServiceListSSHKeysResponse](t)(e.client.ListSSHKeys(ctx, req(tok, &iamv1.IAMServiceListSSHKeysRequest{}))).Keys; len(list) != 1 {
		t.Fatalf("key gone: %+v", list)
	}
	if es := e.audit(store.ActionSSHKeyDelete); len(es) != 0 {
		t.Fatalf("audit: %+v", es)
	}
}
