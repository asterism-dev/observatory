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
	"golang.org/x/sys/unix"

	"github.com/asterism-dev/observatory/internal/proto"
)

const rootDevice = "/dev/vda"

// Run is PID 1: it boots the guest, then blocks until the kernel asks for poweroff or reboot.
func Run() error {
	// Registered first so a signal during boot is not lost; with ctrl-alt-del off the kernel sends PID 1 SIGINT instead of rebooting hard.
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_CAD_OFF); err != nil {
		logf("init: disable ctrl-alt-del: %v", err)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGHUP)
	telemetry = newSink(dialTelemetry)

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
	if err := mountRoot(rootDevice); err != nil {
		return err
	}
	if err := growFS(rootDevice, newRoot); err != nil {
		logf("init: grow root filesystem: %v", err)
	}
	if err := switchRoot(newRoot); err != nil {
		return err
	}
	root := Root{Path: "/"}
	// Kept open: StartImage detaches this /proc, and shutdown still needs our mountinfo.
	proc, err := os.Open("/proc")
	if err != nil {
		logf("init: open /proc: %v", err)
	}
	agent := newAgent(root)
	go func() { logf("init: agent: %v", agent.listen()) }()
	var hostKeys []ssh.Signer
	runSteps([]bootStep{
		{"dev", root.EnsureDev},
		{"hostname", func() error { return root.WriteHostname(seed.Hostname, seed.InstanceID) }},
		{"sethostname", func() error { return syscall.Sethostname([]byte(seed.Hostname)) }},
		{"network", func() error { return ConfigureNICs(seed.NICs) }},
		{"resolv.conf", func() error { return root.WriteResolv(seed.NICs) }},
		{"host keys", func() (err error) { hostKeys, err = root.HostKeys(false); return err }},
		{"disks", func() error { return MountDisks(root, seed.Disks) }},
		// A guest without the CAs still runs; only hosts bound to the egress proxy fail TLS.
		{"egress CAs", func() error { _, err := root.InstallCAs(seed.EgressCAs); return err }},
	})
	agent.setSeedKeys(seed.SSHKeys)
	env := agent.applyEnv(seed)

	// cmd.log survives reboots: the service log starts at what this boot adds.
	var cmdLogStart int64
	if fi, err := os.Stat(serviceLog); err == nil {
		cmdLogStart = fi.Size()
	}
	// Without the image the guest still serves SSH port forwarding and the agent, so a failed start is not fatal.
	img, err := StartImage(root, seed.Image, env)
	if err != nil {
		logf("init: image: %v", err)
	} else {
		// In the image's PID namespace and exec cgroup, the image's journalctl cannot reach the init and dies with the image.
		go followServiceLog(context.Background(), telemetry, root.systemd() != "", img.Exec, cmdLogStart)
	}
	mounts := []string{"/"}
	for _, d := range seed.Disks {
		mounts = append(mounts, root.join(d.MountPath))
	}
	// After StartImage, /proc is the image's procfs.
	go runUsage(context.Background(), telemetry, newUsage("/proc", mounts, time.Now()))
	agent.SetSSH(startSSH(root, agent, hostKeys, img))
	// Before SetReady: takeoff reads agent.Sync and StopDesktop, and none is served until ready.
	startIntegrations(root, agent, img)
	agent.StopDesktop = startDesktop(root, agent, devSpawn(root, img))
	agent.SetReady()
	go announceReadyWhenAddressed(seed.NICs)

	var imageDone <-chan struct{}
	if img != nil {
		imageDone = img.Done()
	}
	// The kernel signals PID 1 only for ctrl-alt-del (SIGINT); the image's own poweroff/restart reaches us as its exit status.
	select {
	case sig := <-sigs:
		shutdown(img, proc, shutdownFor(sig.(syscall.Signal)))
	case <-imageDone:
		shutdown(img, proc, img.shutdownCmd())
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
	startIn := devSpawn(root, img)
	go runIntegrationSync(context.Background(), integrationsInterval, trigger, func(ctx context.Context) error {
		return root.SyncIntegrations(ctx, startIn, agent.Env())
	})
}

func devSpawn(root Root, img *Image) func(*exec.Cmd) error {
	return func(cmd *exec.Cmd) error {
		if img == nil {
			return errors.New("the image is not running")
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: devID, Gid: devID, Groups: root.devGroups()}}
		return img.Exec(cmd)
	}
}

func startSSH(root Root, agent *Agent, hostKeys []ssh.Signer, img *Image) *SSHServer {
	l, err := net.Listen("tcp", ":22")
	if err != nil {
		logf("init: ssh: %v", err)
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
		logf("init: ssh: %v", srv.Serve(l))
	}()
	return srv
}

// announceReadyWhenAddressed writes the ready marker once the primary NIC has an IPv4 address, giving up after 300 s like /run/vmm-ready.sh did.
func announceReadyWhenAddressed(nics []proto.GuestNIC) {
	if len(nics) > 0 && !waitIPv4(nics[0].Name, 300*time.Second) {
		logf("init: primary NIC got no IPv4 address, not reporting ready")
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

// shutdown stops the image, unmounts all but "/", remounts it read-only and powers off or reboots; it only returns on failure.
func shutdown(img *Image, proc *os.File, cmd int) {
	if img != nil {
		img.Stop(imageStopTimeout)
	}
	if err := unmountAll(proc); err != nil {
		consolef("init: %v", err)
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
		consolef("init: remount / read-only: %v", err)
	}
	syscall.Sync()
	if err := syscall.Reboot(cmd); err != nil {
		consolef("init: reboot: %v", err)
	}
}

func unmountAll(proc *os.File) error {
	fd, err := unix.Openat(int(proc.Fd()), "self/mountinfo", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("mountinfo: %w", err)
	}
	f := os.NewFile(uintptr(fd), "mountinfo")
	defer f.Close()
	var errs []error
	for _, m := range mountsBelow(f, "/") {
		if m == "/" {
			continue
		}
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
