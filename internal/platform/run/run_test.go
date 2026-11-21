package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func httpService(name string, h http.Handler) Service {
	return Service{Name: name, Run: func(ctx context.Context, ln net.Listener) error {
		return Serve(ctx, &http.Server{Handler: h}, ln)
	}}
}

func text(s string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, s) })
}

func get(url string) (string, error) {
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func TestRunAllStartsServicesBeforeEdge(t *testing.T) {
	edgeLn := listen(t)
	got := make(chan string, 2)
	ctx, cancel := context.WithCancel(t.Context())
	edge := func(services map[string]string, hubURL string) Service {
		return Service{Name: "edge", Run: func(ctx context.Context, ln net.Listener) error {
			defer cancel()
			if ln != edgeLn {
				return errors.New("edge got the wrong listener")
			}
			for _, u := range []string{hubURL, services["iam"]} {
				body, err := get(u)
				if err != nil {
					return err
				}
				got <- body
			}
			if _, ok := services["hub"]; ok {
				return errors.New("hub listed as an extracted service")
			}
			return nil
		}}
	}
	err := All(ctx, edgeLn, edge, []Service{httpService("hub", text("hub")), httpService("iam", text("iam"))})
	if err != nil {
		t.Fatal(err)
	}
	if a, b := <-got, <-got; a != "hub" || b != "iam" {
		t.Fatalf("edge reached %q, %q", a, b)
	}
}

func TestRunAllStopsAllOnError(t *testing.T) {
	boom := errors.New("boom")
	cancelled := make(chan struct{})
	waiter := Service{Name: "waiter", Run: func(ctx context.Context, _ net.Listener) error {
		<-ctx.Done()
		close(cancelled)
		return nil
	}}
	failing := Service{Name: "hub", Run: func(context.Context, net.Listener) error { return boom }}
	edge := func(map[string]string, string) Service { return waiter }
	err := All(t.Context(), listen(t), edge, []Service{failing})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "hub") {
		t.Fatalf("err %v, want hub: boom", err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("edge was not cancelled")
	}
}

func TestRunAllShutdownOnCancel(t *testing.T) {
	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(t.Context())
	edgeLn := listen(t)
	edgeAddr := edgeLn.Addr().String()
	var hubAddr string
	edge := func(_ map[string]string, hubURL string) Service {
		hubAddr = strings.TrimPrefix(hubURL, "http://")
		return httpService("edge", text("edge"))
	}
	done := make(chan error, 1)
	go func() { done <- All(ctx, edgeLn, edge, []Service{httpService("hub", text("hub"))}) }()
	if body, err := get("http://" + edgeAddr); err != nil || body != "edge" {
		t.Fatalf("edge: %q %v", body, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("All did not return after cancel")
	}
	for _, addr := range []string{edgeAddr, hubAddr} {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			t.Errorf("%s still accepts connections", addr)
		}
	}
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline {
		t.Fatalf("%d goroutines, baseline %d", n, baseline)
	}
}

func TestServeDrainsBeforeReturning(t *testing.T) {
	ln := listen(t)
	started, release := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		io.WriteString(w, "done")
	})}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, srv, ln) }()
	resp := make(chan string, 1)
	go func() { body, _ := get("http://" + ln.Addr().String()); resp <- body }()
	<-started
	cancel()
	select {
	case err := <-served:
		t.Fatalf("Serve returned with a request in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if body := <-resp; body != "done" {
		t.Fatalf("in-flight request got %q", body)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestRunAllDrainsEdgeBeforeServices(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	edgeDone := make(chan struct{})
	hubSawEdgeDone := make(chan bool, 1)
	edge := func(map[string]string, string) Service {
		return Service{Name: "edge", Run: func(ctx context.Context, _ net.Listener) error {
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond)
			close(edgeDone)
			return nil
		}}
	}
	hub := Service{Name: "hub", Run: func(ctx context.Context, _ net.Listener) error {
		<-ctx.Done()
		select {
		case <-edgeDone:
			hubSawEdgeDone <- true
		default:
			hubSawEdgeDone <- false
		}
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- All(ctx, listen(t), edge, []Service{hub}) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !<-hubSawEdgeDone {
		t.Fatal("hub was cancelled before the edge had drained")
	}
}

func TestRunAllListensOnServiceAddr(t *testing.T) {
	free := listen(t)
	port := free.Addr().(*net.TCPAddr).Port
	free.Close()
	got := make(chan string, 1)
	edge := func(_ map[string]string, hubURL string) Service {
		return Service{Name: "edge", Run: func(context.Context, net.Listener) error {
			got <- hubURL
			body, err := get(hubURL)
			if err == nil && body != "hub" {
				err = errors.New("hub answered " + body)
			}
			return err
		}}
	}
	hub := httpService("hub", text("hub"))
	hub.Addr = fmt.Sprintf(":%d", port)
	if err := All(t.Context(), listen(t), edge, []Service{hub}); err != nil {
		t.Fatal(err)
	}
	if u, want := <-got, fmt.Sprintf("http://127.0.0.1:%d", port); u != want {
		t.Fatalf("hub URL %q, want %q", u, want)
	}
}
