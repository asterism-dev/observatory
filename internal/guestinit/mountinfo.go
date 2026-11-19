package guestinit

import (
	"bufio"
	"io"
	"slices"
	"strconv"
	"strings"
)

// mountsBelow lists the mount points at or below prefix from /proc/self/mountinfo, deepest first.
func mountsBelow(mountinfo io.Reader, prefix string) []string {
	var out []string
	sc := bufio.NewScanner(mountinfo)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		p := unescapeMountPath(fields[4])
		if p == prefix || strings.HasPrefix(p, strings.TrimSuffix(prefix, "/")+"/") {
			out = append(out, p)
		}
	}
	// A child is always longer than its parent.
	slices.SortStableFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

func hasMount(mountinfo io.Reader, path string) bool {
	return slices.Contains(mountsBelow(mountinfo, path), path)
}

// unescapeMountPath decodes the octal escapes (\040 for a space) the kernel writes into mountinfo.
func unescapeMountPath(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
