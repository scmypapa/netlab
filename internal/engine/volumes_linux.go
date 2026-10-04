//go:build linux

package engine

import (
	"fmt"

	"netlab.local/core/api"
)

func removeVolumeFiles(volumes *[]api.Volume, references map[string]bool, pathFor func(string) string, remove func(string) error, requireUnused bool) error {
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
		if err := remove(volume.Id); err != nil {
			return err
		}
	}
	return nil
}
