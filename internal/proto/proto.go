// Package proto defines the messages exchanged between hub and nodes.
package proto

import (
	"encoding/json"
	"io"
	"net"
	"strconv"
	"time"
)

type VMState string

const (
	StatePending  VMState = "pending"
	StateStarting VMState = "starting"
	StateRunning  VMState = "running"
	StateStopped  VMState = "stopped"
	StateError    VMState = "error"
	StateUnknown  VMState = "unknown"
	// StatePaused keeps the VM's process and RAM with its vCPUs stopped; StateHibernated keeps its memory on the node's disk, StateSuspended too, by auto-suspend.
	StatePaused     VMState = "paused"
	StateHibernated VMState = "hibernated"
	StateSuspended  VMState = "suspended"
)

type VMSpec struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	CPUs        int              `json:"cpus"`
	MemoryMB    int              `json:"memoryMB"`
	DiskGB      int              `json:"diskGB"`
	NICs        []NIC            `json:"nics"`
	Attachments []DiskAttachment `json:"attachments,omitempty"`
	Isolated    bool             `json:"isolated,omitempty"`
	Image       *ImageRef        `json:"image,omitempty"`      // nil: the platform image
	NoTemplate  bool             `json:"noTemplate,omitempty"` // always boot cold: the VM's service reads its env once at start
}

// ImageRef is an image the node pulls by digest; an empty PullToken pulls anonymously.
type ImageRef struct {
	Repo      string `json:"repo"` // <registry host>/i/<image id>
	Digest    string `json:"digest"`
	PullToken string `json:"pullToken,omitempty"` // node memory only, like Boot
	Gone      bool   `json:"gone,omitempty"`      // the hub no longer has the image
}

type Boot struct {
	SSHPublicKeys []string          `json:"sshPublicKeys"`
	Env           map[string]string `json:"env"`
	EgressCAs     map[string]string `json:"egressCAs,omitempty"`
}

type DiskAttachment struct {
	DiskID    string `json:"diskId"`
	MountPath string `json:"mountPath"`
	SizeGB    int    `json:"sizeGB"`        // size the guest should see; a change restarts the VM
	URL       string `json:"url,omitempty"` // set by the hub in DesiredVM only
	PSK       string `json:"psk,omitempty"` // set by the hub in DesiredVM only
}

// NIC is one virtual network interface; Port is the OVN logical switch port name.
type NIC struct {
	Network string `json:"network"`
	MAC     string `json:"mac"`
	IP      string `json:"ip"`
	Port    string `json:"port"`
}

// Disk is a block device the node attached to the VM; Name is "root" or "seed".
type Disk struct {
	Name           string `json:"name"`
	File           string `json:"file"`
	Device         string `json:"device"`
	ReadOnly       bool   `json:"readOnly"`
	SizeBytes      int64  `json:"sizeBytes"`
	AllocatedBytes int64  `json:"allocatedBytes"`
	Error          string `json:"error,omitempty"`
}

type VMStatus struct {
	ID    string  `json:"id"`
	State VMState `json:"state"`
	IP    string  `json:"ip,omitempty"`
	Error string  `json:"error,omitempty"`
	Disks []Disk  `json:"disks,omitempty"`
	// Generation echoes the DesiredVM generation this status reflects.
	Generation  uint64         `json:"generation,omitempty"`
	Snapshots   []SnapshotInfo `json:"snapshots,omitempty"`
	RestoredSeq uint64         `json:"restoredSeq,omitempty"`
	// Image is the reference and digest (ref@sha256:…) of the image the VM's root disk was built from.
	Image string `json:"image,omitempty"`
}

// SnapshotRef is a snapshot the hub wants the node to keep; Complete lets the node resume the VM it paused for it.
type SnapshotRef struct {
	ID       string `json:"id"`
	Memory   bool   `json:"memory"`
	Complete bool   `json:"complete,omitempty"`
}

// Restore asks the node to roll a VM back to one of its snapshots; Ready means the attached disks are restored too.
type Restore struct {
	SnapshotID string `json:"snapshotId"`
	Seq        uint64 `json:"seq"`
	Memory     bool   `json:"memory"`
	Ready      bool   `json:"ready,omitempty"`
}

// Fork builds a new VM from a snapshot of another VM on the same node.
type Fork struct {
	SourceVMID string `json:"sourceVmId"`
	SnapshotID string `json:"snapshotId"`
	Memory     bool   `json:"memory"`
}

