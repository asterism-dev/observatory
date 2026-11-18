package main

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	snapA = `{"id":"s1","name":"clean","memory":false,"state":"ready","sizeBytes":1073741824,"disks":[],"createdAt":"2026-11-19T13:00:00Z","canRestoreMemory":false}`
	snapB = `{"id":"s2","name":"warm","memory":true,"state":"creating","sizeBytes":0,"disks":[],"createdAt":"2026-11-19T13:50:00Z","canRestoreMemory":false}`
)

func snapHub(t *testing.T) (*fakeHub, *cli, func() string, func() string) {
	h, c, out, errb := vmHub(t, oneNode)
	h.on("GET /api/orgs/o1/vms/v1/snapshots", 200, `[`+snapA+`,`+snapB+`]`)
	return h, c, out, errb
}

func snapState(state, errText string) string {
	return `{"id":"s3","name":"snap","memory":false,"state":"` + state + `","error":"` + errText + `","sizeBytes":0,"disks":[],"createdAt":"2026-11-19T13:50:00Z"}`
}

func TestSnapshotListGolden(t *testing.T) {
	_, c, out, errb := snapHub(t)
	if code := run(c, []string{"snapshot", "list", "dev"}); code != 0 {
		t.Fatal(code, errb())
	}
	golden(t, "snapshot-list.golden", out())
}

func TestSnapshotCreateWait(t *testing.T) {
	for _, tc := range []struct {
		final, errText string
		code           int
	}{
		{"ready", "", 0},
		{"error", "node disconnected mid-snapshot", 1},
	} {
		h, c, _, errb := vmHub(t, oneNode)
		h.on("POST /api/orgs/o1/vms/v1/snapshots", 201, snapState("creating", ""))
		var n atomic.Int32
		h.onFunc("GET /api/orgs/o1/vms/v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) == 1 {
				w.Write([]byte("[" + snapState("creating", "") + "]"))
				return
			}
			w.Write([]byte("[" + snapState(tc.final, tc.errText) + "]"))
		})
		if code := run(c, []string{"snapshot", "create", "dev", "snap", "--memory", "--wait"}); code != tc.code {
			t.Fatal(code, errb())
		}
		if got := h.calls("POST /api/orgs/o1/vms/v1/snapshots")[0].Body; got != `{"name":"snap","memory":true}` {
			t.Errorf("body %s", got)
		}
		if !strings.Contains(errb(), tc.errText) {
			t.Errorf("stderr %q", errb())
		}
	}
}

func TestSnapshotRestoreConfirmsAndBody(t *testing.T) {
	h, c, _, errb := snapHub(t)
	const post = "POST /api/orgs/o1/vms/v1/snapshots/s1/restore"
	h.on(post, 200, vmDev)
	if code := run(c, []string{"snapshot", "restore", "dev", "clean"}); code != 2 {
		t.Fatal(code, errb())
	}
	if len(h.calls(post)) != 0 {
		t.Fatal("POST without confirmation")
	}
	if code := run(c, []string{"snapshot", "restore", "dev", "clean", "--yes", "--memory", "--restart"}); code != 0 {
		t.Fatal(code, errb())
	}
	if got := h.calls(post)[0].Body; got != `{"memory":true,"restartAllowed":true}` {
		t.Errorf("body %s", got)
	}
}

func TestSnapshotRestoreWait(t *testing.T) {
	h, c, _, errb := snapHub(t)
	h.on("POST /api/orgs/o1/vms/v1/snapshots/s1/restore", 200, strings.Replace(vmDev, `"state":"running"`, `"state":"restoring"`, 1))
	var n atomic.Int32
	h.onFunc("GET /api/orgs/o1/vms/v1", func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Write([]byte(strings.Replace(vmDev, `"state":"running"`, `"state":"restoring"`, 1)))
			return
		}
		w.Write([]byte(vmDev))
	})
	if code := run(c, []string{"snapshot", "restore", "dev", "clean", "-y", "--wait"}); code != 0 {
		t.Fatal(code, errb())
	}
}

func TestSnapshotForkWait(t *testing.T) {
	h, c, _, errb := snapHub(t)
	nv := strings.NewReplacer(`"id":"v1"`, `"id":"v9"`, `"name":"dev"`, `"name":"dev2"`)
	h.on("POST /api/orgs/o1/vms/v1/snapshots/s1/fork", 201, strings.Replace(nv.Replace(vmDev), `"state":"running"`, `"state":"creating"`, 1))
	h.on("GET /api/orgs/o1/vms/v9", 200, nv.Replace(vmDev))
	if code := run(c, []string{"snapshot", "fork", "dev", "clean", "dev2", "--wait"}); code != 0 {
		t.Fatal(code, errb())
	}
	if got := h.calls("POST /api/orgs/o1/vms/v1/snapshots/s1/fork")[0].Body; got != `{"name":"dev2","memory":false}` {
		t.Errorf("body %s", got)
	}
	if len(h.calls("GET /api/orgs/o1/vms/v9")) == 0 {
		t.Error("new VM not polled")
	}
}

func TestSnapshotDeleteConfirms(t *testing.T) {
	h, c, _, _ := snapHub(t)
	h.on("DELETE /api/orgs/o1/vms/v1/snapshots/s1", 204, "")
	if code := run(c, []string{"snapshot", "delete", "dev", "clean"}); code != 2 || len(h.calls("DELETE /api/orgs/o1/vms/v1/snapshots/s1")) != 0 {
		t.Fatal(code)
	}
	if code := run(c, []string{"snapshot", "delete", "dev", "clean", "--yes"}); code != 0 {
		t.Fatal(code)
	}
}

func TestSnapshotNotFound(t *testing.T) {
	_, c, _, errb := snapHub(t)
	if code := run(c, []string{"snapshot", "restore", "dev", "nope", "--yes"}); code != 5 {
		t.Fatal(code)
	}
	if !strings.Contains(errb(), "VM dev") {
		t.Errorf("stderr %q", errb())
	}
}
