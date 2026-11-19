package proto

import "time"

// GatewayState is the desired state the gateway applies; it carries private keys and must never be logged.
type GatewayState struct {
	UnderlayKey string           `json:"underlayKey"`
	ClientsKey  string           `json:"clientsKey"`
	SB          GatewaySB        `json:"sb"`
	GuestMTU    int              `json:"guestMTU"`
	Chassis     []GatewayChassis `json:"chassis"`
	Devices     []GatewayDevice  `json:"devices"`
	Orgs        []GatewayOrg     `json:"orgs"`
}

// GatewaySB is the southbound DB's CA and server certificate with its key.
type GatewaySB struct {
	CACert string `json:"caCert"`
	Cert   string `json:"cert"`
	Key    string `json:"key"`
}

type GatewayChassis struct {
	NodeID      string `json:"nodeId"`
	OrgID       string `json:"orgId"`
	PublicKey   string `json:"publicKey"`
	IP          string `json:"ip"`
	Quarantined bool   `json:"quarantined,omitempty"`
}

type GatewayDevice struct {
	PublicKey string `json:"publicKey"`
	IP        string `json:"ip"`
	OrgID     string `json:"orgId"`
}

// GatewayOrg is one org's management switch, networks and DNS records (name without trailing dot → IP).
type GatewayOrg struct {
	ID       string            `json:"id"`
	Slice    string            `json:"slice"`
	Ports    []GatewayPort     `json:"ports"`
	Networks []GatewayNetwork  `json:"networks"`
	Records  map[string]string `json:"records"`
}

// GatewayPort is a logical switch port bound to Chassis; AllowFrom lists the device IPs admitted to a management port.
type GatewayPort struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac"`
	IP        string   `json:"ip"`
	Chassis   string   `json:"chassis"`
	AllowFrom []string `json:"allowFrom,omitempty"`
}

type GatewayNetwork struct {
	ID     string        `json:"id"`
	Subnet string        `json:"subnet"`
	Ports  []GatewayPort `json:"ports"`
}

// GatewayStatus is what the gateway observes: handshakes by peer public key, chassis registered in the SB and ports whose foreign binding it cleared.
type GatewayStatus struct {
	Handshakes map[string]time.Time `json:"handshakes"`
	Chassis    []string             `json:"chassis"`
	Rebound    []string             `json:"rebound"`
	// Quarantined lists nodes caught holding a port bound to another node.
	Quarantined []string `json:"quarantined"`
	Version     string   `json:"version"`
}
