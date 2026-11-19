package fleet

import "time"

var (
	DesiredRetry        = &desiredRetry
	IntegrationRetry    = &integrationRetry
	UnderlayCheck       = &underlayCheck
	UnderlayRenewBefore = &underlayRenewBefore
	Clock               = &clock
)

func SetUpdateTimeouts(f *Fleet, back, vms, poll time.Duration) {
	f.updateTimeouts.back, f.updateTimeouts.vms, f.updateTimeouts.poll = back, vms, poll
}
