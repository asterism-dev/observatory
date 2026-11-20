package main

import (
	"strings"
	"testing"
	"time"
)

func TestVMCreateDeleteAfterFlag(t *testing.T) {
	h, c, _, _ := vmHub(t, oneNode)
	h.on("POST /api/orgs/o1/vms", 201, vmDev)
	if code := run(c, []string{"vm", "create", "dev", "--delete-after", "2d"}); code != 0 {
		t.Fatal(code)
	}
	if got := h.calls("POST /api/orgs/o1/vms")[0].Body; !strings.Contains(got, `"deleteAfter":"2d"`) {
		t.Errorf("body %s", got)
	}
}

func TestVMDeleteAfterCommand(t *testing.T) {
	for arg, want := range map[string]string{"1h": `{"deleteAfter":"1h"}`, "off": `{"deleteAfter":null}`} {
		h, c, _, _ := vmHub(t, oneNode)
		h.on("PUT /api/orgs/o1/vms/v1/delete-after", 200, vmDev)
		if code := run(c, []string{"vm", "delete-after", "dev", arg}); code != 0 {
			t.Fatal(code)
		}
		if got := h.calls("PUT /api/orgs/o1/vms/v1/delete-after")[0].Body; got != want {
			t.Errorf("%s: %s", arg, got)
		}
	}
}

func TestVMListDeletedGolden(t *testing.T) {
	h, c, out, _ := vmHub(t, oneNode)
	h.on("GET /api/orgs/o1/vms/history", 200, `{"vms":[
		{"id":"v8","name":"probe","owner":{"kind":"user","id":"u1","name":"Ada Lovelace"},"cpus":1,"memoryMB":4096,"diskGB":100,"createdAt":"2026-11-18T09:12:00Z","createdBy":{"id":"u1","name":"Ada Lovelace"},"deletedAt":"2026-11-19T09:12:00Z","deletedBy":null,"reason":"expired"},
		{"id":"v9","name":"wolf","owner":{"kind":"team","id":"t1","name":"backend"},"cpus":2,"memoryMB":4096,"diskGB":20,"createdAt":"2026-11-17T14:03:00Z","createdBy":{"id":"u2","name":""},"deletedAt":"2026-11-17T16:40:00Z","deletedBy":{"id":"u1","name":"Ada Lovelace"},"reason":"user"}]}`)
	if code := run(c, []string{"vm", "list", "--deleted"}); code != 0 {
		t.Fatal(code)
	}
	golden(t, "vm-list-deleted.golden", out())
}

func TestLifetime(t *testing.T) {
	for d, want := range map[time.Duration]string{24 * time.Hour: "1d 0h", 157 * time.Minute: "2h 37m", 12 * time.Minute: "12m", 30 * time.Second: "0m"} {
		if got := lifetime(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}
