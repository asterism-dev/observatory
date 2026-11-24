package policy

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestParseIDRoundTrip(t *testing.T) {
	acc := uuid.MustParse("6f1c2b1e-0a4b-4c1e-9a77-1f2e3d4c5b6a")
	for _, s := range []string{
		"obs:compute:" + acc.String() + ":vm/9b7e",
		"obs:compute:" + acc.String() + ":vm/9b7e/snapshot/1",
		"obs:images:" + acc.String() + ":image/x-1",
	} {
		id, err := ParseID(s)
		if err != nil || id.String() != s || id.Account != acc {
			t.Fatalf("ParseID(%q) = %+v, %v", s, id, err)
		}
	}
	if got := NewID("compute", acc, "vm", "9b7e", "snapshot", "1").String(); got != "obs:compute:"+acc.String()+":vm/9b7e/snapshot/1" {
		t.Fatalf("NewID = %q", got)
	}
}

func TestParseIDRejects(t *testing.T) {
	acc := "6f1c2b1e-0a4b-4c1e-9a77-1f2e3d4c5b6a"
	for _, s := range []string{
		"", "obs:compute", "aws:compute:" + acc + ":vm/1", "obs::" + acc + ":vm/1",
		"obs:compute:" + strings.ToUpper(acc) + ":vm/1", "obs:compute:not-a-uuid:vm/1",
		"obs:compute:" + acc + ":vm", "obs:compute:" + acc + ":vm/", "obs:compute:" + acc + ":vm/1:x",
		"obs:Compute:" + acc + ":vm/1", "obs:compute:" + acc + ":vm//1", "obs:compute:" + acc + ":vm/1*",
	} {
		if _, err := ParseID(s); err == nil {
			t.Errorf("ParseID(%q) accepted", s)
		}
	}
}
