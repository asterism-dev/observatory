package quota

import (
	quotav1 "github.com/asterism-dev/observatory/gen/go/observatory/quota/v1"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
)

func Catalog() (authz.Catalog, error) {
	actions, err := catalog.FromAnnotations(quotav1.File_observatory_quota_v1_quota_proto.Services().ByName("QuotaService"))
	if err != nil {
		return authz.Catalog{}, err
	}
	return authz.Catalog{
		Service: "quota",
		Actions: actions,
		RoleDefaults: []authz.RoleDefault{
			{Role: "member", Object: "org/limits", Action: "quota.limits.get"},
			{Role: "admin", Object: "org/limits", Action: "quota.limits.update"},
		},
	}, nil
}
