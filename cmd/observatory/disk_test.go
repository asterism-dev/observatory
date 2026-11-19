package main

import (
	"strings"
	"testing"
)

const (
	diskA       = `{"id":"a-id","name":"a","owner":{"kind":"user","id":"u1","name":"Ada Lovelace"},"sizeGB":10,"allocatedBytes":1073741824,"state":"ready","attachment":{"vm":{"id":"v1","name":"dev"},"mountPath":"/mnt/a"},"createdAt":"2026-11-19T13:00:00Z"}`
	diskB       = `{"id":"b-id","name":"b","owner":{"kind":"team","id":"t1","name":"backend"},"sizeGB":50,"allocatedBytes":0,"state":"creating","createdAt":"2026-11-19T13:50:00Z"}`
	vmWithDisks = `{"id":"v1","name":"dev","state":"running","disks":[{"id":"a-id","name":"a","mountPath":"/mnt/a","sizeGB":10,"state":"ready"},{"id":"b-id","name":"b","mountPath":"/mnt/b","sizeGB":50,"state":"ready"}]}`
	vmOneDisk   = `{"id":"v1","name":"dev","state":"running","disks":[{"id":"a-id","name":"a","mountPath":"/mnt/a","sizeGB":10,"state":"ready"}]}`
)

func diskHub(t *testing.T, vm string, online bool) (*fakeHub, *cli, func() string, func() string) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.on("GET /api/orgs/o1/vms", 200, `{"vms":[`+vm+`],"nodes":[]}`)
	on := "false"
	if online {
		on = "true"
	}
	h.on("GET /api/orgs/o1/disks", 200, `{"disks":[`+diskA+`,`+diskB+`],"storageOnline":`+on+`}`)
	c, out, errb := newTestCLI(t, h)
	return h, c, out.String, errb.String
}

func TestDiskListGolden(t *testing.T) {
	_, c, out, errb := diskHub(t, vmOneDisk, false)
	if code := run(c, []string{"disk", "list"}); code != 0 {
		t.Fatal(code, errb())
	}
	golden(t, "disk-list.golden", out())
	if !strings.Contains(errb(), "storage offline") {
		t.Errorf("stderr %q", errb())
	}
}

func TestDiskAttachKeepsExisting(t *testing.T) {
	h, c, _, errb := diskHub(t, vmOneDisk, true)
	h.on("PUT /api/orgs/o1/vms/v1/disks", 200, vmWithDisks)
	if code := run(c, []string{"disk", "attach", "b", "dev"}); code != 0 {
		t.Fatal(code, errb())
	}
	want := `{"disks":[{"diskId":"a-id","mountPath":"/mnt/a"},{"diskId":"b-id","mountPath":"/mnt/b"}],"restartAllowed":false}`
	if got := h.calls("PUT /api/orgs/o1/vms/v1/disks")[0].Body; got != want {
		t.Errorf("body %s", got)
	}
}

func TestDiskDetachKeepsOthers(t *testing.T) {
	h, c, _, errb := diskHub(t, vmWithDisks, true)
	h.on("PUT /api/orgs/o1/vms/v1/disks", 200, vmOneDisk)
	if code := run(c, []string{"disk", "detach", "a", "dev", "--restart"}); code != 0 {
		t.Fatal(code, errb())
	}
	want := `{"disks":[{"diskId":"b-id","mountPath":"/mnt/b"}],"restartAllowed":true}`
	if got := h.calls("PUT /api/orgs/o1/vms/v1/disks")[0].Body; got != want {
		t.Errorf("body %s", got)
	}
}

func TestDiskAttachDetachJSONPrintsVM(t *testing.T) {
	h, c, out, errb := diskHub(t, vmOneDisk, true)
	h.on("PUT /api/orgs/o1/vms/v1/disks", 200, vmWithDisks)
	for _, args := range [][]string{{"disk", "attach", "b", "dev", "--json"}, {"disk", "detach", "a", "dev", "--json"}} {
		if code := run(c, args); code != 0 {
			t.Fatal(code, errb())
		}
	}
	if strings.Count(out(), `"mountPath": "/mnt/b"`) != 2 {
		t.Errorf("stdout %q", out())
	}
}

