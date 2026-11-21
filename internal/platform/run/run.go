// Package run starts services in one process the way they would run apart: each on its own listener, talking over HTTP.
package run

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

type Service struct {
	Name string
	// Addr is where the service listens; empty means an ephemeral loopback port.
	Addr string
	Run  func(ctx context.Context, ln net.Listener) error
}

// All binds every service, calls edge with their URLs before starting any, starts the edge on edgeLn last, and stops all once any returns; on cancel the edge drains before the services stop.
func All(ctx context.Context, edgeLn net.Listener, edge func(services map[string]string, hubURL string) Service, svcs []Service) error {
	lns := make([]net.Listener, 0, len(svcs))
	services := make(map[string]string, len(svcs))
	for _, s := range svcs {
		addr := s.Addr
		if addr == "" {
			addr = "127.0.0.1:0"
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			edgeLn.Close()
			for _, l := range lns {
				l.Close()
			}
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		lns = append(lns, ln)
		services[s.Name] = "http://" + dialAddr(ln.Addr().(*net.TCPAddr))
	}
	hubURL := services["hub"]
	delete(services, "hub")

	// Services outlive the parent context: they stop once the edge has returned.
	svcCtx, cancelSvcs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelSvcs()
	edgeCtx, cancelEdge := context.WithCancel(ctx)
	defer cancelEdge()
	errs := make(chan error, len(svcs)+1)
	start := func(ctx context.Context, s Service, ln net.Listener) {
		go func() {
			err := s.Run(ctx, ln)
			ln.Close()
			if err != nil {
				err = fmt.Errorf("%s: %w", s.Name, err)
			}
			errs <- err
			cancelEdge()
			cancelSvcs()
		}()
	}
	edgeSvc := edge(services, hubURL)
	for i, s := range svcs {
		start(svcCtx, s, lns[i])
	}
	start(edgeCtx, edgeSvc, edgeLn)
	var first error
	for range len(svcs) + 1 {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

func dialAddr(a *net.TCPAddr) string {
	if a.IP.IsUnspecified() {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(a.Port))
	}
	return a.String()
}

var drainTimeout = 10 * time.Second

// Serve serves srv on ln until ctx is done, then drains in-flight requests for up to drainTimeout and closes what is left.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	err := srv.Shutdown(shutdown)
	if errors.Is(err, context.DeadlineExceeded) {
		err = srv.Close()
	} else if err != nil {
		srv.Close()
	}
	if serr := <-served; !errors.Is(serr, http.ErrServerClosed) {
		return serr
	}
	return err
}
