package main

import (
	"reflect"
	"slices"
	"testing"

	"github.com/asterism-dev/observatory/internal/audit"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/authz/authztest"
	"github.com/asterism-dev/observatory/internal/hubserver"
	"github.com/asterism-dev/observatory/internal/iam"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
	"github.com/asterism-dev/observatory/internal/quota"
)

// extractedFromHub names services whose catalog the 0.x hub catalog held.
var extractedFromHub = []string{"quota"}

func TestRegisteredCatalogsReproduce0x(t *testing.T) {
	want := map[string]authz.Catalog{}
	for _, c := range authztest.Legacy() {
		want[c.Service] = catalog.Normalize(c)
	}
	registered := map[string]authz.Catalog{}
	for _, build := range []func() (authz.Catalog, error){iam.Catalog, audit.Catalog, hubserver.Catalog, quota.Catalog} {
		c, err := build()
		if err != nil {
			t.Fatal(err)
		}
		if err := catalog.Validate(c, catalog.AllowedPrefixes(c.Service)); err != nil {
			t.Errorf("%s: %v", c.Service, err)
		}
		svc := c.Service
		if slices.Contains(extractedFromHub, svc) {
			svc = "hub"
		}
		u := registered[svc]
		u.Service = svc
		u.Actions = append(u.Actions, c.Actions...)
		u.RoleDefaults = append(u.RoleDefaults, c.RoleDefaults...)
		u.Templates = append(u.Templates, c.Templates...)
		registered[svc] = u
	}
	if len(registered) != len(want) {
		t.Fatalf("%d catalogs, legacy has %d", len(registered), len(want))
	}
	for svc, c := range registered {
		if got := catalog.Normalize(c); !reflect.DeepEqual(got, want[svc]) {
			t.Errorf("%s differs from legacy:\n got  %+v\n want %+v", svc, got, want[svc])
		}
	}
}
