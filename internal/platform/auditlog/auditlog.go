// Package auditlog records a service's calls as audit events through its outbox.
package auditlog

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"

	eventsv1 "github.com/asterism-dev/observatory/gen/go/observatory/events/v1"
	"github.com/asterism-dev/observatory/internal/platform/authz"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/outbox"
	"github.com/asterism-dev/observatory/internal/platform/permission"
	"github.com/asterism-dev/observatory/internal/platform/textx"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
)

const writeTimeout = 5 * time.Second

type interceptor struct {
	svc                  string
	settings             *Settings
	pool                 *pgxpool.Pool
	handlerRecordsWrites bool
}

type Option func(*interceptor)

// HandlerRecordsWrites leaves allowed and failed writes to handlers that record them inside their own transaction; denials are still recorded here.
func HandlerRecordsWrites() Option {
	return func(i *interceptor) { i.handlerRecordsWrites = true }
}

// Interceptors is the chain every service handler must use: auditing outside authz, since authz stops denied calls before any inner interceptor.
func Interceptors(svc string, v *identity.Verifier, s authz.Source, settings *Settings, pool *pgxpool.Pool, opts ...Option) []connect.Interceptor {
	return []connect.Interceptor{NewInterceptor(svc, settings, pool, opts...), authz.NewInterceptor(v, s)}
}

// NewInterceptor records writes and authz denials of verified callers, and reads with data events on; use it through Interceptors.
func NewInterceptor(svc string, settings *Settings, pool *pgxpool.Pool, opts ...Option) connect.Interceptor {
	i := &interceptor{svc: svc, settings: settings, pool: pool}
	for _, o := range opts {
		o(i)
	}
	return i
}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		ctx, caller := identity.WithCallerSlot(ctx)
		resp, err := next(ctx, req)
		// Unverified callers are not recorded: anyone could fill the trail with them.
		if caller.Verified {
			i.record(ctx, req, caller, err)
		}
		return resp, err
	}
}

// SetDetails attaches d to the call's audit event; values must suit structpb (nil, bool, numbers, strings, maps, slices).
func SetDetails(ctx context.Context, d map[string]any) { identity.SetDetails(ctx, d) }

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func (i *interceptor) record(ctx context.Context, req connect.AnyRequest, caller *identity.Caller, callErr error) {
	md, _ := req.Spec().Schema.(protoreflect.MethodDescriptor)
	msg, _ := req.Any().(proto.Message)
	if md == nil || msg == nil {
		return
	}
	perm, ok := permission.Of(md)
	if !ok {
		return
	}
	m := msg.ProtoReflect()
	org := ""
	if fd := m.Descriptor().Fields().ByName("org"); fd != nil && fd.Kind() == protoreflect.StringKind && fd.Cardinality() != protoreflect.Repeated {
		if _, err := uuidx.Canonical(m.Get(fd).String()); err == nil {
			org = m.Get(fd).String()
		}
	}
	res := eventsv1.Result_RESULT_ALLOWED
	switch {
	case caller.Denied:
		res = eventsv1.Result_RESULT_DENIED
	case callErr != nil:
		res = eventsv1.Result_RESULT_FAILED
	}
	if !caller.Denied && (perm.Write && i.handlerRecordsWrites || !perm.Write && !i.settings.DataEvents(org, i.svc)) {
		return
	}
	resource, err := authz.ResolveObject(perm.Object, caller.Principal.User, m)
	if err != nil {
		resource = perm.Object
	}
	e := &eventsv1.Event{
		Type: perm.Name, Org: org, Resource: resource, Result: res, Audit: true,
		Actor:    &eventsv1.Actor{Kind: eventsv1.ActorKind_ACTOR_KIND_USER, Id: caller.Principal.User.String()},
		ClientIp: caller.ClientIP, UserAgent: textx.Sanitize(req.Header().Get("User-Agent")),
	}
	if caller.Details != nil {
		if d, err := structpb.NewStruct(caller.Details); err != nil {
			slog.Error("audit details dropped", "type", e.GetType(), "err", err)
		} else {
			e.Details = d
		}
	}
	// The call already happened, so the event is written even if the client went away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err := pgx.BeginFunc(ctx, i.pool, func(tx pgx.Tx) error { return outbox.Write(ctx, tx, e) }); err != nil {
		slog.Error("audit event lost", "type", e.GetType(), "org", org, "actor", e.GetActor().GetId(), "err", err)
	}
}
