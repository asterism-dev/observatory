package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	auditv1 "github.com/asterism-dev/observatory/gen/go/observatory/audit/v1"
	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
)

func TestFromAnnotations(t *testing.T) {
	got, err := FromAnnotations(auditv1.File_observatory_audit_v1_audit_proto.Services().ByName("AuditService"))
	if err != nil {
		t.Fatal(err)
	}
	want := []authz.Action{
		{Name: "audit.events.list", Object: "org/audit"},
		{Name: "audit.settings.get", Object: "org/audit"},
		{Name: "audit.settings.update", Object: "org/audit", Write: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFromAnnotationsPlaceholdersAndMixedKinds(t *testing.T) {
	got, err := FromAnnotations(iamv1.File_observatory_iam_v1_iam_proto.Services().ByName("IAMService"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if !strings.HasPrefix(a.Object, "org/") {
			t.Errorf("non-org action %v", a)
		}
	}
	// ListInvites reads and CreateInvite writes the same action, so it is a write.
	if !slices.Contains(got, authz.Action{Name: "iam.invites.create", Object: "org/members", Write: true}) {
		t.Errorf("iam.invites.create missing or not a write: %v", got)
	}
	if !slices.Contains(got, authz.Action{Name: "iam.teams.update", Object: "org/teams/*", Write: true}) {
		t.Errorf("org/teams/{team} not turned into org/teams/*: %v", got)
	}
}

func sample() authz.Catalog {
	return authz.Catalog{
		Service: "audit",
		Actions: []authz.Action{
			{Name: "audit.events.list", Object: "org/audit"},
			{Name: "audit.settings.update", Object: "org/audit", Write: true},
			{Name: "audit.settings.get", Object: "org/audit"},
		},
		RoleDefaults: []authz.RoleDefault{
			{Role: "admin", Object: "org/audit", Action: "audit.events.list"},
			{Role: "admin", Object: "org/audit", Action: "audit.settings.*"},
		},
		Templates: []authz.OwnerTemplate{
			{Relation: authz.RelTeam, Object: "org/audit-views/{team}", Actions: []string{"audit.events.list", "audit.settings.get"}},
			{Relation: authz.RelMember, Object: "org/audit-views/{user}", Actions: []string{"audit.events.list"}},
		},
	}
}

func TestDigestStableUnderReorder(t *testing.T) {
	a := sample()
	b := sample()
	slices.Reverse(b.Actions)
	slices.Reverse(b.RoleDefaults)
	slices.Reverse(b.Templates)
	slices.Reverse(b.Templates[1].Actions)
	if Digest(a) != Digest(b) {
		t.Error("digest changed under reorder")
	}
	if len(Digest(a)) != 64 {
		t.Errorf("digest %q is not hex SHA-256", Digest(a))
	}
	b.Actions[0].Write = !b.Actions[0].Write
	if Digest(a) == Digest(b) {
		t.Error("digest ignores a changed kind")
	}
}

func TestProtoRoundTrip(t *testing.T) {
	c := Normalize(sample())
	back, err := FromProto(ToProto(c))
	if err != nil {
		t.Fatal(err)
	}
	if Digest(back) != Digest(c) || back.Service != c.Service || len(back.Templates) != 2 {
		t.Errorf("round trip changed the catalog: %+v", back)
	}
	bad := ToProto(c)
	bad.Templates[0].Relation = iamv1.Relation_RELATION_UNSPECIFIED
	if _, err := FromProto(bad); err == nil {
		t.Error("unspecified relation accepted")
	}
}

func TestValidateCatalogAcceptsSample(t *testing.T) {
	if err := Validate(sample(), []string{"audit"}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateCatalogRejectsForeignActions(t *testing.T) {
	for name, mut := range map[string]func(*authz.Catalog){
		"foreign action": func(c *authz.Catalog) {
			c.Actions = append(c.Actions, authz.Action{Name: "iam.members.list", Object: "org/members"})
		},
		"star default": func(c *authz.Catalog) {
			c.RoleDefaults = append(c.RoleDefaults, authz.RoleDefault{Role: "admin", Object: "org/*", Action: "*"})
		},
		"foreign wildcard": func(c *authz.Catalog) {
			c.RoleDefaults = append(c.RoleDefaults, authz.RoleDefault{Role: "admin", Object: "org/audit", Action: "iam.*"})
		},
		"leading wildcard": func(c *authz.Catalog) {
			c.RoleDefaults = append(c.RoleDefaults, authz.RoleDefault{Role: "admin", Object: "org/audit", Action: "*.list"})
		},
		"foreign default": func(c *authz.Catalog) {
			c.RoleDefaults = append(c.RoleDefaults, authz.RoleDefault{Role: "admin", Object: "org/members", Action: "iam.members.delete"})
		},
		"foreign template action": func(c *authz.Catalog) { c.Templates[0].Actions = append(c.Templates[0].Actions, "secrets.variables.*") },
		"default object org/*": func(c *authz.Catalog) {
			c.RoleDefaults = append(c.RoleDefaults, authz.RoleDefault{Role: "admin", Object: "org/*", Action: "audit.events.list"})
		},
		"undeclared action": func(c *authz.Catalog) {
			c.RoleDefaults = append(c.RoleDefaults, authz.RoleDefault{Role: "admin", Object: "org/audit", Action: "audit.events.purge"})
		},
		"unknown role":      func(c *authz.Catalog) { c.RoleDefaults[0].Role = "viewer" },
		"bad action object": func(c *authz.Catalog) { c.Actions[0].Object = "platform/audit" },
		"bad action name":   func(c *authz.Catalog) { c.Actions[0].Name = "audit.events" },
		"duplicate action": func(c *authz.Catalog) {
			c.Actions = append(c.Actions, authz.Action{Name: "audit.events.list", Object: "org/audit", Write: true})
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := sample()
			mut(&c)
			if err := Validate(c, []string{"audit"}); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestValidateCatalogPlaceholders(t *testing.T) {
	for _, tc := range []struct {
		rel authz.Relation
		obj string
		ok  bool
	}{
		{authz.RelMember, "org/audit-views/{user}", true},
		{authz.RelMember, "org/audit-views/{team}", false},
		{authz.RelTeam, "org/audit-views/{team}", true},
		{authz.RelTeamAdmin, "org/audit-views/{team}", true},
		{authz.RelTeam, "org/audit", true},
		{authz.RelTeam, "org/audit-views/{user}", false},
		{authz.RelOwnerUser, "org/vms/{id}", true},
		{authz.RelOwnerUser, "org/vms/{user}", false},
		{authz.RelOwnerTeam, "org/vms", false},
		{authz.RelMember, "org/vms/{id}", false},
		{authz.RelMember, "org/audit-views/*", false},
		{"friend", "org/audit", false},
	} {
		c := sample()
		c.Templates = []authz.OwnerTemplate{{Relation: tc.rel, Object: tc.obj, Actions: []string{"audit.events.list"}}}
		if err := Validate(c, []string{"audit"}); (err == nil) != tc.ok {
			t.Errorf("%s %s: err %v, want ok=%v", tc.rel, tc.obj, err, tc.ok)
		}
	}
}

func TestValidateCatalogLimits(t *testing.T) {
	grow := func(n int, f func(*authz.Catalog, int)) authz.Catalog {
		c := sample()
		for i := range n {
			f(&c, i)
		}
		return c
	}
	action := func(c *authz.Catalog, i int) {
		c.Actions = append(c.Actions, authz.Action{Name: fmt.Sprintf("audit.r%d.list", i), Object: "org/audit"})
	}
	def := func(c *authz.Catalog, i int) {
		c.RoleDefaults = append(c.RoleDefaults, authz.RoleDefault{Role: "member", Object: fmt.Sprintf("org/a%c%c", 'a'+i/26%26, 'a'+i%26), Action: "audit.events.list"})
	}
	tmpl := func(c *authz.Catalog, i int) {
		c.Templates = append(c.Templates, authz.OwnerTemplate{Relation: authz.RelTeam, Object: fmt.Sprintf("org/a%c%c", 'a'+i/26%26, 'a'+i%26), Actions: []string{"audit.events.list"}})
	}
	for name, tc := range map[string]struct {
		ok, over authz.Catalog
	}{
		"actions":   {grow(500-3, action), grow(500-2, action)},
		"defaults":  {grow(500-2, def), grow(500-1, def)},
		"templates": {grow(200-2, tmpl), grow(200-1, tmpl)},
	} {
		if err := Validate(tc.ok, []string{"audit"}); err != nil {
			t.Errorf("%s at the limit: %v", name, err)
		}
		if err := Validate(tc.over, []string{"audit"}); err == nil {
			t.Errorf("%s over the limit accepted", name)
		}
	}
}

type flakyIAM struct {
	iamv1connect.UnimplementedCatalogInternalServiceHandler
	failures atomic.Int32
	code     connect.Code
	calls    atomic.Int32
}

func (f *flakyIAM) RegisterCatalog(_ context.Context, req *connect.Request[iamv1.RegisterCatalogRequest]) (*connect.Response[iamv1.RegisterCatalogResponse], error) {
	f.calls.Add(1)
	if f.failures.Add(-1) >= 0 {
		return nil, connect.NewError(f.code, errors.New("not yet"))
	}
	c, err := FromProto(req.Msg.GetCatalog())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&iamv1.RegisterCatalogResponse{Digest: Digest(c)}), nil
}

func serve(t *testing.T, h *flakyIAM) iamv1connect.CatalogInternalServiceClient {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(iamv1connect.NewCatalogInternalServiceHandler(h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return iamv1connect.NewCatalogInternalServiceClient(srv.Client(), srv.URL)
}

func fastRetry(t *testing.T) {
	t.Helper()
	base, ceiling := retryBase, retryMax
	retryBase, retryMax = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() { retryBase, retryMax = base, ceiling })
}

func TestRegisterRetriesUntilAvailable(t *testing.T) {
	fastRetry(t)
	h := &flakyIAM{code: connect.CodeUnavailable}
	h.failures.Store(5)
	digest, err := Register(t.Context(), serve(t, h), sample())
	if err != nil {
		t.Fatal(err)
	}
	if digest != Digest(sample()) || h.calls.Load() != 6 {
		t.Errorf("digest %q after %d calls", digest, h.calls.Load())
	}
}

func TestRegisterRetriesTransportErrors(t *testing.T) {
	fastRetry(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := Register(ctx, iamv1connect.NewCatalogInternalServiceClient(http.DefaultClient, srv.URL), sample())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %v, want the context's deadline", err)
	}
}

func TestRegisterInvalidArgumentAtOnce(t *testing.T) {
	fastRetry(t)
	h := &flakyIAM{code: connect.CodeInvalidArgument}
	h.failures.Store(5)
	if _, err := Register(t.Context(), serve(t, h), sample()); connect.CodeOf(err) != connect.CodeInvalidArgument || h.calls.Load() != 1 {
		t.Errorf("err %v after %d calls", err, h.calls.Load())
	}
}
