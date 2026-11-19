//go:build linux

package firecracker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asterism-dev/observatory/internal/chassisconf"
	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/node/ociimage"
	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/provider/linuxvm"
)

const chassisTimeout = 60 * time.Second

type Provider struct {
	VMsDir, ImagesDir string

	imageRef string
	store    ociimage.Store
	// storeMu is held for writing only while pruning, which must not remove what a pull or disk build is using.
	storeMu  sync.RWMutex
	platform string // digest of the ready guest image; guarded by storeMu
	boot     guestboot

	mu  sync.Mutex // serializes NAT address allocation
	nbd linuxvm.NBDWatch

	fastBoot  bool
	fcVersion string
	building  sync.Mutex // held while a template builds; builds never queue

	tokens atomic.Pointer[node.TokenLookup]
}

func New(c node.Config) (*Provider, error) {
	var s struct {
		GuestImage string `json:"guestImage"`
		FastBoot   *bool  `json:"fastBoot"`
	}
	if err := c.ProviderSection(&s); err != nil {
		return nil, fmt.Errorf("config firecracker section: %w", err)
	}
	if err := linuxvm.CheckHost("firecracker", "jailer", "qemu-nbd"); err != nil {
		return nil, err
	}
	boot, err := loadGuestboot(guestbootDir, filepath.Join(c.Paths.VMs, ".guestboot"))
	if err != nil {
		return nil, err
	}
	// Layers and the unpacked tree hold setuid files and device nodes of any owner.
	store := ociimage.Store{Dir: filepath.Join(c.Paths.Images, "oci")}
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(store.Dir, 0o700); err != nil {
		return nil, err
	}
	p := &Provider{VMsDir: c.Paths.VMs, ImagesDir: c.Paths.Images, imageRef: guestImageRef(s.GuestImage), store: store, boot: boot, fastBoot: s.FastBoot == nil || *s.FastBoot}
	if p.fcVersion, err = installedFirecracker(); err != nil {
		return nil, err
	}
	p.removeUnfinishedTemplates()
	removeLegacyImages(p.ImagesDir)
	return p, nil
}

func (p *Provider) Name() string { return "firecracker" }

var (
	firecrackerVersionCmd = func(ctx context.Context) ([]byte, error) {
		return exec.CommandContext(ctx, "firecracker", "--version").Output()
	}
	versionTimeout = 5 * time.Second
)

func installedFirecracker() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	out, err := firecrackerVersionCmd(ctx)
	if err != nil {
		return "", fmt.Errorf("firecracker --version: %w", err)
	}
	v, _, _ := strings.Cut(string(out), "\n")
	return v, nil
}

// HypervisorChanged reports whether the installed Firecracker differs from the one running VMs, whose memory states it could not resume.
func (p *Provider) HypervisorChanged() bool {
	v, err := installedFirecracker()
	if err != nil {
		log.Printf("hypervisor check: %v", err)
		return false
	}
	return v != p.fcVersion
}

func (p *Provider) Hypervisor() proto.Hypervisor {
	return proto.Hypervisor{Type: "firecracker", Version: strings.TrimPrefix(p.fcVersion, "Firecracker ")}
}

// baseFor is the root disk of f's image, built on demand; VMs own their root.img clones of it.
func (p *Provider) baseFor(ctx context.Context, f node.VMFile) (string, error) {
	digest, err := imageDigest(f.Image)
	if err != nil {
		return "", err
	}
	p.storeMu.RLock()
	defer p.storeMu.RUnlock()
	base, err := p.store.Disk(ctx, digest)
	if err != nil {
		return "", fmt.Errorf("image %s: %w", f.Image, err)
	}
	return base, nil
}

func (p *Provider) cloneBase(ctx context.Context, f node.VMFile, dir string) error {
	base, err := p.baseFor(ctx, f)
	if err != nil {
		return err
	}
	return node.CloneResize(base, filepath.Join(dir, "root.img"), f.Spec.DiskGB)
}

// recloneBase replaces a template-cloned root.img with the base image, leaving an empty marker on failure so the next start retries.
func (p *Provider) recloneBase(ctx context.Context, f node.VMFile, dir, marker string) error {
	err := p.cloneBase(ctx, f, dir)
	if err != nil {
		if werr := os.WriteFile(marker, nil, 0o600); werr != nil {
			return errors.Join(err, werr)
		}
	}
	return err
}

