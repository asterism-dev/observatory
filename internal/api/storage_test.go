package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/disks"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

const storageToken = "fedcba9876543210fedcba9876543210"

func TestStorageEndpointsAuth(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	u, utok := e.user("alice")
	org := e.createOrg(utok, "Acme")
	node, tok, err := e.store.CreateNode(ctx, store.Node{OrgID: org, Name: "n1", CreatedBy: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.EnrollNode(ctx, tok); err != nil {
		t.Fatal(err)
	}
	vm, err := e.store.CreateVM(ctx, store.VM{OrgID: org, NodeID: node.ID, UserID: &u.ID, Name: "v", CPUs: 1, MemoryMB: 1, DiskGB: 1, CreatedBy: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.SetVMPower(ctx, org, vm.ID, proto.PowerStopped, proto.StopHard); err != nil {
		t.Fatal(err)
	}
	d, err := e.store.CreateDisk(ctx, store.Disk{OrgID: org, UserID: &u.ID, Name: "data", SizeGB: 1, CreatedBy: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.SetVMDisks(ctx, org, vm.ID, []store.VMDisk{{DiskID: d.ID, MountPath: "/data"}}, false); err != nil {
		t.Fatal(err)
	}

	var notified []uuid.UUID
	mux := http.NewServeMux()
	StorageEndpoints(mux, disks.New(e.store, storageToken, func(org uuid.UUID) { notified = append(notified, org) }))
	for _, path := range []string{"/api/storage/state", "/api/storage/status"} {
		method := map[bool]string{true: "GET", false: "POST"}[path == "/api/storage/state"]
		for _, auth := range []string{"", "Bearer wrong", "Bearer " + gatewayToken, "Bearer " + storageToken[:31], storageToken} {
			if rec := gatewayDo(mux, method, path, auth, "{}"); rec.Code != 401 {
				t.Fatalf("%s %q: %d", path, auth, rec.Code)
			}
		}
	}

	body := `{"files":[{"name":"` + proto.StorageFileName(d.ID.String()) + `","sizeBytes":1073741824,"allocatedBytes":0}],"version":"v"}`
	if rec := gatewayDo(mux, "POST", "/api/storage/status", "Bearer "+storageToken, body); rec.Code != 204 {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	if len(notified) != 1 || notified[0] != org {
		t.Fatalf("notified %v", notified)
	}
	rec := gatewayDo(mux, "GET", "/api/storage/state", "Bearer "+storageToken, "")
	var st proto.StorageState
	decode(t, rec, &st)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	if len(st.Disks) != 1 || st.Disks[0].Create || len(st.Exports) != 1 || st.Exports[0].DiskID != d.ID.String() || len(st.Exports[0].PSK) != 64 {
		t.Fatalf("%+v", st)
	}
}
