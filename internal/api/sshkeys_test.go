package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

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
	body, _ := json.Marshal(map[string]string{"name": "laptop", "publicKey": key})

	if rec := e.send("POST", "/api/me/ssh-keys", tok, `{"name":"x","publicKey":"nope"}`); rec.Code != 400 {
		t.Fatalf("invalid key: %d", rec.Code)
	}
	if rec := e.send("POST", "/api/me/ssh-keys", tok, `{"name":"","publicKey":"`+key+`"}`); rec.Code != 400 {
		t.Fatalf("empty name: %d", rec.Code)
	}
	rec := e.send("POST", "/api/me/ssh-keys", tok, string(body))
	if rec.Code != 201 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	var added struct{ ID, Name, Fingerprint string }
	decode(t, rec, &added)
	if added.Name != "laptop" || !strings.HasPrefix(added.Fingerprint, "SHA256:") {
		t.Fatalf("%+v", added)
	}
	if rec := e.send("POST", "/api/me/ssh-keys", tok, string(body)); rec.Code != 409 {
		t.Fatalf("duplicate: %d", rec.Code)
	}

	var list []struct{ ID, Name, Fingerprint string }
	decode(t, e.do("GET", "/api/me/ssh-keys", tok), &list)
	if len(list) != 1 || list[0].ID != added.ID {
		t.Fatalf("list: %+v", list)
	}
	decode(t, e.do("GET", "/api/me/ssh-keys", bobTok), &list)
	if len(list) != 0 {
		t.Fatalf("bob sees %+v", list)
	}
	if rec := e.do("DELETE", "/api/me/ssh-keys/"+added.ID, bobTok); rec.Code != 404 {
		t.Fatalf("bob delete: %d", rec.Code)
	}
	if rec := e.do("DELETE", "/api/me/ssh-keys/"+added.ID, tok); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}

	keyBody := strings.Fields(key)[1]
	for _, action := range []string{"iam.sshKeys.create", "iam.sshKeys.delete"} {
		es := e.audit(action)
		if len(es) != 1 || es[0].OrgID != nil || es[0].ActorID != alice.ID.String() || len(es[0].Details) != 2 ||
			es[0].Details["name"] != "laptop" || es[0].Details["fingerprint"] != added.Fingerprint {
			t.Fatalf("%s audit: %+v", action, es)
		}
		if strings.Contains(es[0].Resource+strings.Join(detailStrings(es[0].Details), " "), keyBody) {
			t.Fatalf("%s audit leaks the key", action)
		}
	}
}