func (p *Provider) Prepare(ctx context.Context, f node.VMFile, dir string) error {
	base, err := p.baseFor(ctx, f)
	if err != nil {
		return err
	}
	marker := filepath.Join(dir, templateMarker)
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if p.fastBoot && len(f.Spec.Attachments) == 0 && f.Spec.DiskGB >= templateDiskGB {
		key, err := templateKey(f, p.fcVersion, p.boot.id)
		if err != nil {
			return err
		}
		tpl := p.templateDir(key)
		if _, err := os.Stat(tpl); err != nil {
			go func() {
				if err := p.buildTemplate(context.Background(), key, base, f); err != nil {
					log.Printf("template %s: %v", key, err)
				}
			}()
		} else if err := node.CloneResize(filepath.Join(tpl, "root.img"), filepath.Join(dir, "root.img"), f.Spec.DiskGB); err != nil {
			log.Printf("vm %s: template %s: %v; booting cold", f.Spec.ID, key, err)
		} else {
			now := time.Now()
			os.Chtimes(tpl, now, now)
			if err := os.WriteFile(marker, []byte(key), 0o600); err != nil {
				return err
			}
			return p.Reseed(ctx, f, dir)
		}
	}
	if err := node.CloneResize(base, filepath.Join(dir, "root.img"), f.Spec.DiskGB); err != nil {
		return err
	}
	return p.Reseed(ctx, f, dir)
}

func (p *Provider) Reseed(_ context.Context, f node.VMFile, dir string) error {
	// Without a pushed Boot (node restarted, hub not yet heard from) the existing seed holds the last pushed keys and variables.
	if f.Boot == nil {
		if _, err := os.Stat(filepath.Join(dir, node.SeedISO)); err == nil {
			boot, err := node.SeedBoot(dir)
			if err != nil {
				return fmt.Errorf("read keys and variables from seed: %w", err)
			}
			f.Boot = boot
		}
	}
	p.mu.Lock()
	ip, err := linuxvm.AllocNATIP(p.VMsDir, f.Spec.ID)
	p.mu.Unlock()
	if err != nil {
		return err
	}
	return node.WriteSeedISO(dir, node.GuestSeedFiles(f, node.PrimaryNet{Address: ip + "/24", Gateway: linuxvm.NATGateway}))
}

func (p *Provider) Disks(f node.VMFile, dir string) ([]proto.Disk, error) {
	return node.ListDisks(f, dir, func(diskID string) string { return p.nbd.Err(f.Spec.ID, diskID) })
}

func (p *Provider) Start(ctx context.Context, f node.VMFile, dir string) (node.Instance, error) {
	marker := filepath.Join(dir, templateMarker)
	if key, err := os.ReadFile(marker); err == nil {
		os.Remove(marker)
		// An empty marker means an earlier re-clone failed: root.img is still the template's disk.
		if len(key) > 0 && len(f.Spec.Attachments) == 0 {
			tpl := p.templateDir(string(key))
			began := time.Now()
			inst, err := p.Restore(ctx, f, dir, node.RestoreFrom{StateDir: tpl, Takeoff: true, Shared: true})
			if err == nil {
				log.Printf("vm %s: started from template %s in %s", f.Spec.ID, key, time.Since(began).Round(time.Millisecond))
				return inst, nil
			}
			if ctx.Err() != nil {
				// The marker is gone and the guest may have written to the disk: the next start must not boot it.
				if cerr := p.recloneBase(context.Background(), f, dir, marker); cerr != nil {
					return nil, cerr
				}
				return nil, err
			}
			log.Printf("vm %s: start from template %s: %v; deleting the template and booting cold", f.Spec.ID, key, err)
			if err := os.RemoveAll(tpl); err != nil {
				log.Printf("template %s: %v", key, err)
			}
		}
		// The template's disk carries its machine id and host keys; a cold boot starts from the base image instead.
		if err := p.recloneBase(ctx, f, dir, marker); err != nil {
			return nil, err
		}
	}
	if err := p.Reseed(ctx, f, dir); err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	return p.coldBoot(ctx, f, dir)
}

// coldBoot boots dir's root.img with the seed already in dir.
func (p *Provider) coldBoot(_ context.Context, f node.VMFile, dir string) (*instance, error) {
	drives, nbds, err := linuxvm.AttachDisks(f, dir)
	if err != nil {
		return nil, err
	}
	files := append([]jailFile{{path: p.boot.kernel, shared: true}, {path: p.boot.initrd, shared: true}}, driveFiles(f, drives)...)
	return p.launch(f, dir, files, drives, nbds, true)
}

