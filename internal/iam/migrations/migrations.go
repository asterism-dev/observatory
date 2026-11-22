// Package migrations holds iam's schema; outbox.Migrations adds the outbox as version 2.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
