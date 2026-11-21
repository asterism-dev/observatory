package main

import (
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

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

func (d *dnsServer) handle(network string, req []byte, upstream string, src netip.Addr) ([]byte, error) {
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
	// Forwarding for others would loop when the upstream (Docker's DNS) asks the gateway back: hub host resolving via a device.
	if v == nil {
		return reply(m, dnsmessage.RCodeRefused, nil, false)
	}
	if name != "internal." && !strings.HasSuffix(name, ".internal.") {
		return forward(network, req, upstream)
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

func reply(req dnsmessage.Message, rcode dnsmessage.RCode, answers []dnsmessage.Resource, authoritative bool) ([]byte, error) {
	return (&dnsmessage.Message{
		Header: dnsmessage.Header{ID: req.ID, Response: true, Authoritative: authoritative,
			RecursionDesired: req.RecursionDesired, RCode: rcode},
		Questions: req.Questions,
		Answers:   answers,
	}).Pack()
}

func forward(network string, req []byte, upstream string) ([]byte, error) {
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

func (d *dnsServer) serveUDP(conn net.PacketConn, upstream string) {
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
			if resp, err := d.handle("udp", req, upstream, sourceAddr(from)); err == nil {
				conn.WriteTo(resp, from)
			}
		}()
	}
}

func (d *dnsServer) serveTCP(l net.Listener, upstream string) {
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
				resp, err := d.handle("tcp", req, upstream, src)
				if err != nil || writeFramed(c, resp) != nil {
					return
				}
			}
		}()
	}
}