// Restore boots f from a memory state written by SaveState, with the drives at the jail paths the state names.
func (p *Provider) Restore(ctx context.Context, f node.VMFile, dir string, from node.RestoreFrom) (node.Instance, error) {
	seed := filepath.Join(dir, node.SeedISO)
	before := fileSize(seed)
	if err := p.Reseed(ctx, f, dir); err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	seedGrew := fileSize(seed) > before
	drives, nbds, err := linuxvm.AttachDisks(f, dir)
	if err != nil {
		return nil, err
	}
	files := driveFiles(f, drives)
	for _, n := range []string{node.StateFile, node.MemFile} {
		files = append(files, jailFile{path: filepath.Join(from.StateDir, n), shared: from.Shared})
	}
	inst, err := p.launch(f, dir, files, drives, nbds, false)
	if err != nil {
		return nil, err
	}
	inst.restored = true
	if err := inst.waitSock(5 * time.Second); err != nil {
		inst.Kill()
		return nil, err
	}
	if err := inst.load(ctx, f, dir, from, seedGrew); err != nil {
		inst.Kill()
		return nil, err
	}
	return inst, nil
}

// waitSock waits for Firecracker to listen on its API socket, which it does only after the jailer has set it up.
func (i *instance) waitSock(timeout time.Duration) error {
	for deadline := time.Now().Add(timeout); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(i.sock); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("firecracker api socket: %w", err)
		}
		select {
		case <-i.Done():
			return errors.New("firecracker exited before listening on its api socket")
		default:
		}
	}
}

