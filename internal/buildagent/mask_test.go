package buildagent

import "testing"

func maskAll(m *Masker, chunks ...string) string {
	var out []byte
	for _, c := range chunks {
		m.Write([]byte(c))
		out = append(out, m.Take()...)
	}
	m.Close()
	return string(append(out, m.Take()...))
}

func TestMaskerAcrossChunks(t *testing.T) {
	m := NewMasker([][]byte{[]byte("hunter2-secret"), []byte("ghp_abcdef")})
	got := maskAll(m, "token: hunter2-", "secret and ghp_ab", "cdef done\n")
	if want := "token: *** and *** done\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMaskerShortSecretsIgnored(t *testing.T) {
	m := NewMasker([][]byte{[]byte("abc"), []byte(""), []byte("abcd")})
	if got, want := maskAll(m, "abc abcd ab", "cd"), "abc *** ***"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMaskerHoldsOnlyPossibleSecretTail(t *testing.T) {
	m := NewMasker([][]byte{[]byte("secret")})
	m.Write([]byte("line one\nsec"))
	if got := string(m.Take()); got != "line one\n" {
		t.Errorf("Take = %q, want the text before a possible secret", got)
	}
}
