package fleet

import (
	"time"

	"github.com/google/uuid"
)

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

func SetBetweenVMReads(f *Fleet, hook func()) { f.betweenVMReads = hook }

func Seen(f *Fleet, node, vm uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.conns[node]
	return c != nil && c.seen[vm.String()]
}

func SetVMErrorGrace(f *Fleet, d time.Duration) { f.updateTimeouts.vmErrGrace = d }
