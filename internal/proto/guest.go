package proto

const (
	GuestSeedFile = "observatory.json"
	// ReadyMarker is what the guest writes to its console once its primary NIC has an address.
	ReadyMarker = "vmm-guest-ready"
)

// GuestSeed is the document the node puts on a guest's seed drive for the init.
type GuestSeed struct {
	Hostname   string            `json:"hostname"`
	InstanceID string            `json:"instanceId"`
	NICs       []GuestNIC        `json:"nics"` // first is primary
	Disks      []GuestDisk       `json:"disks,omitempty"`
	SSHKeys    []string          `json:"sshKeys,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	EgressCAs  map[string]string `json:"egressCAs,omitempty"`
	Context    string            `json:"context,omitempty"` // AGENTS.md
	Image      ImageConfig       `json:"image"`
}

type GuestNIC struct {
	MAC     string `json:"mac"`
	Name    string `json:"name"`
	Address string `json:"address,omitempty"` // CIDR when static
	Gateway string `json:"gateway,omitempty"`
	DHCP    bool   `json:"dhcp,omitempty"`
	Mgmt    bool   `json:"mgmt,omitempty"`
}

type GuestDisk struct {
	Device    string `json:"device"`
	MountPath string `json:"mountPath"`
}

type ImageConfig struct {
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
	Env        []string `json:"env,omitempty"`
	WorkingDir string   `json:"workingDir,omitempty"`
	StopSignal string   `json:"stopSignal,omitempty"`
}
