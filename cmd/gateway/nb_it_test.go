package main

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/asterism-dev/observatory/internal/proto"
)

// Opt-in: OBS_NB_IT=<container> runs reconcileNB against ovn-nbctl inside that container.
func TestReconcileNBRealNB(t *testing.T) {
	ctr := os.Getenv("OBS_NB_IT")
	if ctr == "" {
		t.Skip("OBS_NB_IT not set")
	}
	run := func(args ...string) (string, error) {
		out, err := exec.Command("docker", append([]string{"exec", ctr}, args...)...).CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("%w: %s", err, out)
		}
		return string(out), nil
	}
	st, _ := twoOrgs()
	only := func(orgs ...proto.GatewayOrg) proto.GatewayState { s := st; s.Orgs = orgs; return s }
	for _, s := range []proto.GatewayState{st, st, only(st.Orgs[0]), st} {
		if err := reconcileNB(run, s); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range [][]string{{"show"}, {"acl-list", pgName(orgA)}, {"acl-list", pgName(orgB)}, {"list", "dhcp_options"}, {"list", "port_group"}} {
		out, _ := run(append([]string{"ovn-nbctl"}, c...)...)
		t.Logf("ovn-nbctl %v\n%s", c, out)
	}
}
