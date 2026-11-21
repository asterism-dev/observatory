package uuidx

import "testing"

func TestCanonical(t *testing.T) {
	for s, ok := range map[string]bool{
		"7d444840-9dc0-11d1-b245-5ffdce74fad2":          true,
		"7D444840-9DC0-11D1-B245-5FFDCE74FAD2":          false,
		"{7d444840-9dc0-11d1-b245-5ffdce74fad2}":        false,
		"urn:uuid:7d444840-9dc0-11d1-b245-5ffdce74fad2": false,
		"": false,
	} {
		if _, err := Canonical(s); (err == nil) != ok {
			t.Errorf("Canonical(%q) err = %v, want ok = %v", s, err, ok)
		}
	}
}
