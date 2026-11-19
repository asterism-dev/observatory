package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/version"
)

func main() {
	home, _ := os.UserHomeDir()
	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		if err := runEnroll(os.Args[2:], home); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "egress" {
		os.Exit(egressMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	configPath := flag.String("config", filepath.Join(home, ".observatory-node", "node.json"), "path to node config")
	flag.Parse()

	cfg, err := node.LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	provider, err := newProvider(cfg)
	if err != nil {
		log.Fatal(err)
	}
	egressHost, err := newEgress(cfg)
	if err != nil {
		log.Fatal(err)
	}
	for _, d := range []string{cfg.Paths.VMs, cfg.Paths.Images} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatal(err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	public, err := cfg.Public()
	if err != nil {
		log.Fatal(err)
	}
	chassisPriv, chassisPub, err := node.LoadOrCreateChassisKey(filepath.Join(cfg.Paths.VMs, "chassis.key"))
	if err != nil {
		log.Fatal(err)
	}
	ovnKeyFile := filepath.Join(cfg.Paths.VMs, "ovn-key.pem")
	chassisCSR, err := node.LoadOrCreateOVNKey(ovnKeyFile, cfg.NodeID)
	if err != nil {
		log.Fatal(err)
	}
	a := &node.Agent{
		ChassisKey: chassisPub, ChassisPrivateKey: chassisPriv, ChassisCSR: chassisCSR, OVNKeyFile: ovnKeyFile,
		HubURL: cfg.Hub.URL, Credential: cfg.Hub.Credential, NodeID: cfg.NodeID, VMsDir: cfg.Paths.VMs,
		CPUs: cfg.Capacity.CPUs, MemoryMB: cfg.Capacity.MemoryMB, Provider: provider,
		Config: public, Machine: node.MachineInfo(),
		Version: version.Version, Egress: egressHost,
	}
	log.Printf("node %s (%s): provider %s, %d CPUs, %d MB", cfg.NodeID, version.Version, provider.Name(), cfg.Capacity.CPUs, cfg.Capacity.MemoryMB)
	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

// runEnroll redeems a one-time token at the hub and writes node.json; data lives under dataDir unless the config already says otherwise.
func runEnroll(args []string, home string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	var o node.InitOptions
	fs.StringVar(&o.Path, "config", filepath.Join(home, ".observatory-node", "node.json"), "path to node config")
	fs.StringVar(&o.HubURL, "hub", "", "hub address, http://host:port")
	tokenStdin := fs.Bool("token-stdin", false, "read the enrollment token from stdin")
	fs.IntVar(&o.MemoryMB, "memory-mb", 0, "memory offered to VMs")
	fs.Parse(args)
	if !*tokenStdin {
		return errors.New("-token-stdin is required")
	}
	token, err := node.ReadSecret(os.Stdin)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	o.NodeID, o.Credential, err = node.Enroll(ctx, o.HubURL, token, http.DefaultClient)
	if err != nil {
		return err
	}
	dir := dataDir(home)
	return node.InitConfig(o, filepath.Join(dir, "vms"), filepath.Join(dir, "images"))
}
