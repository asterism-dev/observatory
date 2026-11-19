package guestinit

import (
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const noShellMessage = "this image has no shell; port forwarding still works"

type session struct {
	srv       *SSHServer
	sc        *ssh.ServerConn
	ch        ssh.Channel
	clientEnv map[string]string
	term      string
	ptmx, tty *os.File
	cmd       *exec.Cmd
	agent     net.Listener
	agentDir  string
}

func (s *SSHServer) serveSession(sc *ssh.ServerConn, nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	ss := &session{srv: s, sc: sc, ch: ch, clientEnv: map[string]string{}}
	defer ss.cleanup()
	started := false
	for req := range reqs {
		ok := false
		switch req.Type {
		case "pty-req":
			ok = !started && ss.allocPTY(req.Payload)
		case "window-change":
			ok = ss.resize(req.Payload)
		case "env":
			ok = ss.setEnv(req.Payload)
		case "auth-agent-req@openssh.com":
			ok = !started && ss.forwardAgent()
		case "signal":
			ok = ss.signal(req.Payload)
		case "shell", "exec", "subsystem":
			if started {
				break
			}
			var arg struct{ Value string }
			if req.Type != "shell" && ssh.Unmarshal(req.Payload, &arg) != nil {
				break
			}
			var argv []string
			if req.Type == "subsystem" {
				if argv = subsystems[arg.Value]; argv == nil {
					break
				}
			}
			started = true
			// The reply must precede exit-status, or OpenSSH reports a failed request instead of the exit code.
			_ = req.Reply(true, nil)
			if argv != nil {
				ss.run(argv[0], argv, ss.srv.Shell(), os.Stderr)
			} else {
				ss.start(arg.Value)
			}
			continue
		}
		if req.WantReply {
			_ = req.Reply(ok, nil)
		}
	}
}

func (ss *session) start(command string) {
	shell := ss.srv.Shell()
	if shell == "" {
		fmt.Fprintln(ss.ch.Stderr(), noShellMessage)
		ss.exit(127, 0)
		return
	}
	args := []string{"-" + filepath.Base(shell)}
	if command != "" {
		args = []string{filepath.Base(shell), "-c", command}
	}
	ss.run(shell, args, shell, ss.ch.Stderr())
}

// run spawns path as dev in the image; stderr is used only without a pty.
func (ss *session) run(path string, args []string, shell string, stderr io.Writer) {
	cmd := &exec.Cmd{Path: path, Args: args, Dir: devHome, Env: ss.environ(shell)}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: devID, Gid: devID, Groups: ss.srv.Root.devGroups()}}
	var stdin io.WriteCloser
	if ss.tty != nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = ss.tty, ss.tty, ss.tty
		cmd.SysProcAttr.Setsid, cmd.SysProcAttr.Setctty = true, true
	} else {
		var err error
		if stdin, err = cmd.StdinPipe(); err != nil {
			ss.fail(err)
			return
		}
		cmd.Stdout, cmd.Stderr = ss.ch, stderr
	}
	if err := ss.srv.Spawn(cmd); err != nil {
		ss.fail(err)
		return
	}
	ss.cmd = cmd
	if ss.tty != nil {
		ss.tty.Close()
		ss.tty = nil
		go func() { _, _ = io.Copy(ss.ptmx, ss.ch) }()
	} else {
		go func() {
			_, _ = io.Copy(stdin, ss.ch)
			stdin.Close()
		}()
	}
	go func() {
		if ss.ptmx != nil {
			// Reading the master ends with EIO once every holder of the tty is gone.
			_, _ = io.Copy(ss.ch, ss.ptmx)
		}
		_ = cmd.Wait()
		ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
		var sig syscall.Signal
		if ws.Signaled() {
			sig = ws.Signal()
		}
		ss.exit(uint32(ws.ExitStatus()), sig)
	}()
}

func (ss *session) fail(err error) {
	fmt.Fprintln(ss.ch.Stderr(), "start session:", err)
	ss.exit(1, 0)
}

