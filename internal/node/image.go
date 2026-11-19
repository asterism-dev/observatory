package node

import (
	"fmt"
	"os"
)

// CloneResize copies the image src to dst and grows it to sizeGB.
func CloneResize(src, dst string, sizeGB int) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	size := int64(sizeGB) << 30
	if size < st.Size() {
		return fmt.Errorf("disk %d GB is smaller than image %s (%d MB)", sizeGB, src, (st.Size()+1<<20-1)>>20)
	}
	if err := cloneFile(src, dst); err != nil {
		return err
	}
	return os.Truncate(dst, size)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if err := copyContents(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// PrimaryNet is a static address for the primary NIC (CIDR); the zero value means DHCP.
type PrimaryNet struct{ Address, Gateway string }
