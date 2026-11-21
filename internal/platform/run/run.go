// Package run starts services in one process the way they would run apart: each on its own listener, talking over HTTP.
package run

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

type Service struct {
	Name string
	Run  func(ctx context.Context, ln net.Listener) error
}

// All binds every service on an ephemeral loopback port, starts the edge on edgeLn last with their URLs, and stops all once any returns.
func All(ctx context.Context, edgeLn net.Listener, edge func(services map[string]string, hubURL string) Service, svcs []Service) error {
	lns := make([]net.Listener, 0, len(svcs))
	services := make(map[string]string, len(svcs))
	for _, s := range svcs {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			edgeLn.Close()
			for _, l := range lns {
				l.Close()
			}
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		lns = append(lns, ln)
		services[s.Name] = "http://" + ln.Addr().String()
	}
	hubURL := services["hub"]
	delete(services, "hub")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, len(svcs)+1)
	start := func(s Service, ln net.Listener) {
		go func() {
			err := s.Run(ctx, ln)
			ln.Close()
			if err != nil {
				err = fmt.Errorf("%s: %w", s.Name, err)
			}
			errs <- err
			cancel()
		}()
	}
	for i, s := range svcs {
		start(s, lns[i])
	}
	start(edge(services, hubURL), edgeLn)
	var first error
	for range len(svcs) + 1 {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Serve serves srv on ln until ctx is done, then drains in-flight requests for up to 10s before returning.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := srv.Shutdown(shutdown)
	if err != nil {
		srv.Close()
	}
	if serr := <-served; !errors.Is(serr, http.ErrServerClosed) {
		return serr
	}
	return err
}
