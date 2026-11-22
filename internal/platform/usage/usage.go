// Package usage encodes the lifecycle events an owning service writes to its outbox and quota turns into allocations.
package usage

import (
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/structpb"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
)

const (
	InstanceCreated       = "compute.instances.created"
	InstanceDeleted       = "compute.instances.deleted"
	DiskCreated           = "compute.disks.created"
	DiskResized           = "compute.disks.resized"
	DiskDeleted           = "compute.disks.deleted"
	SnapshotCreated       = "compute.snapshots.created"
	SnapshotDeleted       = "compute.snapshots.deleted"
	ReservationsCommitted = "quota.reservations.committed"
)

// Amounts are a resource's absolute usage.
type Amounts struct{ VCPU, RAMMB, DiskGB, VMCount, SnapshotCount int }

// Lifecycle names a quota resource (vm/<id>, disk/<id>, snapshot/<id>), its parent resource if any, its owner (user:<id> or team:<id>) and its usage.
type Lifecycle struct {
	Resource, Parent, Owner string
	Amounts                 Amounts
}

// names are the detail keys of Amounts' fields, in fields order.
var names = [5]string{"vcpu", "ramMb", "diskGb", "vmCount", "snapshotCount"}

func (a *Amounts) fields() [5]*int { return [5]*int{&a.VCPU, &a.RAMMB, &a.DiskGB, &a.VMCount, &a.SnapshotCount} }

// Event is a lifecycle event of type typ on the authz object, not audited.
func Event(org uuid.UUID, typ, object string, l Lifecycle) *eventsv1.Event {
	f := map[string]*structpb.Value{
		"resource": structpb.NewStringValue(l.Resource),
		"parent":   structpb.NewStringValue(l.Parent),
		"owner":    structpb.NewStringValue(l.Owner),
	}
	for i, v := range l.Amounts.fields() {
		f[names[i]] = structpb.NewNumberValue(float64(*v))
	}
	return &eventsv1.Event{Type: typ, Org: org.String(), Resource: object, Result: eventsv1.Result_RESULT_ALLOWED, Details: &structpb.Struct{Fields: f}}
}

// Committed tells quota that the reservations ids have turned into the allocations of the events written before it.
func Committed(org uuid.UUID, ids []uuid.UUID) *eventsv1.Event {
	vs := make([]*structpb.Value, len(ids))
	for i, id := range ids {
		vs[i] = structpb.NewStringValue(id.String())
	}
	return &eventsv1.Event{Type: ReservationsCommitted, Org: org.String(), Result: eventsv1.Result_RESULT_ALLOWED,
		Details: &structpb.Struct{Fields: map[string]*structpb.Value{"reservations": structpb.NewListValue(&structpb.ListValue{Values: vs})}}}
}

// Parse reads a lifecycle event's details; amounts must be whole numbers in 0…2147483647.
func Parse(e *eventsv1.Event) (Lifecycle, error) {
	f := e.GetDetails().GetFields()
	l := Lifecycle{Resource: f["resource"].GetStringValue(), Parent: f["parent"].GetStringValue(), Owner: f["owner"].GetStringValue()}
	if l.Resource == "" {
		return Lifecycle{}, errors.New("lifecycle event without resource")
	}
	for i, p := range l.Amounts.fields() {
		v, ok := f[names[i]]
		if !ok {
			continue
		}
		n, isNum := v.GetKind().(*structpb.Value_NumberValue)
		if !isNum || n.NumberValue < 0 || n.NumberValue > math.MaxInt32 || n.NumberValue != math.Trunc(n.NumberValue) {
			return Lifecycle{}, fmt.Errorf("lifecycle event: invalid %s", names[i])
		}
		*p = int(n.NumberValue)
	}
	return l, nil
}

// ParseCommitted reads a committed event's reservation ids; the list must be present, though it may be empty.
func ParseCommitted(e *eventsv1.Event) ([]uuid.UUID, error) {
	l, ok := e.GetDetails().GetFields()["reservations"].GetKind().(*structpb.Value_ListValue)
	if !ok {
		return nil, errors.New("committed event without reservations")
	}
	vs := l.ListValue.GetValues()
	ids := make([]uuid.UUID, 0, len(vs))
	for _, v := range vs {
		id, err := uuidx.Canonical(v.GetStringValue())
		if err != nil {
			return nil, fmt.Errorf("committed event: reservation: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
