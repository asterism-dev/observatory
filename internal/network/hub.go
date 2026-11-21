package network

import (
	"context"
	"crypto/subtle"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/authz"
	"github.com/asterism-dev/observatory/internal/netaddr"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
	"github.com/asterism-dev/observatory/internal/wgkey"
)

const (
	minTokenLen = 32
	sbPort      = 6642
)

type Config struct {
	GatewayHost  string
	GatewayToken string
	// GatewayNode is the node on the hub host whose chassis serves the gateway; zero keeps the gateway's own chassis.
	GatewayNode uuid.UUID
}

type wgKeys struct{ priv, pub string }

// Hub holds the gateway's keys and the OVN CA; private keys leave it only in the gateway state.
type Hub struct {
	store    *store.Store
	authz    *authz.Service
	cfg      Config
	underlay wgKeys
	clients  wgKeys
	ca       *CA
	sbCert   string
	sbKey    string
	status   gatewayStatus
	onChange func(org uuid.UUID)
	// gatewayNodeWarned keeps a not-yet-enrolled gateway node from logging on every state poll.
	gatewayNodeWarned sync.Once
}

// OnChange sets fn to run when an org's peer sets may have changed; call it before the hub serves.
func (h *Hub) OnChange(fn func(org uuid.UUID)) { h.onChange = fn }

// Load reads the hub's sealed network secrets, generating each on first start.
func Load(ctx context.Context, s *store.Store, z *authz.Service, c Config) (*Hub, error) {
	if c.GatewayHost == "" {
		return nil, errors.New("gateway host is required")
	}
	if len(c.GatewayToken) < minTokenLen {
		return nil, fmt.Errorf("gateway token must be at least %d characters", minTokenLen)
	}
	h := &Hub{store: s, authz: z, cfg: c}
	var err error
	if h.underlay, err = loadWG(ctx, s, "wg-underlay"); err != nil {
		return nil, err
	}
	if h.clients, err = loadWG(ctx, s, "wg-clients"); err != nil {
		return nil, err
	}
	bundle, err := s.HubSecret(ctx, "ovn-ca", NewCA)
	if err != nil {
		return nil, fmt.Errorf("hub secret ovn-ca: %w", err)
	}
	if h.ca, err = ParseCA(bundle); err != nil {
		return nil, fmt.Errorf("hub secret ovn-ca: %w", err)
	}
	sb, err := s.HubSecret(ctx, "ovn-sb", func() ([]byte, error) {
		cert, key, err := h.ca.ServerCert(netaddr.GatewayUnderlay)
		return []byte(cert + key), err
	})
	if err != nil {
		return nil, fmt.Errorf("hub secret ovn-sb: %w", err)
	}
	cert, key, err := parseBundle(sb)
	if err != nil {
		return nil, fmt.Errorf("hub secret ovn-sb: %w", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(h.ca.cert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return nil, fmt.Errorf("hub secret ovn-sb: %w", err)
	}
	if h.sbCert, h.sbKey, err = encode(cert.Raw, key); err != nil {
		return nil, err
	}
	return h, nil
}

func loadWG(ctx context.Context, s *store.Store, name string) (wgKeys, error) {
	priv, err := s.HubSecret(ctx, name, func() ([]byte, error) {
		priv, _, err := wgkey.Generate()
		return []byte(priv), err
	})
	if err != nil {
		return wgKeys{}, fmt.Errorf("hub secret %s: %w", name, err)
	}
	pub, err := wgkey.Public(string(priv))
	if err != nil {
		return wgKeys{}, fmt.Errorf("hub secret %s: %w", name, err)
	}
	return wgKeys{string(priv), pub}, nil
}

var ErrUnderlayRequest = errors.New("invalid underlay request")

// Underlay signs the node's chassis CSR and enrolls its WireGuard key; nodes send a fresh CSR on every start.
func (h *Hub) Underlay(ctx context.Context, n store.Node, chassisKey, csrPEM string) (*proto.Underlay, error) {
	if !wgkey.Valid(chassisKey) {
		return nil, fmt.Errorf("%w: chassis key is not a WireGuard public key", ErrUnderlayRequest)
	}
	cert, err := h.ca.SignChassis(n.ID, csrPEM)
	if err != nil {
		return nil, err
	}
	ip, err := h.store.EnrollChassis(ctx, n.ID, chassisKey)
	if err != nil {
		return nil, err
	}
	h.changed(n.OrgID) // the key may be new, and a re-enrolled node is no longer quarantined
	endpoint := h.endpoint(netaddr.UnderlayPort)
	if n.ID == h.cfg.GatewayNode {
		endpoint = net.JoinHostPort("127.0.0.1", strconv.Itoa(netaddr.UnderlayPort)) // same host: skip the public address
	}
	return &proto.Underlay{
		Address:  netip.PrefixFrom(ip, netaddr.UnderlayPool.Bits()).String(),
		HubKey:   h.underlay.pub,
		Endpoint: endpoint,
		SBRemote: "ssl:" + net.JoinHostPort(netaddr.GatewayUnderlay.String(), strconv.Itoa(sbPort)),
		CACert:   h.ca.CertPEM(),
		Cert:     cert,
	}, nil
}

func (h *Hub) ClientsPublicKey() string { return h.clients.pub }

func (h *Hub) ClientsEndpoint() string { return h.endpoint(netaddr.ClientsPort) }

func (h *Hub) TokenOK(token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(h.cfg.GatewayToken)) == 1
}

func (h *Hub) endpoint(port int) string {
	return net.JoinHostPort(h.cfg.GatewayHost, strconv.Itoa(port))
}
