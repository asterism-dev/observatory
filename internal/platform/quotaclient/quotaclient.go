// Package quotaclient holds what callers of the quota service's QuotaInternalService share.
package quotaclient

import (
	"errors"

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

// NewLimitExceeded is the ResourceExhausted error Reserve answers with; msg is the 0.x limit message.
func NewLimitExceeded(msg string, d *quotav1.LimitExceeded) error {
	ce := connect.NewError(connect.CodeResourceExhausted, errors.New(msg))
	if detail, err := connect.NewErrorDetail(d); err == nil {
		ce.AddDetail(detail)
	}
	return ce
}
