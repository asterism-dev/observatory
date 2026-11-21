package main

import (
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/asterism-dev/observatory/internal/proto"
)

func pack(t *testing.T, name string, qt dnsmessage.Type) []byte {
	t.Helper()
	b, err := (&dnsmessage.Message{Header: dnsmessage.Header{ID: 7, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: qt, Class: dnsmessage.ClassINET}}}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func unpack(t *testing.T, b []byte) dnsmessage.Message {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(b); err != nil {
		t.Fatal(err)
	}
	return m
}

func query(t *testing.T, addr, name string, qt dnsmessage.Type) dnsmessage.Message {
	t.Helper()
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(pack(t, name, qt))
	buf := make([]byte, 1500)
	k, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return unpack(t, buf[:k])
}

func ask(t *testing.T, s *dnsServer, src, name string) dnsmessage.Message {
	t.Helper()
	b, err := s.handle("udp", pack(t, name, dnsmessage.TypeA), []string{"127.0.0.1:1"}, netip.MustParseAddr(src))
	if err != nil {
		t.Fatal(err)
	}
	return unpack(t, b)
}

func answer(m dnsmessage.Message) string {
	if len(m.Answers) != 1 {
		return ""
	}
	a := m.Answers[0].Body.(*dnsmessage.AResource).A
	return netip.AddrFrom4(a).String()
}

func viewsServer() *dnsServer {
	s := newDNSServer()
	s.setViews([]proto.GatewayOrg{
		{ID: orgA, Slice: "10.64.0.0/20", Records: map[string]string{"dev.internal": "10.64.0.10", "dev.build.internal": "10.210.0.10"}},
		{ID: orgB, Slice: "10.64.16.0/20", Records: map[string]string{"Dev.internal": "10.64.16.10"}},
	}, []proto.GatewayDevice{{IP: "10.192.0.10", OrgID: orgA}, {IP: "10.192.0.20", OrgID: orgB}})
	return s
}

func TestDNSViews(t *testing.T) {
	s := viewsServer()
	for _, c := range []struct{ src, name, want string }{
		{"10.192.0.10", "dev.internal.", "10.64.0.10"},
		{"10.192.0.20", "dev.internal.", "10.64.16.10"},
		{"10.64.0.11", "dev.internal.", "10.64.0.10"},
		{"10.64.16.11", "DEV.internal.", "10.64.16.10"},
		{"10.192.0.10", "dev.build.internal.", "10.210.0.10"},
	} {
		if m := ask(t, s, c.src, c.name); answer(m) != c.want || m.RCode != dnsmessage.RCodeSuccess || !m.Authoritative {
			t.Errorf("%s from %s: %+v", c.name, c.src, m)
		}
	}
	if m := ask(t, s, "10.192.0.20", "dev.build.internal."); m.RCode != dnsmessage.RCodeNameError {
		t.Errorf("org B sees org A network record: %+v", m)
	}
	for _, n := range []string{"build.internal.", "internal."} {
		if m := ask(t, s, "10.192.0.10", n); m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
			t.Errorf("%s: %+v", n, m)
		}
	}
	s.setViews(nil, nil)
	if m := ask(t, s, "10.192.0.10", "dev.internal."); m.RCode != dnsmessage.RCodeRefused {
		t.Errorf("after clear: %+v", m)
	}
}

func TestDNSUnknownSourceRefused(t *testing.T) {
	s := viewsServer()
	for _, src := range []string{"10.210.0.11", "10.198.0.11", "10.192.0.99", "10.192.0.1", "127.0.0.1"} {
		if m := ask(t, s, src, "dev.internal."); m.RCode != dnsmessage.RCodeRefused || len(m.Answers) != 0 {
			t.Errorf("%s: %+v", src, m)
		}
	}
	// The gateway's own addresses: Docker's DNS asking it back when the host resolves through a device.
	for _, src := range []string{"10.192.0.1", "10.198.0.1", "10.199.0.2", "127.0.0.1", "::1"} {
		if m := ask(t, s, src, "example.com."); m.RCode != dnsmessage.RCodeRefused || len(m.Answers) != 0 {
			t.Errorf("forward for %s: %+v", src, m)
		}
	}
}

