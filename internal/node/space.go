package node

import "fmt"

var statfsFree = freeBytes

func checkSpace(dir string, need int64) error {
	free, err := statfsFree(dir)
	if err != nil {
		return err
	}
	if free < need {
		return fmt.Errorf("not enough space on node: %d MiB free, %d MiB needed", free>>20, need>>20)
	}
	return nil
}