func TestDiskAttachTwiceRefused(t *testing.T) {
	h, c, _, errb := diskHub(t, vmWithDisks, true)
	if code := run(c, []string{"disk", "attach", "a", "dev"}); code != 2 {
		t.Fatal(code, errb())
	}
	if len(h.calls("PUT /api/orgs/o1/vms/v1/disks")) != 0 {
		t.Error("PUT sent")
	}
}

func TestDiskDetachNotAttached(t *testing.T) {
	h, c, _, errb := diskHub(t, vmOneDisk, true)
	if code := run(c, []string{"disk", "detach", "b", "dev"}); code != 5 {
		t.Fatal(code, errb())
	}
	if len(h.calls("PUT /api/orgs/o1/vms/v1/disks")) != 0 {
		t.Error("PUT sent")
	}
}

func TestDiskCreateBody(t *testing.T) {
	h, c, _, errb := diskHub(t, vmOneDisk, true)
	h.on("GET /api/orgs/o1/teams", 200, `[{"id":"t1","name":"backend"}]`)
	h.on("POST /api/orgs/o1/disks", 201, diskB)
	if code := run(c, []string{"disk", "create", "data", "--size", "50", "--team", "backend"}); code != 0 {
		t.Fatal(code, errb())
	}
	want := `{"name":"data","owner":{"kind":"team","teamId":"t1"},"sizeGB":50}`
	if got := h.calls("POST /api/orgs/o1/disks")[0].Body; got != want {
		t.Errorf("body %s", got)
	}
	if code := run(c, []string{"disk", "create", "x"}); code != 2 {
		t.Errorf("missing --size: %d", code)
	}
}

func TestDiskGrowBody(t *testing.T) {
	h, c, _, errb := diskHub(t, vmOneDisk, true)
	h.on("POST /api/orgs/o1/disks/a-id/grow", 200, diskA)
	if code := run(c, []string{"disk", "grow", "a", "--size", "20", "--restart"}); code != 0 {
		t.Fatal(code, errb())
	}
	if got := h.calls("POST /api/orgs/o1/disks/a-id/grow")[0].Body; got != `{"sizeGB":20,"restartAllowed":true}` {
		t.Errorf("body %s", got)
	}
}

func TestDiskDeleteConfirms(t *testing.T) {
	h, c, _, errb := diskHub(t, vmOneDisk, true)
	h.on("DELETE /api/orgs/o1/disks/a-id", 204, "")
	if code := run(c, []string{"disk", "delete", "a"}); code != 2 {
		t.Fatal(code, errb())
	}
	if len(h.calls("DELETE /api/orgs/o1/disks/a-id")) != 0 {
		t.Fatal("DELETE without confirmation")
	}
	if code := run(c, []string{"disk", "delete", "a", "--yes"}); code != 0 {
		t.Fatal(code, errb())
	}
}

func TestDiskReleaseConfirms(t *testing.T) {
	h, c, _, errb := diskHub(t, vmOneDisk, true)
	h.on("POST /api/orgs/o1/disks/a-id/release", 204, "")
	if code := run(c, []string{"disk", "release", "a"}); code != 2 {
		t.Fatal(code, errb())
	}
	if len(h.calls("POST /api/orgs/o1/disks/a-id/release")) != 0 {
		t.Fatal("POST without confirmation")
	}
	if code := run(c, []string{"disk", "release", "a", "--yes"}); code != 0 {
		t.Fatal(code, errb())
	}
}

func TestOwnerLabelNamesOtherMembers(t *testing.T) {
	grace := strings.NewReplacer(`"id":"u1","name":"Ada Lovelace"`, `"id":"u2","name":"Grace Hopper"`)
	h, c, out, errb := diskHub(t, vmOneDisk, true)
	h.on("GET /api/orgs/o1/disks", 200, `{"disks":[`+grace.Replace(diskA)+`],"storageOnline":true}`)
	h.on("GET /api/orgs/o1/vms", 200, `{"vms":[`+vmDev+`,`+grace.Replace(strings.NewReplacer(`"id":"v1"`, `"id":"v3"`, `"name":"dev"`, `"name":"gh"`).Replace(vmDev))+`],"nodes":[]}`)
	for _, args := range [][]string{{"disk", "list"}, {"vm", "list"}} {
		if code := run(c, args); code != 0 {
			t.Fatal(code, errb())
		}
	}
	if !strings.Contains(out(), "Grace Hopper") || strings.Count(out(), "private") != 1 {
		t.Errorf("stdout %q", out())
	}
}
