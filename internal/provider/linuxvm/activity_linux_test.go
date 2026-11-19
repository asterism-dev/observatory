package linuxvm

import (
	"context"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
)

func ipCmd(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
		t.Fatalf("ip %v: %v: %s", args, err, out)
	}
}

func TestWaitIdleIgnoresDNS(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	old := activityBackoff
	activityBackoff = 500 * time.Millisecond
	t.Cleanup(func() { activityBackoff = old })
	exec.Command("ip", "netns", "del", "obsidle").Run()
	ipCmd(t, "netns", "add", "obsidle")
	t.Cleanup(func() { exec.Command("ip", "netns", "del", "obsidle").Run() })
	ipCmd(t, "link", "add", "obsidle0", "type", "veth", "peer", "name", "obsidle1", "netns", "obsidle")
	t.Cleanup(func() { exec.Command("ip", "link", "del", "obsidle0").Run() })
	ipCmd(t, "addr", "add", "10.250.0.1/30", "dev", "obsidle0")
	ipCmd(t, "link", "set", "obsidle0", "up")
	ipCmd(t, "-n", "obsidle", "addr", "add", "10.250.0.2/30", "dev", "obsidle1")
	ipCmd(t, "-n", "obsidle", "link", "set", "obsidle1", "up")

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			if c, err := net.Dial("udp", "10.250.0.2:53"); err == nil {
				c.Write([]byte("\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x03dev\x08internal\x00\x00\x01\x00\x01"))
				c.Close()
			}
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	if err := WaitIdle(ctx, []string{"obsidle0"}, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 3*time.Second || d > 4500*time.Millisecond {
		t.Fatalf("idle after %s with DNS only", d)
	}

	connected := make(chan time.Time, 1)
	go func() {
		time.Sleep(time.Second)
		connected <- time.Now()
		if c, err := net.DialTimeout("tcp", "10.250.0.2:22", 500*time.Millisecond); err == nil {
			c.Close()
		}
	}()
	if err := WaitIdle(ctx, []string{"obsidle0"}, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(<-connected); d < 3*time.Second {
		t.Fatalf("idle %s after a TCP connect", d)
	}
}

func TestWaitWakeOnTap(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := net.InterfaceByName(NATBridge); err != nil {
		t.Skip("needs bridge " + NATBridge)
	}
	f := node.VMFile{Spec: proto.VMSpec{ID: "wakeontap"}}
	t.Cleanup(func() { RemoveTaps(f) })
	// A MAC the bridge has not learned is flooded to every port, the tap included.
	ipCmd(t, "neigh", "replace", "192.168.64.250", "lladdr", "02:00:00:00:fa:fa", "dev", NATBridge, "nud", "permanent")
	t.Cleanup(func() { exec.Command("ip", "neigh", "del", "192.168.64.250", "dev", NATBridge).Run() })
	done := make(chan error, 1)
	go func() { done <- WaitWake(context.Background(), f, 0) }()
	tap := TapName("wakeontap", 0)
	time.Sleep(time.Second)
	exec.Command("ping", "-c", "2", "-W", "1", "192.168.64.250").Run()
	select {
	case err := <-done:
		t.Fatalf("woke on ping: %v", err)
	default:
	}
	net.DialTimeout("tcp", "192.168.64.250:22", 300*time.Millisecond)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no wake on a TCP SYN")
	}
	fd, err := openTap(tap)
	if err != nil {
		t.Fatalf("tap not left free for Restore: %v", err)
	}
	fd.Close()
}
