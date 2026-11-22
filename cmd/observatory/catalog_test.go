package main

import (
	"reflect"
	"testing"

	"github.com/asterism-dev/observatory/internal/audit"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/authz/authztest"
	"github.com/asterism-dev/observatory/internal/hubserver"
	"github.com/asterism-dev/observatory/internal/iam"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
)

func TestRegisteredCatalogsReproduce0x(t *testing.T) {
	want := map[string]authz.Catalog{}
	for _, c := range authztest.Legacy() {
		want[c.Service] = catalog.Normalize(c)
	}
	var registered []authz.Catalog
	for _, build := range []func() (authz.Catalog, error){iam.Catalog, audit.Catalog, hubserver.Catalog} {
		c, err := build()
		if err != nil {
			t.Fatal(err)
		}
		registered = append(registered, c)
	}
	if len(registered) != len(want) {
		t.Fatalf("%d catalogs, legacy has %d", len(registered), len(want))
	}
	for _, c := range registered {
		if got := catalog.Normalize(c); !reflect.DeepEqual(got, want[c.Service]) {
			t.Errorf("%s differs from legacy:\n got  %+v\n want %+v", c.Service, got, want[c.Service])
		}
		if err := catalog.Validate(c, iam.AllowedPrefixes(c.Service)); err != nil {
			t.Errorf("%s: %v", c.Service, err)
		}
	}
}
