package storage

import "bytes"

// HasDefaultRoute reports whether a /proc/net/route table holds an IPv4 default route.
func HasDefaultRoute(table []byte) bool {
	for _, line := range bytes.Split(table, []byte("\n")) {
		if f := bytes.Fields(line); len(f) > 1 && string(f[1]) == "00000000" {
			return true
		}
	}
	return false
}
