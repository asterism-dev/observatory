package guestinit

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/asterism-dev/observatory/internal/proto"
)

const (
	platformDir = "/.observatory"
	cgroupRoot  = "/sys/fs/cgroup"
	// systemd halts on SIGRTMIN+3 (glibc numbering), as docker stop does for systemd containers.
	systemdHalt      = syscall.Signal(34 + 3)
	imageStopTimeout = 30 * time.Second
)

// Image is the image's PID 1, running in its own PID and cgroup namespace, chrooted into the root.
type Image struct {
	PID  int
	root string
	stop syscall.Signal
	cmd  *exec.Cmd
	done chan struct{}
}

// StartImage binds the platform directory and /dev into the root, then starts systemd or the nsinit shim as the namespace's PID 1.
func StartImage(r Root, img proto.ImageConfig, env map[string]string) (*Image, error) {
	if err := r.bindPlatform(); err != nil {
		return nil, err
	}
	if err := setupCgroups(); err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{Dir: "/", Stdout: os.Stdout, Stderr: os.Stderr}
	i := &Image{root: r.Path, cmd: cmd, done: make(chan struct{})}
	if init := r.systemd(); init != "" {
		cmd.Path, cmd.Args = init, []string{init}
		cmd.Env = []string{"container=observatory", "PATH=" + env["PATH"]}
		i.stop = systemdHalt
	} else {
		cmd.Path = platformBin + "/observatory-init"
		cmd.Args = append([]string{cmd.Path, "nsinit", "-dir", img.WorkingDir, "-stop-signal", img.StopSignal, "--"},
			slices.Concat(img.Entrypoint, img.Cmd)...)
		cmd.Env = append(envList(env), "container=observatory")
		i.stop = syscall.SIGTERM
	}
	cg, err := os.Open(filepath.Join(cgroupRoot, "payload"))
	if err != nil {
		return nil, err
	}
	defer cg.Close()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot:      r.Path,
		Cloneflags:  syscall.CLONE_NEWPID | syscall.CLONE_NEWCGROUP,
		UseCgroupFD: true,
		CgroupFD:    int(cg.Fd()),
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start image: %w", err)
	}
	i.PID = cmd.Process.Pid
	go func() {
		_ = cmd.Wait()
		close(i.done)
	}()
	return i, nil
}

// Done is closed once the image's PID 1 has exited.
func (i *Image) Done() <-chan struct{} { return i.done }

// shutdownCmd maps how the image's PID 1 ended to reboot(2): the kernel kills a namespace init that calls reboot() with SIGINT for halt/poweroff and SIGHUP for restart.
func (i *Image) shutdownCmd() int {
	ws, _ := i.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if ws.Signaled() {
		return shutdownFor(ws.Signal())
	}
	return syscall.LINUX_REBOOT_CMD_POWER_OFF
}

// Stop asks the image to shut down and kills its namespace after timeout.
func (i *Image) Stop(timeout time.Duration) {
	_ = i.cmd.Process.Signal(i.stop)
	select {
	case <-i.done:
	case <-time.After(timeout):
		_ = i.cmd.Process.Kill()
		<-i.done
	}
}

// Exec starts cmd inside the image: its PID namespace, chrooted into the root, in the exec cgroup.
func (i *Image) Exec(cmd *exec.Cmd) error {
	errc := make(chan error, 1)
	go func() {
		// Never unlocked: the thread's pid_for_children stays in the image, so the runtime must discard it when this goroutine exits.
		runtime.LockOSThread()
		errc <- i.startIn(cmd)
	}()
	return <-errc
}

func (i *Image) startIn(cmd *exec.Cmd) error {
	ns, err := os.Open(fmt.Sprintf("/proc/%d/ns/pid", i.PID))
	if err != nil {
		return err
	}
	defer ns.Close()
	if err := unix.Setns(int(ns.Fd()), unix.CLONE_NEWPID); err != nil {
		return fmt.Errorf("enter image pid namespace: %w", err)
	}
	// The cgroup namespace is not joined: cloning into /exec from inside it fails with ENOENT.
	cg, err := os.Open(filepath.Join(cgroupRoot, "exec"))
	if err != nil {
		return err
	}
	defer cg.Close()
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Chroot = i.root
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(cg.Fd())
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}
	return cmd.Start()
}

