package guestinit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/asterism-dev/observatory/internal/proto"
)

const rootDevice = "/dev/vda"

// Run is PID 1: it boots the guest, then blocks until the kernel asks for poweroff or reboot.
func Run() error {
	// Registered first so a signal during boot is not lost.
	// With ctrl-alt-del off the kernel delivers it to PID 1 as SIGINT instead of rebooting hard.
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_CAD_OFF); err != nil {
		return fmt.Errorf("disable ctrl-alt-del: %w", err)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGHUP)

	if err := mountBase(); err != nil {
		return err
	}
	seedDev, err := FindSeedDevice("/sys/block")
	if err != nil {
		return err
	}
	seed, err := ReadSeed(seedDev)
	if err != nil {
		return err
	}
	root, err := mountRoot(rootDevice)
	if err != nil {
		return err
	}
	if err := growRoot(rootDevice); err != nil {
		return err
	}
	agent := newAgent(root)
	go func() { fmt.Fprintln(os.Stderr, "init: agent:", agent.listen()) }()
	// Tasks 6-10 add their steps here, in boot order, each taking root and seed.
	if err := root.EnsureDev(); err != nil {
		return err
	}
	// Failures below leave a reachable or at least debuggable guest, so they are logged rather than fatal.
	if err := root.WriteHostname(seed.Hostname, seed.InstanceID); err != nil {
		fmt.Fprintln(os.Stderr, "init: hostname:", err)
	}
	if err := syscall.Sethostname([]byte(seed.Hostname)); err != nil {
		fmt.Fprintln(os.Stderr, "init: hostname:", err)
	}
	if err := ConfigureNICs(seed.NICs); err != nil {
		fmt.Fprintln(os.Stderr, "init: network:", err)
	}
	if err := root.WriteResolv(seed.NICs); err != nil {
		fmt.Fprintln(os.Stderr, "init: resolv.conf:", err)
	}
	hostKeys, err := root.HostKeys(false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "init: host keys:", err)
	}
	if err := MountDisks(root, seed.Disks); err != nil {
		fmt.Fprintln(os.Stderr, "init: disks:", err)
	}
	// A guest without the CAs still runs; only hosts bound to the egress proxy fail TLS.
	if _, err := root.InstallCAs(seed.EgressCAs); err != nil {
		fmt.Fprintln(os.Stderr, "init: egress CAs:", err)
	}
	agent.setSeedKeys(seed.SSHKeys)
	env := agent.applyEnv(seed)

	// Without the image the guest still serves SSH port forwarding and the agent, so a failed start is not fatal.
	img, err := StartImage(root, seed.Image, env)
	if err != nil {
		fmt.Fprintln(os.Stderr, "init: image:", err)
	}
	agent.SetSSH(startSSH(root, agent, hostKeys, img))
	// Before SetReady: takeoff reads agent.Sync, and none is served until ready.
	startIntegrations(root, agent, img)
	agent.SetReady()
	go announceReadyWhenAddressed(seed.NICs)

	var imageDone <-chan struct{}
	if img != nil {
		imageDone = img.Done()
	}
	// The kernel signals PID 1 only for ctrl-alt-del (SIGINT); the image's own poweroff/restart reaches us as its exit status.
	select {
	case sig := <-sigs:
		shutdown(img, shutdownFor(sig.(syscall.Signal)))
	case <-imageDone:
		shutdown(img, img.shutdownCmd())
	}
	return nil
}

// startIntegrations syncs at boot, every 5 minutes and on takeoff; claude runs inside the image as dev.
func startIntegrations(root Root, agent *Agent, img *Image) {
	trigger := make(chan struct{}, 1)
	agent.Sync = func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}
	startIn := func(cmd *exec.Cmd) error {
		if img == nil {
			return errors.New("the image is not running")
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: devID, Gid: devID, Groups: root.devGroups()}}
		return img.Exec(cmd)
	}
	go runIntegrationSync(context.Background(), integrationsInterval, trigger, func(ctx context.Context) error {
		return root.SyncIntegrations(ctx, startIn, agent.Env())
	})
}

func startSSH(root Root, agent *Agent, hostKeys []ssh.Signer, img *Image) *SSHServer {
	l, err := net.Listen("tcp", ":22")
	if err != nil {
		fmt.Fprintln(os.Stderr, "init: ssh:", err)
		return nil
	}
	srv := &SSHServer{
		Keys:     func() []ssh.PublicKey { return root.AuthorizedKeys(agent.SeedKeys()) },
		HostKeys: hostKeys,
		Spawn: func(cmd *exec.Cmd) error {
			if img == nil {
				return errors.New("the image is not running")
			}
			return img.Exec(cmd)
		},
		Env: agent.Env,
		Shell: func() string {
			if sh := root.DevShell(); sh != "/bin/false" {
				return sh
			}
			return ""
		},
		Root: root,
	}
	go func() {
		fmt.Fprintln(os.Stderr, "init: ssh:", srv.Serve(l))
	}()
	return srv
}

// announceReadyWhenAddressed writes the ready marker once the primary NIC has an IPv4 address, giving up after 300 s like /run/vmm-ready.sh did.
func announceReadyWhenAddressed(nics []proto.GuestNIC) {
	if len(nics) > 0 && !waitIPv4(nics[0].Name, 300*time.Second) {
		fmt.Fprintln(os.Stderr, "init: primary NIC got no IPv4 address, not reporting ready")
		return
	}
	for _, path := range []string{"/dev/hvc0", "/dev/ttyS0"} {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			continue
		}
		fmt.Fprintln(f, proto.ReadyMarker)
		f.Close()
		return
	}
}

// shutdownFor maps the signal that ends a guest to a reboot(2) command: SIGHUP restarts, anything else powers off.
func shutdownFor(sig syscall.Signal) int {
	if sig == syscall.SIGHUP {
		return syscall.LINUX_REBOOT_CMD_RESTART
	}
	return syscall.LINUX_REBOOT_CMD_POWER_OFF
}

// shutdown stops the image, unmounts /newroot and powers off or reboots; it only returns on failure.
func shutdown(img *Image, cmd int) {
	if img != nil {
		img.Stop(imageStopTimeout)
	}
	if err := unmountRoot(); err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
	}
	syscall.Sync()
	if err := syscall.Reboot(cmd); err != nil {
		fmt.Fprintln(os.Stderr, "init: reboot:", err)
	}
}

func unmountRoot() error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	defer f.Close()
	var errs []error
	for _, m := range mountsBelow(f, "/newroot") {
		err := syscall.Unmount(m, 0)
		if errors.Is(err, syscall.EBUSY) {
			err = syscall.Unmount(m, syscall.MNT_DETACH)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("umount %s: %w", m, err))
		}
	}
	return errors.Join(errs...)
}
