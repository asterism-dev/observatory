package api

import (
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/store"
)

type deleteAtTest struct {
	ID       string
	DeleteAt *time.Time
}

type historyTest struct {
	VMs []struct {
		ID, Name, Reason string
		Owner            struct{ Kind, ID, Name string }
		CreatedBy        struct{ ID, Name string }
		DeletedBy        *struct{ ID, Name string }
	}
}

func (e *env) history(tok, org string) historyTest {
	e.t.Helper()
	rec := e.do("GET", "/api/orgs/"+org+"/vms/history", tok)
	if rec.Code != 200 {
		e.t.Fatalf("history: %d %s", rec.Code, rec.Body.String())
	}
	var h historyTest
	decode(e.t, rec, &h)
	return h
}

func withDeleteAfter(body, v string) string {
	return body[:len(body)-1] + `,"deleteAfter":"` + v + `"}`
}

func TestVMCreateDeleteAfter(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	node := e.enrolledNode(tok, org, "n1")
	rec := e.send("POST", vmsPath(org), tok, withDeleteAfter(vmBody("a", userOwner(), node), "2h"))
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var got deleteAtTest
	decode(t, rec, &got)
	if got.DeleteAt == nil || time.Until(*got.DeleteAt) < 119*time.Minute || time.Until(*got.DeleteAt) > 121*time.Minute {
		t.Fatalf("deleteAt: %+v", got)
	}
	for _, bad := range []string{"4m", "2w"} {
		if rec := e.send("POST", vmsPath(org), tok, withDeleteAfter(vmBody("b", userOwner(), node), bad)); rec.Code != 400 {
			t.Fatalf("%s: %d", bad, rec.Code)
		}
	}
	if l := e.listVMs(tok, org); len(l.VMs) != 1 {
		t.Fatalf("invalid deleteAfter created a VM: %+v", l.VMs)
	}
}

func TestSetDeleteAfter(t *testing.T) {
	e := newEnv(t)
	_, tok := e.user("alice")
	org := e.createOrg(tok, "Acme")
	v := e.createVM(tok, org, vmBody("a", userOwner(), e.enrolledNode(tok, org, "n1")))
	path := vmsPath(org) + "/" + v.ID + "/delete-after"
	rec := e.send("PUT", path, tok, `{"deleteAfter":"1d"}`)
	var got deleteAtTest
	if decode(t, rec, &got); rec.Code != 200 || got.DeleteAt == nil {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.send("PUT", path, tok, `{"deleteAfter":null}`)
	got = deleteAtTest{}
	if decode(t, rec, &got); rec.Code != 200 || got.DeleteAt != nil {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.send("PUT", path, tok, `{"deleteAfter":"3m"}`); rec.Code != 400 {
		t.Fatalf("3m: %d", rec.Code)
	}
	if es := e.audit("vm:delete-after"); len(es) != 2 || es[0].Details["deleteAt"] != nil || es[1].Details["deleteAt"] == nil {
		t.Fatalf("audit: %+v", es)
	}
}

func TestSetDeleteAfterPermission(t *testing.T) {
	e := newEnv(t)
	_, ownerTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	org := e.createOrg(ownerTok, "Acme")
	e.addMember(org, bob, store.RoleMember)
	e.addMember(org, carol, store.RoleMember)
	team := e.createTeam(ownerTok, org, "core")
	if rec := e.send("PUT", teamMemberPath(org, team, bob), ownerTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	e.makeTeamAdmin(ownerTok, org, team, carol)
	v := e.createVM(carolTok, org, vmBody("t1", teamOwner(team), e.enrolledNode(ownerTok, org, "n1")))
	path := vmsPath(org) + "/" + v.ID + "/delete-after"
	if rec := e.send("PUT", path, bobTok, `{"deleteAfter":null}`); rec.Code != 403 {
		t.Fatalf("member without vm:delete: %d", rec.Code)
	}
	if rec := e.send("PUT", path, carolTok, `{"deleteAfter":"1h"}`); rec.Code != 200 {
		t.Fatalf("team admin: %d %s", rec.Code, rec.Body.String())
	}
}

func TestVMHistory(t *testing.T) {
	e := newEnv(t)
	_, aliceTok := e.user("alice")
	bob, bobTok := e.user("bob")
	carol, carolTok := e.user("carol")
	dave, daveTok := e.user("dave")
	org := e.createOrg(aliceTok, "Acme")
	for _, u := range []store.User{bob, carol, dave} {
		e.addMember(org, u, store.RoleMember)
	}
	team := e.createTeam(aliceTok, org, "core")
	if rec := e.send("PUT", teamMemberPath(org, team, bob), aliceTok, `{"admin":false}`); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	e.makeTeamAdmin(aliceTok, org, team, carol)
	node := e.enrolledNode(aliceTok, org, "n1")
	mine := e.createVM(bobTok, org, vmBody("bobs", userOwner(), node))
	teamVM := e.createVM(carolTok, org, vmBody("teams", teamOwner(team), node))
	e.createVM(bobTok, org, vmBody("live", userOwner(), node))
	for _, d := range []struct{ tok, id string }{{bobTok, mine.ID}, {carolTok, teamVM.ID}} {
		if rec := e.do("DELETE", vmsPath(org)+"/"+d.id, d.tok); rec.Code != 204 {
			t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
		}
	}
	names := func(h historyTest) map[string]bool {
		m := map[string]bool{}
		for _, v := range h.VMs {
			m[v.Name] = true
		}
		return m
	}
	bh := e.history(bobTok, org.String())
	if n := names(bh); len(n) != 2 || !n["bobs"] || !n["teams"] {
		t.Fatalf("bob (owner + plain team member): %+v", bh)
	}
	for _, v := range bh.VMs {
		if v.Name == "bobs" && (v.Reason != "user" || v.DeletedBy == nil || v.DeletedBy.ID != bob.ID.String() || v.Owner.Kind != "user") {
			t.Fatalf("bobs row: %+v", v)
		}
	}
	if n := names(e.history(daveTok, org.String())); len(n) != 0 {
		t.Fatalf("unrelated member sees: %+v", n)
	}
	if n := names(e.history(aliceTok, org.String())); len(n) != 2 {
		t.Fatalf("org admin sees: %+v", n)
	}

	if rec := e.do("DELETE", "/api/orgs/"+org.String()+"/members/"+carol.ID.String(), aliceTok); rec.Code != 204 {
		t.Fatalf("remove carol: %d %s", rec.Code, rec.Body.String())
	}
	for _, v := range e.history(aliceTok, org.String()).VMs {
		if v.Name == "teams" && (v.CreatedBy.ID != carol.ID.String() || v.CreatedBy.Name != "") {
			t.Fatalf("removed creator: %+v", v)
		}
	}
}
