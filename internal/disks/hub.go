// Package disks connects the hub to the storage server.
package disks

import (
	"context"
	"crypto/subtle"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/asterism-dev/observatory/internal/proto"
	"github.com/asterism-dev/observatory/internal/store"
)

// OfflineAfter is how long the storage server may stay silent before it counts as offline.
const OfflineAfter = 10 * time.Second

type Hub struct {
	store  *store.Store
	token  string
	notify func(org uuid.UUID)
	now    func() time.Time

	mu         sync.Mutex
	reportedAt time.Time
	files      map[string]proto.StorageFile
	exportErrs map[string]string
	version    string
}

func New(s *store.Store, token string, notify func(org uuid.UUID)) *Hub {
	return &Hub{store: s, token: token, notify: notify, now: time.Now}
}

func (h *Hub) TokenOK(token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) == 1
}

// State is the storage server's desired state, including export keys; callers must never log it.
func (h *Hub) State(ctx context.Context) (proto.StorageState, error) {
	return h.store.StorageState(ctx)
}

// SetStatus keeps the report in memory and writes what the files confirm (ready sizes, finished copies, purges) and finished restores to the store.
func (h *Hub) SetStatus(ctx context.Context, st proto.StorageStatus) error {
	files := make(map[string]proto.StorageFile, len(st.Files))
	for _, f := range st.Files {
		files[f.Name] = f
	}
	errs := map[string]string{}
	for _, x := range st.Exports {
		if x.Error != "" {
			errs[x.DiskID] = x.Error
		}
	}
	orgs, err := h.store.ApplyStorageFiles(ctx, st.Files)
	if err != nil {
		return err
	}
	// The files are committed: their orgs hear about them even when the restores fail.
	defer func() {
		for _, org := range orgs {
			h.notify(org)
		}
	}()
	restored, err := h.store.ApplyStorageRestored(ctx, st.Restored)
	if err != nil {
		return err
	}
	orgs = append(orgs, restored...)
	h.mu.Lock()
	h.reportedAt, h.files, h.exportErrs, h.version = h.now(), files, errs, st.Version
	h.mu.Unlock()
	return nil
}

func (h *Hub) Online() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.reportedAt.IsZero() && h.now().Sub(h.reportedAt) < OfflineAfter
}

func (h *Hub) Version() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.version
}

type Info struct {
	State          string
	AllocatedBytes int64
	ExportError    string
}

// Info is the disk as of the storage server's last report.
func (h *Hub) Info(d store.Disk) Info {
	h.mu.Lock()
	defer h.mu.Unlock()
	var f *proto.StorageFile
	if file, ok := h.files[proto.StorageFileName(d.ID.String())]; ok {
		f = &file
	}
	info := Info{State: State(d, f, h.files != nil), ExportError: h.exportErrs[d.ID.String()]}
	if f != nil {
		info.AllocatedBytes = f.AllocatedBytes
	}
	return info
}

// State derives a disk's state; reported is false until the storage server has sent a status, so nothing is missing before.
func State(d store.Disk, f *proto.StorageFile, reported bool) string {
	switch {
	case d.ReadyGB > 0 && reported && f == nil:
		return "missing"
	case d.Releasing:
		return "releasing"
	case d.ReadyGB == 0:
		return "creating"
	case d.ReadyGB < d.SizeGB:
		return "growing"
	}
	return "ready"
}
