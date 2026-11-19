package node

import (
	"context"
	"time"

	"github.com/asterism-dev/observatory/internal/chassisconf"
	"github.com/asterism-dev/observatory/internal/proto"
)

type Provider interface {
	Name() string
	// Prepare runs once at create time; Start runs on create and again after every node restart.
	Prepare(ctx context.Context, f VMFile, dir string) error
	// Reseed rewrites only the cloud-init seed of a prepared, stopped or paused VM; disks are left alone.
	Reseed(ctx context.Context, f VMFile, dir string) error
	Start(ctx context.Context, f VMFile, dir string) (Instance, error)
	Restore(ctx context.Context, f VMFile, dir string, from RestoreFrom) (Instance, error)
	// Disks lists the block devices Start attaches, in attach order.
	Disks(f VMFile, dir string) ([]proto.Disk, error)
	WaitIdle(ctx context.Context, f VMFile, idle time.Duration) error
	WaitWake(ctx context.Context, f VMFile) error
	StartChassis(ctx context.Context, c chassisconf.Config) error
	StopChassis() error
}

type Instance interface {
	WaitIP(ctx context.Context) (string, error)
	Stop() error
	// Kill powers the VM off without asking the guest.
	Kill() error
	Done() <-chan struct{}
	Pause() error
	Resume() error
	// Flush writes what the paused guest sent to its NBD disks through to the storage server.
	Flush() error
	// SaveState pauses the VM, flushes its disks and writes StateFile and MemFile into dir; the VM stays paused.
	SaveState(dir string) error
	// GuestResume tells the guest agent it was resumed, so it sets its clock and re-reads its seed.
	GuestResume(ctx context.Context) error
	// GuestTrust makes the running guest trust exactly cas (CA id → cert PEM).
	GuestTrust(ctx context.Context, cas map[string]string) error
	// ReloadSeed makes the VM read the seed Reseed last wrote, so a paused guest resumes with the current keys and variables.
	ReloadSeed() error
	CPUTicks() (uint64, error)
	Memory() (totalMB, usedMB int, err error)
}

type RestoreFrom struct {
	StateDir string
	Takeoff  bool
	// Shared state files are linked read-only into the jail and never deleted.
	Shared bool
	// Paused leaves the restored VM paused; the guest agent call is skipped, as a paused guest cannot answer it.
	Paused bool
}
