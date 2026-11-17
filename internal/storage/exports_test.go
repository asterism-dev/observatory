package storage

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/asterism-dev/observatory/internal/proto"
)

type fakeProc struct {
	args    []string
	stopped bool
	done    chan error
}

type fakeSpawner struct{ procs []*fakeProc }

func (f *fakeSpawner) spawn(args []string) (func(), <-chan error, error) {
	p := &fakeProc{args: args, done: make(chan error, 1)}
	f.procs = append(f.procs, p)
	return func() { p.stopped = true }, p.done, nil
}

func newExports(t *testing.T, ids ...string) (*Exports, *fakeSpawner) {
	t.Helper()
	d := Dir{t.TempDir()}
	for _, id := range ids {
		if err := os.WriteFile(d.File(id), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeSpawner{}
	return &Exports{Addr: "10.198.0.1", KeyDir: filepath.Join(t.TempDir(), "keys"), Dir: d, Spawn: f.spawn}, f
}

var (
	psk1 = strings.Repeat("a", 64)
	psk2 = strings.Repeat("b", 64)
)

func TestExportsApply(t *testing.T) {
	ex, f := newExports(t, "d1", "d2")
	want := []proto.StorageExport{{DiskID: "d1", Port: 10810, PSK: psk1}, {DiskID: "d2", Port: 10811, PSK: psk2}}
	st := ex.Apply(want)
	if len(f.procs) != 2 || len(st) != 2 || st[0].Error != "" || st[1].Error != "" {
		t.Fatalf("procs %d status %+v", len(f.procs), st)
	}
	key1 := filepath.Join(ex.KeyDir, "d1.psk")
	if !reflect.DeepEqual(f.procs[0].args, NBDKitArgs("10.198.0.1", 10810, key1, ex.Dir.File("d1"))) {
		t.Fatalf("args %v", f.procs[0].args)
	}
	fi, err := os.Stat(key1)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v %v", fi, err)
	}
	if b, _ := os.ReadFile(key1); string(b) != "d1:"+psk1+"\n" {
		t.Fatalf("key file content %q", b)
	}
	if di, _ := os.Stat(ex.KeyDir); di.Mode().Perm() != 0o700 {
		t.Fatalf("key dir mode %v", di.Mode())
	}

	ex.Apply(want)
	if len(f.procs) != 2 {
		t.Fatalf("unchanged state respawned: %d", len(f.procs))
	}

	ex.Apply(want[:1])
	if !f.procs[1].stopped || f.procs[0].stopped {
		t.Fatal("removed export not stopped (or kept one stopped)")
	}
	if _, err := os.Stat(filepath.Join(ex.KeyDir, "d2.psk")); !os.IsNotExist(err) {
		t.Fatalf("d2 key file kept: %v", err)
	}

	ex.Apply([]proto.StorageExport{{DiskID: "d1", Port: 10810, PSK: psk2}})
	if !f.procs[0].stopped || len(f.procs) != 3 {
		t.Fatalf("changed key: stopped %v procs %d", f.procs[0].stopped, len(f.procs))
	}
	if b, _ := os.ReadFile(key1); string(b) != "d1:"+psk2+"\n" {
		t.Fatalf("key file content %q", b)
	}

	f.procs[2].done <- nil
	st = ex.Apply([]proto.StorageExport{{DiskID: "d1", Port: 10810, PSK: psk2}})
	if len(f.procs) != 4 || st[0].Error == "" {
		t.Fatalf("exited process: procs %d status %+v", len(f.procs), st)
	}
	st = ex.Apply([]proto.StorageExport{{DiskID: "d1", Port: 10810, PSK: psk2}})
	if len(f.procs) != 4 || st[0].Error != "" {
		t.Fatalf("after respawn: procs %d status %+v", len(f.procs), st)
	}
}

func TestExportsSkipMissingFile(t *testing.T) {
	ex, f := newExports(t)
	st := ex.Apply([]proto.StorageExport{{DiskID: "d1", Port: 10810, PSK: psk1}})
	if len(f.procs) != 0 || len(st) != 1 || st[0].Error != "file missing" || st[0].Port != 10810 {
		t.Fatalf("procs %d status %+v", len(f.procs), st)
	}
	if _, err := os.Stat(filepath.Join(ex.KeyDir, "d1.psk")); !os.IsNotExist(err) {
		t.Fatalf("key written for missing file: %v", err)
	}
}

func TestExportsRejectsBadInput(t *testing.T) {
	ex, f := newExports(t, "d1")
	st := ex.Apply([]proto.StorageExport{{DiskID: "d1", Port: 10810, PSK: "x\nevil:" + psk1}, {DiskID: "../d1", Port: 10811, PSK: psk1}})
	if len(f.procs) != 0 || st[0].Error == "" || st[1].Error == "" || strings.Contains(st[0].Error, psk1) {
		t.Fatalf("procs %d status %+v", len(f.procs), st)
	}
}
