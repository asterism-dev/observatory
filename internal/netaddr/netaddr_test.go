package netaddr

import (
	"errors"
	"net/netip"
	"testing"
)

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func ip(s string) netip.Addr    { return netip.MustParseAddr(s) }

func TestSlicesNeverOverlap(t *testing.T) {
	var used []netip.Prefix
	for i := 0; i < 1024; i++ {
		p, err := FreeSlice(used)
		if err != nil {
			t.Fatalf("slice %d: %v", i, err)
		}
		if p.Bits() != SliceBits || !MgmtPool.Contains(p.Addr()) {
			t.Fatalf("bad slice %s", p)
		}
		for _, u := range used {
			if u.Overlaps(p) {
				t.Fatalf("%s overlaps %s", p, u)
			}
		}
		used = append(used, p)
	}
	if _, err := FreeSlice(used); !errors.Is(err, ErrExhausted) {
		t.Fatalf("want ErrExhausted, got %v", err)
	}
}

func TestFreeSliceSkipsUsed(t *testing.T) {
	got, err := FreeSlice([]netip.Prefix{pfx("10.64.0.0/20")})
	if err != nil || got != pfx("10.64.16.0/20") {
		t.Fatalf("got %s, %v", got, err)
	}
	got, _ = FreeSlice([]netip.Prefix{pfx("10.64.0.0/20"), pfx("10.64.32.0/20")})
	if got != pfx("10.64.16.0/20") {
		t.Fatalf("gap not filled: %s", got)
	}
}

func TestFreeHost(t *testing.T) {
	p := pfx("10.64.16.0/20")
	got, err := FreeHost(p, nil)
	if err != nil || got != ip("10.64.16.10") {
		t.Fatalf("got %s, %v", got, err)
	}
	var used []netip.Addr
	for a := ip("10.64.16.10"); a != ip("10.64.31.254"); a = a.Next() {
		used = append(used, a)
	}
	got, err = FreeHost(p, used)
	if err != nil || got != ip("10.64.31.254") {
		t.Fatalf("last candidate: got %s, %v", got, err)
	}
	used = append(used, ip("10.64.31.254"))
	if _, err := FreeHost(p, used); !errors.Is(err, ErrExhausted) {
		t.Fatalf("want ErrExhausted, got %v", err)
	}
}

func TestFreeAddr(t *testing.T) {
	got, err := FreeAddr(ip("10.192.0.2"), ip("10.192.0.3"), []netip.Addr{ip("10.192.0.2")})
	if err != nil || got != ip("10.192.0.3") {
		t.Fatalf("got %s, %v", got, err)
	}
}

func TestFreeSubnet(t *testing.T) {
	got, err := FreeSubnet(nil)
	if err != nil || got != pfx("10.210.0.0/24") {
		t.Fatalf("got %s, %v", got, err)
	}
	got, _ = FreeSubnet([]netip.Prefix{pfx("10.210.0.0/23")})
	if got != pfx("10.210.2.0/24") {
		t.Fatalf("got %s", got)
	}
}

func TestValidateSubnet(t *testing.T) {
	ok := map[string]string{
		"172.16.0.0/16": "172.16.0.0/16",
		"10.1.2.3/24":   "10.1.2.0/24",
	}
	for in, want := range ok {
		got, err := ValidateSubnet(in)
		if err != nil || got != pfx(want) {
			t.Errorf("%s: got %s, %v", in, got, err)
		}
	}
	for _, in := range []string{
		"10.64.1.0/24", "10.100.0.0/16", "10.192.5.0/24", "10.198.0.0/24",
		"10.199.0.0/29", "10.210.5.0/24", "192.168.64.0/25", "8.8.8.0/24",
		"10.1.0.0/15", "10.1.0.0/29", "fd00::/64", "garbage",
	} {
		if _, err := ValidateSubnet(in); !errors.Is(err, ErrInvalidSubnet) {
			t.Errorf("%s: want ErrInvalidSubnet, got %v", in, err)
		}
	}
}

func TestMAC(t *testing.T) {
	if got := MAC(ip("10.64.0.10")); got != "02:0a:40:00:00:0a" {
		t.Fatalf("got %s", got)
	}
}

func TestRouter(t *testing.T) {
	if got := Router(pfx("10.64.16.0/20")); got != ip("10.64.16.1") {
		t.Fatalf("got %s", got)
	}
}
