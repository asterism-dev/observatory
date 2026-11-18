package firecracker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/asterism-dev/observatory/internal/node"
	"github.com/asterism-dev/observatory/internal/node/guest"
)

const (
	maxTemplates   = 4
	templateDiskGB = 5
	// templateMarker in a VM dir names the template Prepare cloned root.img from; the first Start restores that template.
	templateMarker = "from-template"
	templatesDir   = ".templates"
)

// templateKey identifies the template f can start from: same base image, vCPUs, RAM, NIC count and guest scripts.
func templateKey(base string, f node.VMFile) (string, error) {
	st, err := os.Stat(base)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%d\x00%d\x00%d\x00%d\x00", base, st.Size(), st.ModTime().UnixNano(), f.Spec.CPUs, f.Spec.MemoryMB, len(f.Spec.NICs))
	fmt.Fprintf(h, "%d\x00%s%d\x00%s", len(guest.BootScript), guest.BootScript, len(guest.AgentScript), guest.AgentScript)
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// evictTemplates removes the least recently used templates in dir beyond keep; using a template touches its mtime.
func evictTemplates(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type tpl struct {
		path string
		mod  int64
	}
	var tpls []tpl
	for _, e := range entries {
		if !e.IsDir() || filepath.Ext(e.Name()) == ".tmp" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		tpls = append(tpls, tpl{filepath.Join(dir, e.Name()), info.ModTime().UnixNano()})
	}
	sort.Slice(tpls, func(i, j int) bool { return tpls[i].mod > tpls[j].mod })
	for len(tpls) > keep {
		if err := os.RemoveAll(tpls[len(tpls)-1].path); err != nil {
			return err
		}
		tpls = tpls[:len(tpls)-1]
	}
	return nil
}
