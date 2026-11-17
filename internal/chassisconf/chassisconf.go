// Package chassisconf runs host commands for the Linux providers.
package chassisconf

import (
	"fmt"
	"os/exec"
)

func Run(name string, args ...string) error {
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return nil
}
