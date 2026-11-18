package main

import (
	"strings"
	"testing"
)

const (
	netN1       = `{"id":"n1","name":"front","subnet":"10.1.0.0/24","vmCount":2,"canUse":true,"canDelete":true}`
	netN2       = `{"id":"n2","name":"back","subnet":"10.2.0.0/24","vmCount":0,"canUse":true,"canDelete":false}`
	vmOnN1      = `{"id":"v1","name":"dev","state":"running","networks":[{"id":"n1","name":"front","ip":"10.1.0.2"}]}`
	netPutRoute = "PUT /api/orgs/o1/vms/v1/networks"
)

func networkHub(t *testing.T) (*fakeHub, *cli, func() string, func() string) {
	h := newFakeHub(t)
	meOneOrg(h)
	h.on("GET /api/orgs/o1/vms", 200, `{"vms":[`+vmOnN1+`],"nodes":[]}`)
	h.on("GET /api/orgs/o1/networks", 200, `[`+netN1+`,`+netN2+`]`)
	c, out, errb := newTestCLI(t, h)
	return h, c, out.String, errb.String
}

func TestNetworkListGolden(t *testing.T) {
	_, c, out, errb := networkHub(t)
	if code := run(c, []string{"network", "list"}); code != 0 {
		t.Fatal(code, errb())
	}
	golden(t, "network-list.golden", out())
}

func TestNetworkAttachKeepsExisting(t *testing.T) {
	h, c, _, errb := networkHub(t)
	h.on(netPutRoute, 200, vmOnN1)
	if code := run(c, []string{"network", "attach", "back", "dev"}); code != 0 {
		t.Fatal(code, errb())
	}
	if got, want := h.calls(netPutRoute)[0].Body, `{"networkIds":["n1","n2"],"restartAllowed":false}`; got != want {
		t.Errorf("body %s", got)
	}
	if code := run(c, []string{"network", "detach", "front", "dev", "--restart"}); code != 0 {
		t.Fatal(code, errb())
	}
	if got, want := h.calls(netPutRoute)[1].Body, `{"networkIds":[],"restartAllowed":true}`; got != want {
		t.Errorf("body %s", got)
	}
}

func TestNetworkAttachDetachJSONPrintsVM(t *testing.T) {
	h, c, out, errb := networkHub(t)
	h.on(netPutRoute, 200, vmOnN1)
	for _, cmd := range []string{"attach", "detach"} {
		net := map[string]string{"attach": "back", "detach": "front"}[cmd]
		if code := run(c, []string{"network", cmd, net, "dev", "--json"}); code != 0 {
			t.Fatal(code, errb())
		}
	}
	if strings.Count(out(), `"ip": "10.1.0.2"`) != 2 {
		t.Errorf("stdout %q", out())
	}
}

func TestNetworkDetachKeepsOthers(t *testing.T) {
	h, c, _, errb := networkHub(t)
	h.on("GET /api/orgs/o1/vms", 200, `{"vms":[{"id":"v1","name":"dev","networks":[{"id":"n1"},{"id":"n2"}]}],"nodes":[]}`)
	h.on(netPutRoute, 200, vmOnN1)
	if code := run(c, []string{"network", "detach", "front", "dev"}); code != 0 {
		t.Fatal(code, errb())
	}
	if got, want := h.calls(netPutRoute)[0].Body, `{"networkIds":["n2"],"restartAllowed":false}`; got != want {
		t.Errorf("body %s", got)
	}
}

func TestNetworkDetachNotAttached(t *testing.T) {
	h, c, _, errb := networkHub(t)
	if code := run(c, []string{"network", "detach", "back", "dev"}); code != 5 {
		t.Fatal(code, errb())
	}
	if len(h.calls(netPutRoute)) != 0 {
		t.Error("PUT sent")
	}
}

func TestNetworkCreateBody(t *testing.T) {
	h, c, _, errb := networkHub(t)
	h.on("POST /api/orgs/o1/networks", 201, netN2)
	if code := run(c, []string{"network", "create", "back", "--subnet", "10.2.0.0/24"}); code != 0 {
		t.Fatal(code, errb())
	}
	if code := run(c, []string{"network", "create", "back"}); code != 0 {
		t.Fatal(code, errb())
	}
	calls := h.calls("POST /api/orgs/o1/networks")
	if calls[0].Body != `{"name":"back","subnet":"10.2.0.0/24"}` || calls[1].Body != `{"name":"back","subnet":""}` {
		t.Errorf("bodies %s %s", calls[0].Body, calls[1].Body)
	}
}

func TestNetworkDeleteConfirms(t *testing.T) {
	h, c, _, errb := networkHub(t)
	h.on("DELETE /api/orgs/o1/networks/n1", 204, "")
	if code := run(c, []string{"network", "delete", "front"}); code != 2 {
		t.Fatal(code, errb())
	}
	if len(h.calls("DELETE /api/orgs/o1/networks/n1")) != 0 {
		t.Fatal("DELETE without confirmation")
	}
	if code := run(c, []string{"network", "delete", "front", "--yes"}); code != 0 {
		t.Fatal(code, errb())
	}
}
