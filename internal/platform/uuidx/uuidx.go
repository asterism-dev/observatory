// Package uuidx parses UUIDs that must arrive in canonical form.
package uuidx

import (
	"fmt"

	"github.com/google/uuid"
)

// Canonical rejects every spelling but the lowercase hyphenated one, so one ID has one string form.
func Canonical(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err == nil && id.String() != s {
		err = fmt.Errorf("%q is not a canonical UUID", s)
	}
	return id, err
}
