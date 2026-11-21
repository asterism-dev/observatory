package iam

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	hubv1 "github.com/asterism-dev/observatory/gen/go/observatory/hub/v1"
	"github.com/asterism-dev/observatory/internal/authz"
)

// reloadWait bounds the policy reload on the escalation path, like the hub's CanHold wait.
var reloadWait = 5 * time.Second

// canGrant is CanGrantRole on iam's policy; on an escalation it asks the hub, whose policy adds the grants of service-owned objects.
func (s *server) canGrant(ctx context.Context, e *authz.Enforcer, actor string, org uuid.UUID, isAdmin bool, rules []authz.Rule, parents []string) error {
	denied := e.CanGrantRole(actor, org, isAdmin, rules, parents)
	if !errors.Is(denied, authz.ErrEscalation) {
		return denied
	}
	inherited, err := e.EffectiveRules(parents, org)
	if err != nil {
		return fmt.Errorf("%w: %w", authz.ErrInvalidPattern, err)
	}
	// Under the caller's LockOrg this sees every earlier commit for org; bounded since it needs a second connection while the caller holds one.
	rctx, cancel := context.WithTimeout(ctx, reloadWait)
	defer cancel()
	if err := s.Policy.Reload(rctx); err != nil {
		slog.Error("policy reload before can hold", "org", org, "err", err)
		return connect.NewError(connect.CodeUnavailable, errors.New("cannot check the grant against service-owned resources, retry"))
	}
	req := &hubv1.CanHoldRequest{OrgId: org.String(), Actor: actor, MinRevision: s.Revision()}
	for _, r := range slices.Concat(rules, inherited) {
		req.Rules = append(req.Rules, &hubv1.Rule{Effect: string(r.Effect), Object: r.Object, Action: r.Action})
	}
	_, err = s.Resources.CanHold(ctx, connect.NewRequest(req))
	var ce *connect.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ce) && ce.Code() == connect.CodePermissionDenied:
		// The hub's detail names the action it found missing, which may differ from iam's first miss.
		if detail := strings.TrimPrefix(ce.Message(), authz.ErrEscalation.Error()); detail != "" {
			return fmt.Errorf("%w%s", authz.ErrEscalation, detail)
		}
		return denied
	}
	slog.Error("can hold at hub", "org", org, "err", err)
	return connect.NewError(connect.CodeUnavailable, errors.New("cannot check the grant against service-owned resources, retry"))
}
