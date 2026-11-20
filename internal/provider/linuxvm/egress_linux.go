package linuxvm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/proto"
)

// egressKillWait bounds how long Kill waits for a SIGKILLed proxy before Done closes anyway.
const egressKillWait = 5 * time.Second

// Egress runs per-VM proxies as `<Binary> egress` and redirects the VMs' NAT-tap HTTPS to them with nftables.
type Egress struct {
	VMsDir, Binary string
}

// NewEgress recreates the node's egress tables, dropping redirects a previous run left behind.
func NewEgress(vmsDir, binary string) (*Egress, error) {
	if err := nftApply(egressRules(nameservers("/etc/resolv.conf"))); err != nil {
		return nil, err
	}
	return &Egress{VMsDir: vmsDir, Binary: binary}, nil
}

func nftApply(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %w: %s", err, out)
	}
	return nil
}

// slot reads the VM's NAT address without allocating one: the provider owns allocation.
func (e *Egress) slot(id string) (natIP string, uid, port int, mark uint32, err error) {
	b, err := os.ReadFile(filepath.Join(e.VMsDir, id, natFile))
	if err != nil {
		return "", 0, 0, 0, err
	}
	natIP = strings.TrimSpace(string(b))
	uid, port, mark, err = egressSlot(natIP)
	return natIP, uid, port, mark, err
}

// Redirect replaces any elements of the VM in one transaction; adding first makes the delete succeed when none exist.
func (e *Egress) Redirect(f node.VMFile) error {
	_, _, port, mark, err := e.slot(f.Spec.ID)
	if err != nil {
		return err
	}
	tap := TapName(f.Spec.ID, 0)
	return nftApply(egressElements(true, tap, port, mark) + egressElements(false, tap, port, mark) + egressElements(true, tap, port, mark))
}

func (e *Egress) Unredirect(f node.VMFile) error {
	_, _, port, mark, err := e.slot(f.Spec.ID)
	if err != nil {
		return err
	}
	tap := TapName(f.Spec.ID, 0)
	return nftApply(egressElements(true, tap, port, mark) + egressElements(false, tap, port, mark))
}

func (e *Egress) Spawn(f node.VMFile) (node.EgressProc, error) {
	id := f.Spec.ID
	natIP, uid, port, _, err := e.slot(id)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(e.Binary, "egress", "-listen", NATGateway+":"+strconv.Itoa(port), "-vm", id, "-peer", natIP)
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid)},
		Pdeathsig:  syscall.SIGKILL,
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	p := &egressProc{cmd: cmd, stdin: stdin, ready: make(chan struct{}), exited: make(chan struct{}), done: make(chan struct{})}
	var readers sync.WaitGroup
	readers.Add(2)
	started := make(chan error, 1)
	go func() {
		// Pdeathsig fires when the starting thread exits, so this goroutine keeps its thread until the proxy is reaped.
		runtime.LockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		readers.Wait()
		cmd.Wait()
		close(p.exited)
		p.doneOnce.Do(func() { close(p.done) })
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	go func() {
		defer readers.Done()
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if sc.Text() == "ready" {
				p.readyOnce.Do(func() { close(p.ready) })
			}
		}
		io.Copy(io.Discard, stdout)
	}()
	go func() {
		defer readers.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			slog.Info("egress proxy output", "vm", id, "line", sc.Text())
		}
		// An over-long line ends the scanner; draining keeps the proxy from blocking on a full pipe.
		io.Copy(io.Discard, stderr)
	}()
	return p, nil
}

type egressProc struct {
	cmd       *exec.Cmd
	mu        sync.Mutex
	stdin     io.WriteCloser
	ready     chan struct{}
	readyOnce sync.Once
	exited    chan struct{}
	done      chan struct{}
	doneOnce  sync.Once
}

// Send writes t as one JSON line; json.Marshal never emits newlines, so the line is never blank or split.
func (p *egressProc) Send(t proto.Egress) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	defer clear(b)
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.stdin.Write(b); err != nil {
		return err
	}
	_, err = io.WriteString(p.stdin, "\n")
	return err
}

func (p *egressProc) Ready() <-chan struct{} { return p.ready }
func (p *egressProc) Done() <-chan struct{}  { return p.done }

// Kill closes Done once the process is reaped or egressKillWait passed, so a stuck process cannot block the supervisor.
func (p *egressProc) Kill() {
	p.cmd.Process.Kill()
	go func() {
		select {
		case <-p.exited:
		case <-time.After(egressKillWait):
			slog.Warn("egress proxy still running after SIGKILL", "pid", p.cmd.Process.Pid, "wait", egressKillWait)
			p.doneOnce.Do(func() { close(p.done) })
		}
	}()
}
