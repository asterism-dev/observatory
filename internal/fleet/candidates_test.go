package fleet

import (
	"fmt"
	"slices"
	"testing"
)

func TestValidCandidatesRejects(t *testing.T) {
	in := []string{"10.198.0.20:51822", "127.0.0.1:1", "fe80::1%eth0:5", "999.1.1.1:5", "1.2.3.4:0", "192.168.64.2:9", "1.2.3.4:5", "1.2.3.4:5"}
	if got := validCandidates(in); !slices.Equal(got, []string{"1.2.3.4:5"}) {
		t.Fatalf("got %v", got)
	}
	var nine []string
	for i := 1; i <= 9; i++ {
		nine = append(nine, fmt.Sprintf("203.0.113.%d:51822", i))
	}
	if got := validCandidates(nine); len(got) != 8 {
		t.Fatalf("len = %d", len(got))
	}
}