func TestDNSServesAndForwards(t *testing.T) {
	up, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer up.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			k, from, err := up.ReadFrom(buf)
			if err != nil {
				return
			}
			var m dnsmessage.Message
			if m.Unpack(buf[:k]) != nil {
				continue
			}
			m.Response = true
			m.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
				Body: &dnsmessage.AResource{A: [4]byte{9, 9, 9, 9}}}}
			out, _ := m.Pack()
			up.WriteTo(out, from)
		}
	}()
	conn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer conn.Close()
	s := newDNSServer()
	s.setViews([]proto.GatewayOrg{{ID: orgA, Slice: "127.0.0.0/8", Records: map[string]string{"dev.internal": "10.64.0.10"}}}, nil)
	dead, _ := net.ListenPacket("udp", "127.0.0.1:0")
	dead.Close()
	upstreams := []string{dead.LocalAddr().String(), up.LocalAddr().String()}
	go s.serveUDP(conn, upstreams)
	addr := conn.LocalAddr().String()

	if m := query(t, addr, "dev.internal.", dnsmessage.TypeA); answer(m) != "10.64.0.10" {
		t.Fatalf("A: %+v", m)
	}
	if m := query(t, addr, "dev.internal.", dnsmessage.TypeAAAA); m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Fatalf("AAAA: %+v", m)
	}
	if m := query(t, addr, "gone.internal.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeNameError {
		t.Fatalf("gone: %+v", m)
	}
	if m := query(t, addr, "example.com.", dnsmessage.TypeA); answer(m) != "9.9.9.9" {
		t.Fatalf("forward: %+v", m)
	}
	s.setViews(nil, nil)
	if m := query(t, addr, "dev.internal.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("unknown source: %+v", m)
	}
	if m := query(t, addr, "example.com.", dnsmessage.TypeA); m.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("forward for the gateway itself: %+v", m)
	}
	// A device or VM the gateway has no state for yet still resolves public names.
	b, err := s.handle("udp", pack(t, "example.com.", dnsmessage.TypeA), upstreams, netip.MustParseAddr("10.192.0.99"))
	if err != nil {
		t.Fatal(err)
	}
	if m := unpack(t, b); answer(m) != "9.9.9.9" {
		t.Fatalf("forward for an unknown device: %+v", m)
	}
}

func writeConf(t *testing.T, content string) string {
	t.Helper()
	p := t.TempDir() + "/resolv.conf"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadUpstreamsOverride(t *testing.T) {
	resolved := writeConf(t, "nameserver 192.168.1.1\n")
	for in, want := range map[string]string{
		"10.0.0.53":                        "10.0.0.53:53",
		" 10.0.0.53 , 10.0.0.54:5353 ":     "10.0.0.53:53 10.0.0.54:5353",
		"2001:db8::53,[2001:db8::54]:5353": "[2001:db8::53]:53 [2001:db8::54]:5353",
	} {
		got, err := loadUpstreams(in, resolved)
		if err != nil || strings.Join(got, " ") != want {
			t.Errorf("%q: %v %v, want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"dns.example", "10.0.0.53,", "10.0.0.1:x"} {
		if _, err := loadUpstreams(in, resolved); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestLoadUpstreamsFromHost(t *testing.T) {
	// systemd-resolved's full list on the hub host while a device with DNS = 10.192.0.1 is up.
	resolved := writeConf(t, `# This is /run/systemd/resolve/resolv.conf managed by man:systemd-resolved(8).
nameserver 192.168.178.1
nameserver 10.192.0.1
nameserver 10.198.0.1
nameserver 10.192.0.7
nameserver 127.0.0.53
nameserver ::1
nameserver fe80::1%2
nameserver 2001:db8::1
search fritz.box
`)
	stub := writeConf(t, "nameserver 127.0.0.53\noptions edns0 trust-ad\nsearch .\n")
	missing := t.TempDir() + "/absent"
	for _, c := range []struct {
		name  string
		files []string
		want  string
	}{
		{"resolved with a device link", []string{resolved, stub}, "192.168.178.1:53 [2001:db8::1]:53"},
		{"no resolved: falls back", []string{missing, writeConf(t, "nameserver 10.0.0.2\n")}, "10.0.0.2:53"},
		{"stub only", []string{missing, stub}, ""},
		{"nothing readable", []string{missing, missing}, ""},
	} {
		got, err := loadUpstreams("", c.files...)
		if err != nil || strings.Join(got, " ") != c.want {
			t.Errorf("%s: %v %v, want %q", c.name, got, err, c.want)
		}
	}
}

func TestDNSWithoutUpstreams(t *testing.T) {
	s := viewsServer()
	b, err := s.handle("udp", pack(t, "example.com.", dnsmessage.TypeA), nil, netip.MustParseAddr("10.192.0.10"))
	if err != nil {
		t.Fatal(err)
	}
	if m := unpack(t, b); m.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("public name without upstreams: %+v", m)
	}
	if m := ask(t, s, "10.192.0.10", "dev.internal."); answer(m) != "10.64.0.10" {
		t.Errorf("internal name without upstreams: %+v", m)
	}
}

func TestDNSBoundsConcurrency(t *testing.T) {
	conn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer conn.Close()
	s := newDNSServer()
	s.setViews([]proto.GatewayOrg{{ID: orgA, Slice: "127.0.0.0/8", Records: map[string]string{"dev.internal": "10.64.0.10"}}}, nil)
	for i := 0; i < cap(s.sem); i++ {
		s.sem <- struct{}{}
	}
	go s.serveUDP(conn, []string{"127.0.0.1:1"})
	c, _ := net.Dial("udp", conn.LocalAddr().String())
	defer c.Close()
	c.Write(pack(t, "dev.internal.", dnsmessage.TypeA))
	c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 1500)
	if _, err := c.Read(buf); err == nil {
		t.Fatal("answered while all slots were taken")
	}
	for i := 0; i < cap(s.sem); i++ {
		<-s.sem
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	k, err := c.Read(buf)
	if err != nil || answer(unpack(t, buf[:k])) != "10.64.0.10" {
		t.Fatalf("not answered after release: %v", err)
	}
}