func (ss *session) exit(status uint32, sig syscall.Signal) {
	if sig != 0 {
		_, _ = ss.ch.SendRequest("exit-signal", false, ssh.Marshal(struct {
			Signal     string
			CoreDumped bool
			Msg, Lang  string
		}{Signal: strings.TrimPrefix(unix.SignalName(sig), "SIG")}))
	} else {
		_, _ = ss.ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	}
	ss.ch.Close()
}

func (ss *session) environ(shell string) []string {
	env := maps.Clone(ss.srv.Env())
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, ss.clientEnv)
	env["HOME"], env["USER"], env["LOGNAME"], env["SHELL"] = devHome, devName, devName, shell
	if ss.term != "" {
		env["TERM"] = ss.term
	}
	if ss.agentDir != "" {
		env["SSH_AUTH_SOCK"] = "/tmp/" + filepath.Base(ss.agentDir) + "/agent"
	}
	rHost, rPort, _ := net.SplitHostPort(ss.sc.RemoteAddr().String())
	lHost, lPort, _ := net.SplitHostPort(ss.sc.LocalAddr().String())
	env["SSH_CONNECTION"] = strings.Join([]string{rHost, rPort, lHost, lPort}, " ")
	env["SSH_CLIENT"] = strings.Join([]string{rHost, rPort, lPort}, " ")
	var out []string
	for _, n := range slices.Sorted(maps.Keys(env)) {
		out = append(out, n+"="+env[n])
	}
	return out
}

func (ss *session) allocPTY(payload []byte) bool {
	var p struct {
		Term                      string
		Cols, Rows, Width, Height uint32
		Modes                     string
	}
	if ss.ptmx != nil || ssh.Unmarshal(payload, &p) != nil {
		return false
	}
	ptmx, tty, err := pty.Open()
	if err != nil {
		return false
	}
	_ = chown(tty.Name(), devID, -1)
	ss.ptmx, ss.tty, ss.term = ptmx, tty, p.Term
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(p.Rows), Cols: uint16(p.Cols)})
	return true
}

func (ss *session) resize(payload []byte) bool {
	var p struct{ Cols, Rows, Width, Height uint32 }
	if ss.ptmx == nil || ssh.Unmarshal(payload, &p) != nil {
		return false
	}
	return pty.Setsize(ss.ptmx, &pty.Winsize{Rows: uint16(p.Rows), Cols: uint16(p.Cols)}) == nil
}

// setEnv accepts only the locale, like OpenSSH's default AcceptEnv.
func (ss *session) setEnv(payload []byte) bool {
	var p struct{ Name, Value string }
	if ssh.Unmarshal(payload, &p) != nil || !envName.MatchString(p.Name) || strings.ContainsRune(p.Value, 0) {
		return false
	}
	if p.Name != "LANG" && !strings.HasPrefix(p.Name, "LC_") {
		return false
	}
	ss.clientEnv[p.Name] = p.Value
	return true
}

func (ss *session) signal(payload []byte) bool {
	var p struct{ Signal string }
	if ss.cmd == nil || ssh.Unmarshal(payload, &p) != nil {
		return false
	}
	sig := unix.SignalNum("SIG" + p.Signal)
	return sig != 0 && ss.cmd.Process.Signal(sig) == nil
}

// forwardAgent listens on a socket in the image's /tmp and relays each connection to the client's agent.
func (ss *session) forwardAgent() bool {
	if ss.agent != nil {
		return true
	}
	dir, err := os.MkdirTemp(ss.srv.Root.join("tmp"), "ssh-")
	if err != nil {
		return false
	}
	sock := filepath.Join(dir, "agent")
	l, err := net.Listen("unix", sock)
	if err != nil || chown(sock, devID, devID) != nil || chown(dir, devID, devID) != nil {
		if l != nil {
			l.Close()
		}
		os.RemoveAll(dir)
		return false
	}
	ss.agent, ss.agentDir = l, dir
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				ch, reqs, err := ss.sc.OpenChannel("auth-agent@openssh.com", nil)
				if err != nil {
					c.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				pipe(ch, c)
			}()
		}
	}()
	return true
}

func (ss *session) cleanup() {
	if ss.tty != nil {
		ss.tty.Close()
	}
	if ss.ptmx != nil {
		ss.ptmx.Close()
	}
	if ss.agent != nil {
		ss.agent.Close()
		os.RemoveAll(ss.agentDir)
	}
}
