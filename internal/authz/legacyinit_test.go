package authz_test

import (
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/authz/authztest"
)

func init() { authz.LegacyCatalogs = authztest.Legacy }