type SnapshotInfo struct {
	ID        string `json:"id"`
	Memory    bool   `json:"memory"`
	SizeBytes int64  `json:"sizeBytes"`
	Error     string `json:"error,omitempty"`
}

type Power string

const (
	PowerRunning    Power = "running"
	PowerStopped    Power = "stopped"
	PowerPaused     Power = "paused"
	PowerHibernated Power = "hibernated"
)

type StopMode string

const (
	StopGraceful StopMode = "graceful"
	StopHard     StopMode = "hard"
)

const ErrVMDirMissing = "VM directory missing"

type DesiredVM struct {
	Spec               VMSpec        `json:"spec"`
	Power              Power         `json:"power"`
	StopMode           StopMode      `json:"stopMode,omitempty"`
	Generation         uint64        `json:"generation"`
	Provisioned        bool          `json:"provisioned"`
	Boot               *Boot         `json:"boot,omitempty"`
	Snapshots          []SnapshotRef `json:"snapshots,omitempty"`
	Restore            *Restore      `json:"restore,omitempty"`
	Fork               *Fork         `json:"fork,omitempty"`
	AutoSuspendMinutes int           `json:"autoSuspendMinutes,omitempty"`
	WakeSeq            uint64        `json:"wakeSeq,omitempty"`
	// Egress goes only to the VM's node and lives only in its memory.
	Egress *Egress `json:"egress,omitempty"`
	// Integrations follow the Egress rules: only to the VM's node, memory only.
	Integrations []IntegrationToken `json:"integrations,omitempty"`
	// IntegrationsUnresolved means the hub failed to read the VM's integrations, so Integrations says nothing about them.
	IntegrationsUnresolved bool `json:"integrationsUnresolved,omitempty"`
}

type Register struct {
	NodeID   string     `json:"nodeId"`
	Hostname string     `json:"hostname"`
	Provider string     `json:"provider"`
	CPUs     int        `json:"cpus"`
	MemoryMB int        `json:"memoryMB"`
	VMs      []VMStatus `json:"vms,omitempty"`
	// Config is the node's config file with the hub credential removed; shown in the GUI only.
	Config     json.RawMessage `json:"config,omitempty"`
	Machine    *Machine        `json:"machine,omitempty"`
	ChassisKey string          `json:"chassisKey,omitempty"`
	ChassisCSR string          `json:"chassisCSR,omitempty"`
	DisksInUse []string        `json:"disksInUse,omitempty"`
	AppliedSeq uint64          `json:"appliedSeq,omitempty"` // seq of the last desired list the node fully reconciled
	Version    string          `json:"version,omitempty"`
	Image      *ImageStatus    `json:"image,omitempty"`
	Hypervisor *Hypervisor     `json:"hypervisor,omitempty"`
}

type Hypervisor struct {
	Type    string `json:"type"`
	Version string `json:"version"`
}

// UpdateReport (type "update") tells the hub an update attempt failed; the node restarts on success.
type UpdateReport struct {
	Error string `json:"error,omitempty"`
}

const (
	ImagePulling = "pulling"
	ImageReady   = "ready"
	ImageFailed  = "failed"
)

// ImageStatus is the node's guest image: Version is the reference it pulls, and ref@digest once ready.
type ImageStatus struct {
	Version   string      `json:"version"`
	State     string      `json:"state"`
	Config    ImageConfig `json:"-"` // node-local: goes into seeds, not to the hub
	Error     string      `json:"error,omitempty"`
	StartedAt *time.Time  `json:"startedAt,omitempty"`
	BuiltAt   *time.Time  `json:"builtAt,omitempty"`
	RetryAt   *time.Time  `json:"retryAt,omitempty"`
}

// Underlay tells a node how to join the WireGuard underlay and reach the OVN southbound DB over SSL; Cert is the node's signed chassis cert.
type Underlay struct {
	Address  string `json:"address"`
	HubKey   string `json:"hubKey"`
	Endpoint string `json:"endpoint"`
	SBRemote string `json:"sbRemote"`
	CACert   string `json:"caCert"`
	Cert     string `json:"cert"`
}

type Machine struct {
	OS        string `json:"os"`
	OSVersion string `json:"osVersion,omitempty"`
	Arch      string `json:"arch"`
	CPUModel  string `json:"cpuModel,omitempty"`
	CPUs      int    `json:"cpus"`
	MemoryMB  int    `json:"memoryMB,omitempty"`
}

