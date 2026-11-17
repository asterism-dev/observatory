package node

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeVsock serves one connection like Firecracker's vsock uds: handshake, then the guest's reply.
func fakeVsock(t *testing.T, handshake, reply string) (string, <-chan string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "gc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	uds := filepath.Join(dir, "v.sock")
	l, err := net.Listen("unix", uds)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	got := make(chan string, 2)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		line, _ := r.ReadString('\n')
		got <- line
		c.Write([]byte(handshake))
		if !strings.HasPrefix(handshake, "OK ") {
			return
		}
		req, _ := r.ReadString('\n')
		got <- req
		c.Write([]byte(reply))
	}()
	return uds, got
}

func TestCallGuestProtocol(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	uds, got := fakeVsock(t, "OK 1073741824\n", `{"ok":true}`+"\n")
	if err := CallGuest(ctx, uds, GuestRequest{Op: "resume", Time: 5}); err != nil {
		t.Fatal(err)
	}
	if l := <-got; l != "CONNECT 1050\n" {
		t.Fatalf("handshake = %q", l)
	}
	if l := <-got; l != `{"op":"resume","time":5}`+"\n" {
		t.Fatalf("request = %q", l)
	}

	uds, _ = fakeVsock(t, "OK 1\n", `{"ok":false,"error":"boom"}`+"\n")
	if err := CallGuest(ctx, uds, GuestRequest{Op: "ping"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want boom", err)
	}

	uds, _ = fakeVsock(t, "", "")
	if err := CallGuest(ctx, uds, GuestRequest{Op: "ping"}); err == nil {
		t.Fatal("expected an error without OK")
	}
}