// systemd returns the image's systemd to run as its init, or "".
func (r Root) systemd() string {
	if real, ok := r.resolve("sbin/init"); ok && filepath.Base(real) == "systemd" && r.executable("/sbin/init") {
		return "/sbin/init"
	}
	if r.executable("/lib/systemd/systemd") {
		return "/lib/systemd/systemd"
	}
	return ""
}

// bindPlatform mounts /.observatory read-only into the image and gives it the init's /dev.
func (r Root) bindPlatform() error {
	if _, err := os.Stat(platformDir); err == nil {
		target := r.join(platformDir)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		if err := syscall.Mount(platformDir, target, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("bind %s: %w", platformDir, err)
		}
		if err := syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("remount %s read-only: %w", platformDir, err)
		}
	}
	dev := r.join("dev")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount("/dev", dev, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind /dev: %w", err)
	}
	return nil
}

// setupCgroups moves the init into /init and enables every controller for /payload (the image) and /exec (sessions).
func setupCgroups() error {
	for _, d := range []string{"init", "payload", "exec"} {
		if err := os.MkdirAll(filepath.Join(cgroupRoot, d), 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(cgroupRoot, "init/cgroup.procs"), []byte("0"), 0); err != nil {
		return fmt.Errorf("move init cgroup: %w", err)
	}
	ctrls, err := os.ReadFile(filepath.Join(cgroupRoot, "cgroup.controllers"))
	if err != nil {
		return err
	}
	var enable []string
	for _, c := range strings.Fields(string(ctrls)) {
		enable = append(enable, "+"+c)
	}
	if len(enable) == 0 {
		return nil
	}
	if err := os.WriteFile(filepath.Join(cgroupRoot, "cgroup.subtree_control"), []byte(strings.Join(enable, " ")), 0); err != nil {
		return fmt.Errorf("enable cgroup controllers: %w", err)
	}
	return nil
}

// runNsinit is PID 1 of an image without systemd: it mounts /proc, reaps orphans and supervises Entrypoint+Cmd until SIGTERM.
func runNsinit(args []string) int {
	fs := flag.NewFlagSet("nsinit", flag.ContinueOnError)
	dir := fs.String("dir", "", "working directory of the service")
	stop := fs.String("stop-signal", "", "signal that stops the service")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "nsinit: must run as PID 1 of the image")
		return 2
	}
	if err := os.MkdirAll("/proc", 0o555); err == nil {
		if err := syscall.Mount("proc", "/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil && !errors.Is(err, syscall.EBUSY) {
			fmt.Fprintln(os.Stderr, "nsinit: mount /proc:", err)
		}
	}
	// Caught and dropped rather than ignored: SIG_IGN would be inherited by the service and swallow a SIGINT StopSignal.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGINT, syscall.SIGHUP)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()
	chld := make(chan os.Signal, 1)
	signal.Notify(chld, syscall.SIGCHLD)

	svc := &Service{Argv: fs.Args(), Env: os.Environ(), Dir: *dir, StopSignal: *stop, Log: serviceLog}
	if svc.Dir == "" {
		svc.Dir = "/"
	}
	done := make(chan struct{})
	go func() {
		if len(svc.Argv) > 0 {
			svc.Run(ctx)
		} else {
			<-ctx.Done()
		}
		close(done)
	}()
	// The tick catches orphans that exited while the service's own zombie was first in line.
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return 0
		case <-chld:
		case <-tick.C:
		}
		reapOrphans(int(svc.pid.Load()))
	}
}

// reapOrphans waits for exited children except the service, whose exec.Cmd collects its own status.
func reapOrphans(service int) {
	for {
		var info unix.Siginfo
		if err := unix.Waitid(unix.P_ALL, 0, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil); err != nil {
			return
		}
		// unix.Siginfo hides si_pid; on 64-bit Linux it follows si_signo, si_errno, si_code and 4 bytes of padding.
		pid := int(*(*int32)(unsafe.Add(unsafe.Pointer(&info), 16)))
		if pid == 0 || pid == service {
			return
		}
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	}
}
