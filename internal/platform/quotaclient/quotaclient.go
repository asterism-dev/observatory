// Package quotaclient holds what callers of the quota service's QuotaInternalService share.
package quotaclient

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"

	quotav1 "github.com/asterism-dev/observatory/gen/go/observatory/quota/v1"
)

// LimitExceeded returns the limit a failed Reserve would cross, if err carries one.
func LimitExceeded(err error) (*quotav1.LimitExceeded, bool) {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return nil, false
	}
	for _, d := range ce.Details() {
		if v, err := d.Value(); err == nil {
			if le, ok := v.(*quotav1.LimitExceeded); ok {
				return le, true
			}
		}
	}
	return nil, false
}

// Message is the 0.x limit message, as the hub's store.LimitError words it.
func Message(d *quotav1.LimitExceeded) string {
	owner := "the org's"
	if d.GetScope() != "org" {
		owner = "the owner's"
	}
	return fmt.Sprintf("%s %s limit of %d%s would be exceeded (%d%s)", owner, d.GetName(), d.GetLimit(), d.GetUnit(), d.GetGot(), d.GetUnit())
}

func NewLimitExceeded(d *quotav1.LimitExceeded) error {
	ce := connect.NewError(connect.CodeResourceExhausted, errors.New(Message(d)))
	if detail, err := connect.NewErrorDetail(d); err == nil {
		ce.AddDetail(detail)
	}
	return ce
}
