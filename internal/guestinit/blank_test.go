package guestinit

import (
	"bytes"
	"testing"
)

func TestIsBlank(t *testing.T) {
	zero := make([]byte, 2<<20)
	ext4 := bytes.Clone(zero)
	ext4[0x438], ext4[0x439] = 0x53, 0xef
	for _, tc := range []struct {
		name string
		data []byte
		want bool
	}{
		{"zero", zero, true},
		{"short", zero[:4096], false},
		{"ext4", ext4, false},
	} {
		got, err := IsBlank(bytes.NewReader(tc.data))
		if err != nil || got != tc.want {
			t.Errorf("%s: IsBlank = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}