func (i *instance) load(ctx context.Context, f node.VMFile, dir string, from node.RestoreFrom, seedGrew bool) error {
	nets := []netOverride{{IfaceID: "eth0", HostDevName: linuxvm.TapName(f.Spec.ID, 0)}}
	for n := range f.Spec.NICs {
		nets = append(nets, netOverride{IfaceID: "net" + strconv.Itoa(n), HostDevName: linuxvm.TapName(f.Spec.ID, n+1)})
	}
	if err := i.api.LoadSnapshot("/"+node.StateFile, "/"+node.MemFile, nets); err != nil {
		return err
	}
	// The guest keeps the drive sizes from the state until Firecracker re-reads the file and notifies it; a takeoff's state is another VM's.
	if seedGrew || from.Takeoff {
		if err := i.api.PatchDrive("seed", "/"+node.SeedISO); err != nil {
			return err
		}
	}
	if from.Takeoff {
		if err := i.api.PatchDrive("root", "/root.img"); err != nil {
			return err
		}
	}
	if from.Paused {
		i.paused = true
	} else if err := i.api.Resume(); err != nil {
		return err
	}
	if !from.Shared {
		// Firecracker maps the memory file, which keeps its data until the VM exits.
		for _, n := range []string{node.StateFile, node.MemFile} {
			os.Remove(filepath.Join(jailRoot(dir), n))
		}
		if err := os.RemoveAll(from.StateDir); err != nil {
			log.Printf("vm %s: remove state: %v", f.Spec.ID, err)
		}
	}
	if from.Paused {
		return nil
	}
	req := node.GuestRequest{Op: "resume", Time: time.Now().UnixNano()}
	if from.Takeoff {
		req.Op, req.MACs, req.Entropy = "takeoff", []string{f.MAC}, make([]byte, 32)
		for _, n := range f.Spec.NICs {
			req.MACs = append(req.MACs, n.MAC)
		}
		rand.Read(req.Entropy)
	}
	_, err := node.CallGuest(ctx, filepath.Join(jailRoot(dir), vsockPath), req)
	if err != nil && !from.Takeoff {
		// The guest runs where it was, only with a stale clock and seed; that beats discarding its memory.
		log.Printf("vm %s: guest agent: %v", f.Spec.ID, err)
		return nil
	}
	return err
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

func driveFiles(f node.VMFile, drives []string) []jailFile {
	var files []jailFile
	for i, d := range node.AttachOrder(f) {
		jf := jailFile{path: drives[i], readOnly: d.ReadOnly}
		// /dev/nbdN differs between attaches; a memory snapshot needs the drive at the same jail path when restored.
		if d.NBD {
			jf.name = node.DeviceName(i)
		}
		files = append(files, jf)
	}
	return files
}

// launch starts the jailed Firecracker; with boot it boots from fc.json, else it waits for a snapshot load.
func (p *Provider) launch(f node.VMFile, dir string, files []jailFile, drives, nbds []string, boot bool) (*instance, error) {
	proc, sock, uid, err := p.spawnJailed(f, dir, files, nbds, boot)
	if err != nil {
		linuxvm.DetachAll(f.Spec.ID, dir, nbds)
		return nil, err
	}
	js, err := openJailSock(sock)
	if err != nil {
		proc.Kill()
		return nil, err
	}
	go func() { <-proc.Done(); js.close() }()
	// Firecracker has no ACPI power-off and would idle forever after a halt.
	go proc.KillOnConsole("System halted")
	for _, id := range nbds {
		go p.nbd.Watch(f.Spec.ID, dir, id, proc.Done())
	}
	inst := &instance{Process: proc, sock: sock, api: fcAPI{dial: js.dial}, dir: dir, uid: uid}
	for i, d := range node.AttachOrder(f) {
		if d.NBD {
			inst.nbdDevs = append(inst.nbdDevs, drives[i])
		}
	}
	return inst, nil
}

// jailSock reaches the API socket through its directory's fd, as the jail path can exceed the 107-byte unix socket limit.
type jailSock struct {
	mu   sync.Mutex
	dir  *os.File
	name string
}

func openJailSock(sock string) (*jailSock, error) {
	d, err := os.Open(filepath.Dir(sock))
	if err != nil {
		return nil, err
	}
	return &jailSock{dir: d, name: filepath.Base(sock)}, nil
}

// dial holds the lock so close cannot free the fd number for another VM's jail mid-dial.
func (s *jailSock) dial(ctx context.Context) (net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == nil {
		return nil, errors.New("VM has stopped")
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d/%s", s.dir.Fd(), s.name))
}

func (s *jailSock) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dir.Close()
	s.dir = nil
}

// spawnJailed starts Firecracker under the jailer; to boot, files holds the kernel, the initrd and then the drives.
func (p *Provider) spawnJailed(f node.VMFile, dir string, files []jailFile, nbds []string, boot bool) (*linuxvm.Process, string, int, error) {
	p.mu.Lock()
	ip, err := linuxvm.AllocNATIP(p.VMsDir, f.Spec.ID)
	p.mu.Unlock()
	if err != nil {
		return nil, "", 0, err
	}
	uid, err := jailUID(ip)
	if err != nil {
		return nil, "", 0, err
	}
	fcPath, err := exec.LookPath("firecracker")
	if err != nil {
		return nil, "", 0, err
	}
	// The jail holds device nodes owned by the VM's UID; they must not outlive the VM.
	removeJail := func() { os.RemoveAll(jailBase(dir)) }
	inJail, err := buildJail(dir, uid, files)
	if err != nil {
		removeJail()
		return nil, "", 0, fmt.Errorf("jail: %w", err)
	}
	if boot {
		cfg, err := vmConfig(f, inJail[0], inJail[1], inJail[2:])
		if err != nil {
			removeJail()
			return nil, "", 0, err
		}
		if err := os.WriteFile(filepath.Join(jailRoot(dir), "fc.json"), cfg, 0o644); err != nil {
			removeJail()
			return nil, "", 0, err
		}
	}
	tokens, err := listenTokens(dir, uid)
	if err != nil {
		removeJail()
		return nil, "", 0, fmt.Errorf("token socket: %w", err)
	}
	go node.ServeTokens(tokens, f.Spec.ID, p.token)
	proc, err := linuxvm.Spawn(f, dir, jailerCmd(fcPath, dir, uid, boot), nbds, uid, func() { tokens.Close(); removeJail() })
	if err != nil {
		tokens.Close()
	}
	// Without a management NIC the NAT address is the only one the VM has.
	if err == nil && proc.IP == "" {
		proc.IP = ip
	}
	return proc, filepath.Join(jailRoot(dir), "fc.sock"), uid, err
}

func (p *Provider) WaitIdle(ctx context.Context, f node.VMFile, idle time.Duration) error {
	taps := []string{linuxvm.TapName(f.Spec.ID, 0)}
	for i := range f.Spec.NICs {
		taps = append(taps, linuxvm.TapName(f.Spec.ID, i+1))
	}
	return linuxvm.WaitIdle(ctx, taps, idle)
}

// WaitWake creates the taps owned by the VM's jail UID, as Restore's Spawn does, so Firecracker can attach to them.
func (p *Provider) WaitWake(ctx context.Context, f node.VMFile) error {
	p.mu.Lock()
	ip, err := linuxvm.AllocNATIP(p.VMsDir, f.Spec.ID)
	p.mu.Unlock()
	if err != nil {
		return err
	}
	uid, err := jailUID(ip)
	if err != nil {
		return err
	}
	return linuxvm.WaitWake(ctx, f, uid)
}

// StartChassis gives up when the node stops: systemd queues an ovn-controller restart behind the node's own stop job.
func (p *Provider) StartChassis(ctx context.Context, c chassisconf.Config) error {
	ctx, cancel := context.WithTimeout(ctx, chassisTimeout)
	defer cancel()
	_, err := chassisconf.Apply(c, "/", chassisconf.RunContext(ctx))
	return err
}

// StopChassis leaves OVS/OVN running: they belong to the host, not to this node process.
func (p *Provider) StopChassis() error { return nil }

type instance struct {
	*linuxvm.Process
	sock     string
	api      fcAPI
	dir      string
	uid      int
	nbdDevs  []string
	restored bool

	mu     sync.Mutex
	paused bool
}

// WaitIP of a restored VM returns at once: its guest booted long ago and prints no readiness marker again.
func (i *instance) WaitIP(ctx context.Context) (string, error) {
	if i.restored {
		return i.IP, nil
	}
	return i.Process.WaitIP(ctx)
}

// CPUTicks reads the jailed Firecracker, the shell's only child, by its host PID.
func (i *instance) CPUTicks() (uint64, error) {
	sh := strconv.Itoa(i.Cmd.Process.Pid)
	b, err := os.ReadFile("/proc/" + sh + "/task/" + sh + "/children")
	if err != nil {
		return 0, err
	}
	kids := strings.Fields(string(b))
	if len(kids) == 0 {
		return 0, errors.New("firecracker process not found")
	}
	child, err := strconv.Atoi(kids[0])
	if err != nil {
		return 0, err
	}
	return node.ProcTicks(child)
}

func (i *instance) Memory() (int, int, error) {
	st, err := i.api.BalloonStats()
	if err != nil || st.TotalMemory == 0 {
		return 0, 0, err
	}
	return int(st.TotalMemory >> 20), int((st.TotalMemory - min(st.AvailableMemory, st.TotalMemory)) >> 20), nil
}

func (i *instance) Pause() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.paused {
		return nil
	}
	if err := i.api.Pause(); err != nil {
		return err
	}
	i.paused = true
	return nil
}

