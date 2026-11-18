package main

import "testing"

const (
	nodeN1 = `{"id":"n1","name":"orga","state":"online","version":"1.2.0","cpus":16,"memoryMB":65536,"lastSeen":"2026-11-19T14:02:00Z","vmCount":3,"usedCPUs":6,"usedMemoryMB":12288,"canUpdate":true,"canDelete":true,"quarantined":false,"image":{"version":"v7","state":"building"}}`
	nodeN2 = `{"id":"n2","name":"orgc","state":"offline","version":"1.1.0","cpus":8,"memoryMB":32768,"lastSeen":"2026-11-18T09:00:00Z","vmCount":0,"usedCPUs":0,"usedMemoryMB":0,"quarantined":true,"image":{"version":"v7","state":"failed","error":"disk full","retryAt":"2026-11-19T14:10:00Z"}}`
	nodeN3 = `{"id":"n3","name":"fresh","state":"pending","version":"","cpus":0,"memoryMB":0,"lastSeen":null,"vmCount":0,"usedCPUs":0,"usedMemoryMB":0,"quarantined":false,"image":null}`
)

func nodeHub(t *testing.T) (*fakeHub, *cli, func() string, func() string) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.on("GET /api/orgs/o1/nodes", 200, `[`+nodeN1+`,`+nodeN2+`,`+nodeN3+`]`)
	c, out, errb := newTestCLI(t, h)
	return h, c, out.String, errb.String
}

func TestNodeListGolden(t *testing.T) {
	_, c, out, errb := nodeHub(t)
	if code := run(c, []string{"node", "list"}); code != 0 {
		t.Fatal(code, errb())
	}
	golden(t, "node-list.golden", out())
}

func TestNodeShowGolden(t *testing.T) {
	h, c, out, errb := nodeHub(t)
	h.on("GET /api/orgs/o1/nodes/n2", 200, nodeN2)
	if code := run(c, []string{"node", "show", "orgc"}); code != 0 {
		t.Fatal(code, errb())
	}
	golden(t, "node-show.golden", out())
}

func TestNodeAddPrintsInstallCommand(t *testing.T) {
	h, c, out, errb := nodeHub(t)
	const cmd = "curl -fsSL https://hub/node/install.sh | sudo sh -s -- --hub https://hub --token tok-node"
	h.on("POST /api/orgs/o1/nodes", 201, `{"node":`+nodeN3+`,"installCommand":"`+cmd+`"}`)
	if code := run(c, []string{"node", "add", "fresh"}); code != 0 {
		t.Fatal(code, errb())
	}
	if out() != cmd+"\n" {
		t.Errorf("stdout %q", out())
	}
	if got := h.calls("POST /api/orgs/o1/nodes")[0].Body; got != `{"name":"fresh"}` {
		t.Errorf("body %s", got)
	}
}

func TestNodeTokenWarnsAndPrints(t *testing.T) {
	h, c, out, errb := nodeHub(t)
	h.on("POST /api/orgs/o1/nodes/n1/credential", 200, `{"installCommand":"install-cmd"}`)
	if code := run(c, []string{"node", "token", "orga"}); code != 0 {
		t.Fatal(code, errb())
	}
	if out() != "install-cmd\n" || errb() != "The old credential stops working.\n" {
		t.Errorf("stdout %q stderr %q", out(), errb())
	}
}

func TestNodeRenameAndDelete(t *testing.T) {
	h, c, _, errb := nodeHub(t)
	h.on("PATCH /api/orgs/o1/nodes/n1", 204, "")
	h.on("DELETE /api/orgs/o1/nodes/n1", 204, "")
	if code := run(c, []string{"node", "rename", "orga", "big"}); code != 0 {
		t.Fatal(code, errb())
	}
	if got := h.calls("PATCH /api/orgs/o1/nodes/n1")[0].Body; got != `{"name":"big"}` {
		t.Errorf("body %s", got)
	}
	if code := run(c, []string{"node", "delete", "orga"}); code != 2 {
		t.Fatal(code, errb())
	}
	if len(h.calls("DELETE /api/orgs/o1/nodes/n1")) != 0 {
		t.Fatal("DELETE without confirmation")
	}
	if code := run(c, []string{"node", "delete", "orga", "--yes"}); code != 0 {
		t.Fatal(code, errb())
	}
}
