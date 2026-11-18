package egress

import (
	"encoding/base64"
	"net/http"
	"slices"
	"testing"

	"github.com/asterism-dev/observatory/internal/proto"
)

func TestSubstituteUnit(t *testing.T) {
	s := ghSecret("a.test")
	other := proto.HostBoundSecret{Name: "OTHER", Placeholder: "obsph_cccccccccccccccccccccccccccccccc", Value: "other-value-1234", Hosts: []string{"b.test"}}
	empty := proto.HostBoundSecret{Name: "EMPTY", Value: "empty-value-1234", Hosts: []string{"a.test"}}
	h := http.Header{}
	h.Set("Authorization", "basic "+base64.StdEncoding.EncodeToString([]byte("x:"+ghPH)))
	h.Set("X-Token", "pre-"+ghPH+"-post")
	h.Set("X-Other", other.Placeholder)
	h.Set("X-Plain", "nothing")
	names, sent := substitute(h, "a.test", []proto.HostBoundSecret{s, other, empty})
	if !slices.Equal(names, []string{"GH_TOKEN"}) {
		t.Fatalf("names %v", names)
	}
	enc := base64.StdEncoding.EncodeToString([]byte("x:" + ghValue))
	if h.Get("Authorization") != "basic "+enc || h.Get("X-Token") != "pre-"+ghValue+"-post" {
		t.Fatalf("authorization %q x-token %q", h.Get("Authorization"), h.Get("X-Token"))
	}
	if h.Get("X-Other") != other.Placeholder || h.Get("X-Plain") != "nothing" {
		t.Fatal("unrelated header changed")
	}
	want := []string{"pre-" + ghValue + "-post", enc}
	slices.Sort(sent)
	slices.Sort(want)
	if !slices.Equal(sent, want) {
		t.Fatalf("sent %q", sent)
	}
}
