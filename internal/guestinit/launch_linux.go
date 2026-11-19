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

// Image is the image's PID 1, running in its own PID and cgroup namespace.
type Image struct {
	PID        int
	pidfd      int
	execCgroup int
	stop       syscall.Signal
	cmd        *exec.Cmd
	done       chan struct{}
}

// StartImage starts systemd or the nsinit shim as the PID 1 of a new PID namespace on the switched root.
func StartImage(r Root, img proto.ImageConfig, env map[string]string) (*Image, error) {
	if err := r.mountRun(); err != nil {
		return nil, err
	}
	if err := setupCgroups(); err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{Dir: "/", Stdout: os.Stdout, Stderr: os.Stderr}
	i := &Image{cmd: cmd, done: make(chan struct{})}
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
	if i.execCgroup, err = unix.Open(filepath.Join(cgroupRoot, "exec"), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0); err != nil {
		return nil, err
	}
	if err := hideInitMounts(); err != nil {
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWPID | syscall.CLONE_NEWCGROUP,
		UseCgroupFD: true,
		CgroupFD:    int(cg.Fd()),
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start image: %w", err)
	}
	i.PID = cmd.Process.Pid
	if i.pidfd, err = unix.PidfdOpen(i.PID, 0); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("pidfd of the image: %w", err)
	}
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

// Exec starts cmd inside the image: its PID namespace, in the exec cgroup.
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
	if err := unix.Setns(i.pidfd, unix.CLONE_NEWPID); err != nil {
		return fmt.Errorf("enter image pid namespace: %w", err)
	}
	// The cgroup namespace is not joined: cloning into /exec from inside it fails with ENOENT.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = i.execCgroup
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

// hideInitMounts detaches the init's /proc and /sys so the image mounts its own: theirs show only its PID and cgroup namespace.
func hideInitMounts() error {
	for _, m := range []string{"/proc", "/sys"} {
		if err := syscall.Unmount(m, syscall.MNT_DETACH); err != nil {
			return fmt.Errorf("detach %s: %w", m, err)
		}
	}
	return nil
}

// mountRun gives the image a tmpfs /run of ours, as systemd would otherwise mount its own over the platform sockets.
func (r Root) mountRun() error {
	run := r.join("run")
	if err := os.MkdirAll(run, 0o755); err != nil {
		return err
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	mounted := hasMount(f, run)
	f.Close()
	if mounted {
		return nil
	}
	if err := syscall.Mount("tmpfs", run, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=0755"); err != nil {
		return fmt.Errorf("mount %s: %w", run, err)
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

// runNsinit is PID 1 of an image without systemd: it mounts /proc and /sys, reaps orphans and supervises Entrypoint+Cmd until SIGTERM.
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
	for _, m := range []struct{ fstype, target string }{{"proc", "/proc"}, {"sysfs", "/sys"}} {
		if err := os.MkdirAll(m.target, 0o555); err == nil {
			if err := syscall.Mount(m.fstype, m.target, m.fstype, syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil && !errors.Is(err, syscall.EBUSY) {
				fmt.Fprintf(os.Stderr, "nsinit: mount %s: %v\n", m.target, err)
			}
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
