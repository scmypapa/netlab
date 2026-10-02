//go:build linux

package engine

import (
	"fmt"
	"os"

	"netlab.local/core/api"
)

func removeVolumeFiles(volumes *[]api.Volume, references map[string]bool, pathFor func(string) string, requireUnused bool) error {
	if volumes == nil {
		return nil
	}
	for _, volume := range *volumes {
		if volume.Retain != nil && *volume.Retain {
			continue
		}
		path := pathFor(volume.Id)
		if references[path] {
			if requireUnused {
				return fmt.Errorf("volume %s is still attached to an instance", volume.Id)
			}
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}
