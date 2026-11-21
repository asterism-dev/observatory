package authz

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	coreauthz "github.com/asterism-dev/observatory/internal/authz"
)

func TestPolicyPublishDoesNotBlock(t *testing.T) {
	ps := NewPublisher()
	// A subscriber that never drains must not stall Publish (which runs under authz's reload lock).
	ch := ps.subscribe()
	defer ps.unsubscribe(ch)
	done := make(chan struct{})
	go func() {
		for range 1000 {
			ps.Publish(coreauthz.Policy{Teams: []coreauthz.TeamRef{{OrgID: uuid.New(), ID: uuid.New()}}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
	if got := ps.latest(); got == nil || got.Revision == 0 {
		t.Fatalf("latest: %v", got)
	}
}

func TestPolicyPublishSkipsUnchanged(t *testing.T) {
	ps := NewPublisher()
	ps.Publish(coreauthz.Policy{})
	rev := ps.latest().Revision
	ps.Publish(coreauthz.Policy{})
	if got := ps.latest().Revision; got != rev {
		t.Fatalf("unchanged snapshot bumped revision %d → %d", rev, got)
	}
	ps.Publish(coreauthz.Policy{Teams: []coreauthz.TeamRef{{OrgID: uuid.New(), ID: uuid.New()}}})
	if got := ps.latest().Revision; got <= rev {
		t.Fatalf("changed snapshot kept revision %d", got)
	}
}

func TestPolicyWaitRevision(t *testing.T) {
	ps := NewPublisher()
	ps.Publish(coreauthz.Policy{})
	rev := ps.Revision()
	if err := ps.WaitRevision(t.Context(), rev); err != nil {
		t.Fatalf("current revision: %v", err)
	}
	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := ps.WaitRevision(short, rev+1); err == nil {
		t.Fatal("returned before the revision was published")
	}
	done := make(chan error, 1)
	go func() { done <- ps.WaitRevision(t.Context(), rev+1) }()
	time.Sleep(20 * time.Millisecond)
	ps.Publish(coreauthz.Policy{Teams: []coreauthz.TeamRef{{OrgID: uuid.New(), ID: uuid.New()}}})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitRevision missed the publish")
	}
}
