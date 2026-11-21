//go:build linux

package linuxvm

import (
	"testing"

	"github.com/google/uuid"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

func TestEmitEgressSkipsWithoutSinkOrVM(t *testing.T) {
	emitEgress(nil, uuid.New(), []*logspb.LogRecord{{}})
	emitEgress(nil, uuid.Nil, []*logspb.LogRecord{{}})
}
