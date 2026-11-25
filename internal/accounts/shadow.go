package accounts

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/policy"
	"github.com/asterism-dev/observatory/internal/store"
)

type Outcome int

const (
	Same Outcome = iota
	Regression
	Widening
	Skipped
)

func (o Outcome) String() string {
	return [...]string{"same", "regression", "widening", "skipped"}[o]
}

type shadowState struct {
	snap   store.AccountSnapshot
	rights map[uuid.UUID]policy.Rights
}

// Shadow evaluates the account model next to the current one and logs where they disagree; it never decides.
type Shadow struct {
	load   func(context.Context) (store.AccountSnapshot, error)
	state  atomic.Pointer[shadowState]
	counts [4]atomic.Int64
}

func NewShadow(load func(context.Context) (store.AccountSnapshot, error)) *Shadow {
	return &Shadow{load: load}
}

// Reload rebuilds the derived rights for p; on error the previous state stays.
func (sh *Shadow) Reload(ctx context.Context, p authz.Policy) {
	snap, err := sh.load(ctx)
	if err != nil {
		slog.Error("accounts shadow: load accounts", "err", err)
		return
	}
	sh.state.Store(&shadowState{snap: snap, rights: Derive(p, snap)})
}

func (sh *Shadow) Compare(user, org uuid.UUID, obj, act string, old bool) (out Outcome) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("accounts shadow: panic", "panic", r, "object", obj, "action", act)
			out = Skipped
		}
		sh.counts[out].Add(1)
	}()
	st := sh.state.Load()
	if st == nil {
		return Skipped
	}
	q, ok := Translate(st.snap, org, obj, act)
	if !ok {
		return Skipped
	}
	d := policy.Decide(st.rights[user], q)
	switch {
	case old == d.Allowed:
		return Same
	case old:
		slog.Warn("accounts shadow: regression", "user", user, "org", org, "object", obj, "action", act,
			"resource", q.Resource.String(), "reason", d.Reason)
		return Regression
	default:
		return Widening
	}
}

func (sh *Shadow) Counts() map[Outcome]int64 {
	out := map[Outcome]int64{}
	for o := range sh.counts {
		out[Outcome(o)] = sh.counts[o].Load()
	}
	return out
}
