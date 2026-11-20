//go:build linux

package linuxvm

import (
	"testing"

	"github.com/google/uuid"
)

func TestEmitEgressLineSkipsWithoutSinkOrVM(t *testing.T) {
	emitEgressLine(nil, uuid.New(), "egress vm=x vars=SECRET")
	emitEgressLine(nil, uuid.Nil, "egress vm=x vars=SECRET")
}
