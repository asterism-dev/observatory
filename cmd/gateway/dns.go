package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
)

// view is one org's records (lower-case FQDN with trailing dot → IP) and the names above them.
type view struct {
	records map[string]string
	parents map[string]bool
}

type orgSlice struct {
	prefix netip.Prefix
	view   *view
}

type dnsServer struct {
	mu      sync.RWMutex
	devices map[netip.Addr]*view
	slices  []orgSlice
	sem     chan struct{}
}

func newDNSServer() *dnsServer { return &dnsServer{sem: make(chan struct{}, 256)} }

func (d *dnsServer) setViews(orgs []proto.GatewayOrg, devices []proto.GatewayDevice) {
	byOrg := map[string]*view{}
	var slices []orgSlice
	for _, o := range orgs {
		v := &view{records: map[string]string{}, parents: map[string]bool{}}
		for name, ip := range o.Records {
			name = strings.ToLower(strings.TrimSuffix(name, ".")) + "."
			v.records[name] = ip
			for i := strings.Index(name, "."); i >= 0 && i < len(name)-1; i = strings.Index(name, ".") {
				name = name[i+1:]
				v.parents[name] = true
			}
		}
		byOrg[o.ID] = v
		if p, err := netip.ParsePrefix(o.Slice); err == nil {
			slices = append(slices, orgSlice{p.Masked(), v})
		}
	}
	devs := map[netip.Addr]*view{}
	for _, dev := range devices {
		if a, err := netip.ParseAddr(dev.IP); err == nil && byOrg[dev.OrgID] != nil {
			devs[a] = byOrg[dev.OrgID]
		}
	}
	d.mu.Lock()
	d.devices, d.slices = devs, slices
	d.mu.Unlock()
}

// viewFor picks the org view by source: a device address, else the org slice holding a VM's management address.
func (d *dnsServer) viewFor(src netip.Addr) *view {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if v := d.devices[src]; v != nil {
		return v
	}
	for _, s := range d.slices {
		if s.prefix.Contains(src) {
			return s.view
		}
	}
	return nil
}

func (d *dnsServer) handle(network string, req []byte, upstreams []string, src netip.Addr) ([]byte, error) {
	var m dnsmessage.Message
	if err := m.Unpack(req); err != nil {
		return nil, err
	}
	if len(m.Questions) != 1 {
		return reply(m, dnsmessage.RCodeFormatError, nil, false)
	}
	q := m.Questions[0]
	name := strings.ToLower(q.Name.String())
	v := d.viewFor(src)
	if name != "internal." && !strings.HasSuffix(name, ".internal.") {
		// A local source is an upstream set to the gateway or Docker's DNS asking back: a loop.
		if v == nil && gatewayLocal(src) {
			return reply(m, dnsmessage.RCodeRefused, nil, false)
		}
		if len(upstreams) == 0 {
			return reply(m, dnsmessage.RCodeServerFailure, nil, false)
		}
		return forward(network, req, upstreams)
	}
	if v == nil {
		return reply(m, dnsmessage.RCodeRefused, nil, false)
	}
	ip, ok := v.records[name]
	if !ok && !v.parents[name] {
		return reply(m, dnsmessage.RCodeNameError, nil, true)
	}
	var answers []dnsmessage.Resource
	if addr := net.ParseIP(ip).To4(); q.Type == dnsmessage.TypeA && addr != nil {
		answers = append(answers, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 5},
			Body:   &dnsmessage.AResource{A: [4]byte(addr)},
		})
	}
	return reply(m, dnsmessage.RCodeSuccess, answers, true)
}

var gw0Addr = netip.MustParseAddr(gw0IP)

// gatewayLocal reports a source in the gateway's own namespace; a local sender to 10.192.0.1 gets that address as its source.
func gatewayLocal(src netip.Addr) bool {
	src = src.Unmap()
	return src.IsLoopback() || src == netaddr.GatewayDNS || src == netaddr.GatewayUnderlay || src == gw0Addr
}

