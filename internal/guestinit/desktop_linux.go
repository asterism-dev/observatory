package guestinit

import (
	"net"
	"os/exec"

	"golang.org/x/sys/unix"
)

// peerAllowed accepts root and dev only; the credentials are the kernel's, not the peer's claim.
func peerAllowed(c net.Conn) bool {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	var credErr error
	if raw.Control(func(fd uintptr) { cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }) != nil || credErr != nil {
		return false
	}
	return cred.Uid == 0 || cred.Uid == devID
}

// startDesktop serves the helpers' socket and returns the function that stops the desktop.
func startDesktop(root Root, agent *Agent, spawn func(*exec.Cmd) error) func() {
	d := NewDesktop(root, spawn, func() []string {
		return append(envList(agent.Env()), "HOME="+devHome, "USER="+devName, "LOGNAME="+devName, "XDG_RUNTIME_DIR="+desktopRunDir)
	})
	d.Allow = peerAllowed
	l, err := d.Listen()
	if err != nil {
		logf("init: desktop: %v", err)
		return d.Stop
	}
	go func() { logf("init: desktop: %v", d.Serve(l)) }()
	return d.Stop
}