// Message types: "register" (node→hub), "desired"/"underlay" (hub→node), "status", "disks", "metrics" and "image" (node→hub).
// VMMetrics is one sample of a running VM; MemTotalMB 0 means the VM has no balloon statistics.
type VMMetrics struct {
	ID         string  `json:"id"`
	CPUPercent float64 `json:"cpuPercent"`
	MemTotalMB int     `json:"memTotalMB"`
	MemUsedMB  int     `json:"memUsedMB"`
	DiskUsedMB int     `json:"diskUsedMB"` // allocated host blocks of the root disk image
}

type Message struct {
	Type     string    `json:"type"`
	Register *Register `json:"register,omitempty"`
	Status   *VMStatus `json:"status,omitempty"`
	Underlay *Underlay `json:"underlay,omitempty"`
	// Desired (type "desired") is the full VM list for the node; nil means no VMs.
	Desired    []DesiredVM   `json:"desired,omitempty"`
	DisksInUse []string      `json:"disksInUse,omitempty"` // type "disks": every disk ID a local VM still uses
	Seq        uint64        `json:"seq,omitempty"`        // type "desired": increases with every list sent to the node
	AppliedSeq uint64        `json:"appliedSeq,omitempty"` // type "disks": seq of the last desired list the node fully reconciled
	Metrics    []VMMetrics   `json:"metrics,omitempty"`    // type "metrics": one sample per running VM
	Image      *ImageStatus  `json:"image,omitempty"`      // type "image": the guest image changed state
	Update     *UpdateReport `json:"update,omitempty"`     // type "update": an update attempt failed
}

func NewEncoder(w io.Writer) *json.Encoder { return json.NewEncoder(w) }

func StorageFileName(diskID string) string { return diskID + ".img" }

type StorageFile struct {
	Name           string `json:"name"`
	SizeBytes      int64  `json:"sizeBytes"`
	AllocatedBytes int64  `json:"allocatedBytes"`
}

type StorageDisk struct {
	ID        string `json:"id"`
	SizeBytes int64  `json:"sizeBytes"`
	CopyFrom  string `json:"copyFrom,omitempty"`
	// Create is false once the hub has seen the file, so a vanished file is never replaced by a blank one.
	Create       bool   `json:"create,omitempty"`
	RestoreFrom  string `json:"restoreFrom,omitempty"`
	RestoreToken string `json:"restoreToken,omitempty"`
}

type StorageExport struct {
	DiskID string `json:"diskId"`
	Port   int    `json:"port"`
	PSK    string `json:"psk"`
}

// StorageState is what the hub wants from the storage server.
type StorageState struct {
	Disks   []StorageDisk   `json:"disks"`
	Deleted []string        `json:"deleted"` // disk IDs whose files must be removed
	Exports []StorageExport `json:"exports"`
}

type StorageExportStatus struct {
	DiskID string `json:"diskId"`
	Port   int    `json:"port"`
	Error  string `json:"error,omitempty"`
}

// StorageStatus is what the storage server reports to the hub.
type StorageStatus struct {
	Files    []StorageFile         `json:"files"`
	Exports  []StorageExportStatus `json:"exports"`
	Version  string                `json:"version"`
	Restored []string              `json:"restored,omitempty"`
}

// ValidEndpoint reports whether s is host:port with a non-empty host and a port in 1-65535.
func ValidEndpoint(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

type EgressCA struct {
	ID      string `json:"id"`
	CertPEM string `json:"certPem"`
	KeyPEM  string `json:"keyPem"`
}

func (EgressCA) String() string   { return "EgressCA{redacted}" }
func (EgressCA) GoString() string { return "EgressCA{redacted}" }

type HostBoundSecret struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Placeholder string   `json:"placeholder"`
	Value       string   `json:"value"`
	Hosts       []string `json:"hosts"`
	Headers     []string `json:"headers,omitempty"`
}

func (HostBoundSecret) String() string   { return "HostBoundSecret{redacted}" }
func (HostBoundSecret) GoString() string { return "HostBoundSecret{redacted}" }

type IntegrationToken struct {
	Kind      string     `json:"kind"`
	Token     string     `json:"token"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

func (IntegrationToken) String() string   { return "IntegrationToken{redacted}" }
func (IntegrationToken) GoString() string { return "IntegrationToken{redacted}" }

type Egress struct {
	CAs     []EgressCA        `json:"cas"`
	Secrets []HostBoundSecret `json:"secrets"`
}

func (Egress) String() string   { return "Egress{redacted}" }
func (Egress) GoString() string { return "Egress{redacted}" }
