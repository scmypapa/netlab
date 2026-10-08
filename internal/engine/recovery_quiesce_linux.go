//go:build linux

package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"libvirt.org/go/libvirt"
	"netlab.local/core/api"
)

func recoveryFreezePath(data, point string, a api.AssetExecution) string {
	return recoveryDirectory(data, point, a) + ".freeze"
}

func guestFreezeState(domain *libvirt.Domain) (string, error) {
	var status string
	if err := guestAgentCommand(domain, "guest-fsfreeze-status", nil, &status); err != nil {
		return "", err
	}
	if status != "frozen" && status != "thawed" {
		return "", fmt.Errorf("invalid guest freeze state %q", status)
	}
	return status, nil
}

func guestRecoveryConsistency(domain *libvirt.Domain) (api.RecoveryConsistency, error) {
	var result struct {
		ID string `json:"id"`
	}
	if err := guestAgentCommand(domain, "guest-get-osinfo", nil, &result); err != nil {
		return api.Crash, err
	}
	if result.ID == "mswindows" {
		// QGA's Windows freeze completes VSS preparation, writer checks and snapshot creation.
		return api.Application, nil
	}
	return api.Filesystem, nil
}

func (v *VirtualMachines) freezeRecoveryFS(domain *libvirt.Domain, point string, a api.AssetExecution, paused bool) (bool, error) {
	connected, err := guestAgentConnected(domain)
	if err != nil {
		return false, err
	}
	if !connected {
		return false, nil
	}
	if paused {
		if err = domain.Resume(); err != nil {
			return false, err
		}
	}
	status, err := guestFreezeState(domain)
	if err != nil {
		return false, err
	}
	if status == "frozen" {
		return false, errors.New("guest filesystems are already frozen outside this capture")
	}
	consistency, err := guestRecoveryConsistency(domain)
	if err != nil {
		return false, err
	}
	path := recoveryFreezePath(v.data, point, a)
	if err = os.MkdirAll(filepath.Dir(path), 0711); err != nil {
		return false, err
	}
	// Persist ownership before changing the guest, so an interrupted task can thaw it.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false, err
	}
	_, writeErr := file.WriteString(string(consistency))
	if err = errors.Join(writeErr, file.Close()); err != nil {
		return false, err
	}
	if err = domain.FSFreeze(nil, 0); err != nil {
		return false, err
	}
	return true, domain.Suspend()
}

func (v *VirtualMachines) thawRecoveryFS(domain *libvirt.Domain, point string, a api.AssetExecution, keepPaused bool) (bool, error) {
	path := recoveryFreezePath(v.data, point, a)
	state, err := vmState(domain)
	if err != nil {
		return false, err
	}
	if state == "stopped" {
		return false, os.Remove(path)
	}
	if state == "suspended" {
		if err = domain.Resume(); err != nil {
			return false, err
		}
	}
	status, err := guestFreezeState(domain)
	if err != nil {
		return false, err
	}
	if err = domain.FSThaw(nil, 0); err != nil {
		return false, err
	}
	if keepPaused {
		if err = domain.Suspend(); err != nil {
			return false, err
		}
	}
	return status == "frozen", os.Remove(path)
}

func (v *VirtualMachines) finishRecoveryFS(env, point string, a api.AssetExecution, keepPaused bool) error {
	path := recoveryFreezePath(v.data, point, a)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	domain, err := v.conn.LookupDomainByUUIDString(a.InstanceId)
	if noDomain(err) {
		return os.Remove(path)
	}
	if err != nil {
		return err
	}
	defer domain.Free()
	if _, err = v.owned(domain, env, a.Asset.Id); err != nil {
		return err
	}
	_, err = v.thawRecoveryFS(domain, point, a, keepPaused)
	return err
}
