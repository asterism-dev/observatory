package audit

import (
	auditv1 "github.com/asterism-dev/observatory/gen/go/observatory/audit/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
)

func Catalog() authz.Catalog {
	// TestRegisteredCatalogsReproduce0x proves these annotations convert without error.
	actions, _ := catalog.FromAnnotations(auditv1.File_observatory_audit_v1_audit_proto.Services().ByName("AuditService"))
	return authz.Catalog{
		Service: "audit",
		Actions: actions,
		RoleDefaults: []authz.RoleDefault{
			{Role: "admin", Object: "org/audit", Action: "audit.events.list"},
			{Role: "admin", Object: "org/audit", Action: "audit.settings.get"},
			{Role: "admin", Object: "org/audit", Action: "audit.settings.update"},
		},
	}
}
