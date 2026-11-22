package iam

import (
	"context"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"

	iamv1 "github.com/asterism-dev/observatory/gen/go/observatory/iam/v1"
	"github.com/asterism-dev/observatory/gen/go/observatory/iam/v1/iamv1connect"
	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/iam/store"
	"github.com/asterism-dev/observatory/internal/platform/catalog"
	"github.com/asterism-dev/observatory/internal/platform/servicetoken"
)

// catalogServer serves CatalogInternalService; the caller wraps it in servicetoken.Middleware.
type catalogServer struct {
	Store  *store.Store
	Policy *authz.Service
}

var _ iamv1connect.CatalogInternalServiceHandler = catalogServer{}

func (s catalogServer) RegisterCatalog(ctx context.Context, req *connect.Request[iamv1.RegisterCatalogRequest]) (*connect.Response[iamv1.RegisterCatalogResponse], error) {
	caller, _ := servicetoken.Caller(ctx)
	c, err := catalog.FromProto(req.Msg.GetCatalog())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	digest, err := registerCatalog(ctx, s.Store, s.Policy, caller, c)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&iamv1.RegisterCatalogResponse{Digest: digest}), nil
}

// registerCatalog stores caller's catalog and publishes it if it changed; the allowed prefixes come from caller, never from c.Service.
func registerCatalog(ctx context.Context, s *store.Store, z *authz.Service, caller string, c authz.Catalog) (string, error) {
	if c.Service != caller {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("catalog of %q registered by %q", c.Service, caller))
	}
	if err := catalog.Validate(c, AllowedPrefixes(caller)); err != nil {
		return "", connect.NewError(connect.CodeInvalidArgument, err)
	}
	digest, changed, err := s.PutCatalog(ctx, caller, c)
	if err != nil {
		return "", internal("store catalog", err)
	}
	if changed {
		slog.Info("catalog registered", "service", caller, "digest", digest)
		// Committed: a failed reload is retried and withholds the stream meanwhile, as after any commit.
		if err := z.ReloadCommitted(ctx); err != nil {
			slog.Error("authz reload after catalog registration", "err", err)
		}
	}
	return digest, nil
}

// registerOwnCatalog stores iam's own catalog; iam registers it at start, before serving.
func registerOwnCatalog(ctx context.Context, s *store.Store, z *authz.Service) error {
	c, err := Catalog()
	if err != nil {
		return fmt.Errorf("iam catalog: %w", err)
	}
	if _, err := registerCatalog(ctx, s, z, "iam", c); err != nil {
		return fmt.Errorf("register iam catalog: %w", err)
	}
	return nil
}