func (i *instance) Resume() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.paused {
		return nil
	}
	if err := i.api.Resume(); err != nil {
		return err
	}
	i.paused = false
	return nil
}

// ReloadSeed links the VM dir's seed into the jail again, as Reseed renames a new file over it, and has Firecracker reopen it.
func (i *instance) ReloadSeed() error {
	dst := filepath.Join(jailRoot(i.dir), node.SeedISO)
	tmp := dst + ".new"
	os.Remove(tmp)
	if err := jailOne(jailFile{path: filepath.Join(i.dir, node.SeedISO), readOnly: true}, tmp, i.uid); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return i.api.PatchDrive("seed", "/"+node.SeedISO)
}

func (i *instance) GuestResume(ctx context.Context) error {
	_, err := node.CallGuest(ctx, filepath.Join(jailRoot(i.dir), vsockPath), node.GuestRequest{Op: "resume", Time: time.Now().UnixNano()})
	return err
}

func (i *instance) GuestTrust(ctx context.Context, cas map[string]string) error {
	_, err := node.CallGuest(ctx, filepath.Join(jailRoot(i.dir), vsockPath), node.GuestRequest{Op: "trust", Certs: cas})
	return err
}

func (i *instance) Flush() error {
	for _, dev := range i.nbdDevs {
		if err := linuxvm.FlushNBD(dev); err != nil {
			return err
		}
	}
	return nil
}

func (i *instance) SaveState(dir string) error {
	if err := i.Pause(); err != nil {
		return err
	}
	if err := i.Flush(); err != nil {
		return err
	}
	// A file left at these paths may be a link to a snapshot's or template's state, which Firecracker would truncate.
	for _, n := range []string{node.StateFile, node.MemFile} {
		if err := os.Remove(filepath.Join(jailRoot(i.dir), n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := i.api.CreateSnapshot("/"+node.StateFile, "/"+node.MemFile); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, n := range []string{node.StateFile, node.MemFile} {
		dst := filepath.Join(dir, n)
		if err := os.Rename(filepath.Join(jailRoot(i.dir), n), dst); err != nil {
			return err
		}
		// Guest memory holds the VM's keys and variables.
		if err := os.Chmod(dst, 0o600); err != nil {
			return err
		}
		if err := syncPath(dst); err != nil {
			return err
		}
	}
	if err := syncPath(dir); err != nil {
		return err
	}
	return syncPath(filepath.Join(i.dir, "root.img"))
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Stop asks the guest to shut down (Ctrl-Alt-Del reboots it, and reboot=k ends Firecracker) and kills it after 10s.
func (i *instance) Stop() error {
	// A paused guest cannot react to Ctrl-Alt-Del.
	if err := i.Resume(); err != nil {
		return i.Kill()
	}
	if err := i.api.CtrlAltDel(); err == nil {
		select {
		case <-i.Done():
			return nil
		case <-time.After(10 * time.Second):
		}
	}
	return i.Kill()
}
