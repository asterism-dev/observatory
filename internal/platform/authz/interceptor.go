package authz

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	coreauthz "github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/platform/clientip"
	"github.com/asterism-dev/observatory/internal/platform/identity"
	"github.com/asterism-dev/observatory/internal/platform/permission"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type interceptor struct {
	verifier *identity.Verifier
	watcher  *Watcher
}

// NewInterceptor authorizes every handler call by its permission annotation against w's latest snapshot.
func NewInterceptor(v *identity.Verifier, w *Watcher) connect.Interceptor {
	return &interceptor{verifier: v, watcher: w}
}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		ctx, err := i.authorize(ctx, req)
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// ponytail: streaming handlers are refused until one exists; authorize on the first Receive then.
func (i *interceptor) WrapStreamingHandler(connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(context.Context, connect.StreamingHandlerConn) error {
		return connect.NewError(connect.CodeUnimplemented, errors.New("streaming methods are not authorized"))
	}
}

func (i *interceptor) authorize(ctx context.Context, req connect.AnyRequest) (context.Context, error) {
	raw, ok := strings.CutPrefix(req.Header().Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing identity"))
	}
	p, err := i.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid identity"))
	}
	md, _ := req.Spec().Schema.(protoreflect.MethodDescriptor)
	if md == nil {
		return nil, errBadAnnotation()
	}
	perm, ok := permission.Of(md)
	if !ok {
		return nil, errBadAnnotation()
	}
	// One Load per call so membership and enforcer come from the same snapshot.
	snap := i.watcher.cur.Load()
	if snap == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("no policy snapshot yet"))
	}
	msg, ok := req.Any().(proto.Message)
	if !ok {
		return nil, errBadAnnotation()
	}
	m := msg.ProtoReflect()
	// Only now: the header is the edge's, and a verified JWT means the call came through it (or holds a leaked token).
	ctx = identity.WithVerified(ctx, p, req.Header().Get(clientip.HeaderClientIP))
	orgField := m.Descriptor().Fields().ByName("org")
	if orgField == nil {
		return ctx, platformScope(p, perm, m)
	}
	if orgField.Kind() != protoreflect.StringKind || orgField.Cardinality() == protoreflect.Repeated {
		return nil, errBadAnnotation()
	}
	org, err := uuidx.Canonical(m.Get(orgField).String())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("org: %w", err))
	}
	// Checked before the object so non-members learn nothing about the request's validity.
	if !snap.members[orgUser{org, p.User}] {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("not found"))
	}
	obj, err := ResolveObject(perm.Object, m)
	if err != nil {
		return nil, err
	}
	if !snap.enforcer.Allowed(coreauthz.UserSubject(p.User), org, obj, perm.Name) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("permission denied"))
	}
	return ctx, nil
}

// platformScope admits platform admins, and other users only for objects under their own user: scope.
func platformScope(p identity.Principal, perm permission.Permission, m protoreflect.Message) error {
	if p.PlatformAdmin {
		return nil
	}
	if strings.HasPrefix(perm.Object, "user:") {
		obj, err := ResolveObject(perm.Object, m)
		if err != nil {
			return err
		}
		own := coreauthz.UserSubject(p.User)
		if obj == own || strings.HasPrefix(obj, own+"/") {
			return nil
		}
	}
	return connect.NewError(connect.CodePermissionDenied, errors.New("permission denied"))
}
