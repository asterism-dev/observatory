package seal

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func randKey(t *testing.T) Key {
	t.Helper()
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	k := randKey(t)
	nonce, ct, err := Seal(k, []byte("hunter2"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(k, nonce, ct, []byte("aad"))
	if err != nil || !bytes.Equal(got, []byte("hunter2")) {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestWrongAAD(t *testing.T) {
	k := randKey(t)
	nonce, ct, _ := Seal(k, []byte("x"), []byte("a"))
	if _, err := Open(k, nonce, ct, []byte("b")); err == nil {
		t.Fatal("wrong aad accepted")
	}
}

func TestWrongKey(t *testing.T) {
	nonce, ct, _ := Seal(randKey(t), []byte("x"), nil)
	if _, err := Open(randKey(t), nonce, ct, nil); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestNonceUnique(t *testing.T) {
	k := randKey(t)
	n1, c1, _ := Seal(k, []byte("x"), nil)
	n2, c2, _ := Seal(k, []byte("x"), nil)
	if bytes.Equal(n1, n2) || bytes.Equal(c1, c2) {
		t.Fatal("nonce or ciphertext reused")
	}
}

func TestParseKey(t *testing.T) {
	enc := func(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }
	if _, err := ParseKey(enc(32)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{enc(31), enc(33), "not base64!", ""} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestID(t *testing.T) {
	if id := randKey(t).ID(); len(id) != 16 {
		t.Fatalf("id %q", id)
	}
}
