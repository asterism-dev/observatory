package proto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	for _, in := range []Message{
		{Type: "register", Register: &Register{
			NodeID: "n1", Hostname: "mac", Provider: "vz", CPUs: 8, MemoryMB: 16384,
			VMs: []VMStatus{{ID: "abc", State: StateRunning, IP: "192.168.64.2", Generation: 3}},
		}},
		{Type: "desired", Desired: []DesiredVM{{
			Spec:  VMSpec{ID: "abc", Name: "dev", CPUs: 2, MemoryMB: 2048, DiskGB: 20, NICs: []NIC{{Network: "mgmt", MAC: "02:0a:c8:00:00:0a", IP: "10.200.0.10", Port: "abc-mgmt"}}},
			Power: PowerStopped, StopMode: StopHard, Generation: 3, Provisioned: true,
			Boot: &Boot{SSHPublicKeys: []string{"ssh-ed25519 AAA"}, Env: map[string]string{"A": "x\ny"}},
		}}},
	} {
		var buf bytes.Buffer
		if err := NewEncoder(&buf).Encode(in); err != nil {
			t.Fatal(err)
		}
		var out Message
		if err := json.NewDecoder(&buf).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(in, out) {
			t.Fatalf("round trip mismatch:\n in=%+v\nout=%+v", in, out)
		}
	}
}

func TestNICJSONAndNoSSHPort(t *testing.T) {
	b, err := json.Marshal(VMSpec{ID: "a", NICs: []NIC{{Network: "mgmt", MAC: "02:0a:c8:00:00:0a", IP: "10.200.0.10", Port: "a-mgmt"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"nics"`)) || bytes.Contains(b, []byte("sshPort")) {
		t.Fatalf("unexpected JSON: %s", b)
	}
}

func TestRegisterOmitsEmptyVersion(t *testing.T) {
	b, err := json.Marshal(Register{NodeID: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, "version") {
		t.Errorf("empty fields are sent: %s", s)
	}
}

func TestStorageJSON(t *testing.T) {
	for _, c := range []struct {
		in   any
		want string
	}{
		{StorageState{Disks: []StorageDisk{}, Deleted: []string{}, Exports: []StorageExport{{DiskID: "d1", Port: 10810, PSK: "k"}}},
			`{"disks":[],"deleted":[],"exports":[{"diskId":"d1","port":10810,"psk":"k"}]}`},
		{StorageStatus{Files: []StorageFile{}, Exports: []StorageExportStatus{{DiskID: "d1", Port: 10810, Error: "bind"}, {DiskID: "d2", Port: 10811}}, Version: "v"},
			`{"files":[],"exports":[{"diskId":"d1","port":10810,"error":"bind"},{"diskId":"d2","port":10811}],"version":"v"}`},
		{DiskAttachment{DiskID: "d1", MountPath: "/data", SizeGB: 1, URL: "nbd://h:1/d1", PSK: "k"},
			`{"diskId":"d1","mountPath":"/data","sizeGB":1,"url":"nbd://h:1/d1","psk":"k"}`},
	} {
		b, err := json.Marshal(c.in)
		if err != nil || string(b) != c.want {
			t.Errorf("got %s, %v\nwant %s", b, err, c.want)
		}
	}
}

func TestHostBoundSecretRedacted(t *testing.T) {
	s := HostBoundSecret{ID: "i", Name: "N", Placeholder: "obsph_x", Value: "super-secret-value", Hosts: []string{"a.test"}}
	for _, got := range []string{fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("%v", &s), fmt.Sprintf("%+v", []HostBoundSecret{s})} {
		if strings.Contains(got, "super-secret-value") {
			t.Fatalf("value printed: %s", got)
		}
	}
}

func TestIntegrationTokenRedacted(t *testing.T) {
	tok := IntegrationToken{Kind: "github", Token: "ghp_dummy0000000000000000"}
	if got := fmt.Sprintf("%v %+v %#v %v", tok, tok, tok, []IntegrationToken{tok}); strings.Contains(got, "ghp_dummy") {
		t.Fatalf("token printed: %s", got)
	}
}