func reply(req dnsmessage.Message, rcode dnsmessage.RCode, answers []dnsmessage.Resource, authoritative bool) ([]byte, error) {
	return (&dnsmessage.Message{
		Header: dnsmessage.Header{ID: req.ID, Response: true, Authoritative: authoritative,
			RecursionDesired: req.RecursionDesired, RCode: rcode},
		Questions: req.Questions,
		Answers:   answers,
	}).Pack()
}

// loadUpstreams parses the OBSERVATORY_GATEWAY_DNS override, else takes the nameservers of the first readable host resolv.conf.
func loadUpstreams(override string, hostFiles ...string) ([]string, error) {
	if strings.TrimSpace(override) != "" {
		return parseUpstreams(override)
	}
	for _, f := range hostFiles {
		if b, err := os.ReadFile(f); err == nil {
			return hostUpstreams(b), nil
		}
	}
	return nil, nil
}

// hostUpstreams drops the stub, link-local and the gateway's own pools: a device on the hub host lists 10.192.0.1 as its link's DNS, which would loop.
func hostUpstreams(conf []byte) []string {
	var out []string
	for _, line := range strings.Split(string(conf), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		a, err := netip.ParseAddr(f[1])
		if err != nil {
			continue
		}
		a = a.Unmap()
		if a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() || gatewayLocal(a) ||
			netaddr.DevicePool.Contains(a) || netaddr.UnderlayPool.Contains(a) {
			continue
		}
		out = append(out, netip.AddrPortFrom(a.WithZone(""), 53).String())
	}
	return out
}

func parseUpstreams(s string) ([]string, error) {
	var out []string
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if ap, err := netip.ParseAddrPort(f); err == nil {
			out = append(out, ap.String())
		} else if a, err := netip.ParseAddr(f); err == nil {
			out = append(out, netip.AddrPortFrom(a, 53).String())
		} else {
			return nil, fmt.Errorf("OBSERVATORY_GATEWAY_DNS: %q is not an IP address or IP:port", f)
		}
	}
	return out, nil
}

// forward asks each upstream in turn until one answers.
func forward(network string, req []byte, upstreams []string) (resp []byte, err error) {
	for _, u := range upstreams {
		if resp, err = exchange(network, req, u); err == nil {
			return resp, nil
		}
	}
	return nil, err
}

func exchange(network string, req []byte, upstream string) ([]byte, error) {
	c, err := net.DialTimeout(network, upstream, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if network == "tcp" {
		return exchangeTCP(c, req)
	}
	if _, err := c.Write(req); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	n, err := c.Read(buf)
	return buf[:n], err
}

func exchangeTCP(c net.Conn, req []byte) ([]byte, error) {
	if err := writeFramed(c, req); err != nil {
		return nil, err
	}
	return readFramed(c)
}

func writeFramed(w io.Writer, b []byte) error {
	_, err := w.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(b))), b...))
	return err
}

func readFramed(r io.Reader) ([]byte, error) {
	var l [2]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint16(l[:]))
	_, err := io.ReadFull(r, b)
	return b, err
}

func sourceAddr(a net.Addr) netip.Addr {
	ap, _ := netip.ParseAddrPort(a.String())
	return ap.Addr().Unmap()
}

func (d *dnsServer) serveUDP(conn net.PacketConn, upstreams []string) {
	buf := make([]byte, 65535)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		req := append([]byte(nil), buf[:n]...)
		d.sem <- struct{}{}
		go func() {
			defer func() { <-d.sem }()
			if resp, err := d.handle("udp", req, upstreams, sourceAddr(from)); err == nil {
				conn.WriteTo(resp, from)
			}
		}()
	}
}

func (d *dnsServer) serveTCP(l net.Listener, upstreams []string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		d.sem <- struct{}{}
		go func() {
			defer func() { <-d.sem }()
			defer c.Close()
			src := sourceAddr(c.RemoteAddr())
			for {
				c.SetDeadline(time.Now().Add(10 * time.Second))
				req, err := readFramed(c)
				if err != nil {
					return
				}
				resp, err := d.handle("tcp", req, upstreams, src)
				if err != nil || writeFramed(c, resp) != nil {
					return
				}
			}
		}()
	}
}
