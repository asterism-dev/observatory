package node

import (
	"context"

	"github.com/asterism-dev/observatory/internal/proto"
)

type Provider interface {
	Name() string
	// Prepare runs once at create time; Start runs on create and again after every node restart.
	Prepare(ctx context.Context, f VMFile, dir string) error
	// Reseed rewrites only the cloud-init seed of a prepared, stopped VM; disks are left alone.
	Reseed(ctx context.Context, f VMFile, dir string) error
	Start(f VMFile, dir string) (Instance, error)
	// Disks lists the block devices Start attaches, in attach order.
	Disks(f VMFile, dir string) ([]proto.Disk, error)
}

type Instance interface {
	WaitIP(ctx context.Context) (string, error)
	Stop() error
	// Kill powers the VM off without asking the guest.
	Kill() error
	Done() <-chan struct{}
}
