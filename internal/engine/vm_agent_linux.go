//go:build linux

package engine

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

//go:embed guest_netplan.py
var guestNetplan string

func guestAgentCommand(domain *libvirt.Domain, command string, arguments, output any) error {
	request, err := json.Marshal(struct {
		Execute   string `json:"execute"`
		Arguments any    `json:"arguments,omitempty"`
	}{command, arguments})
	if err != nil {
		return err
	}
	text, err := domain.QemuAgentCommand(string(request), libvirt.DOMAIN_QEMU_AGENT_COMMAND_DEFAULT, 0)
	if err != nil {
		return err
	}
	var response struct {
		Return json.RawMessage `json:"return"`
		Error  *struct {
			Desc string `json:"desc"`
		} `json:"error"`
	}
	if err = json.Unmarshal([]byte(text), &response); err != nil {
		return err
	}
	if response.Error != nil {
		return errors.New(response.Error.Desc)
	}
	return json.Unmarshal(response.Return, output)
}

func guestAgentConnected(domain *libvirt.Domain) (bool, error) {
	text, err := domain.GetXMLDesc(0)
	if err != nil {
		return false, err
	}
	var config libvirtxml.Domain
	if err = config.Unmarshal(text); err != nil {
		return false, err
	}
	for _, channel := range config.Devices.Channels {
		if channel.Target != nil && channel.Target.VirtIO != nil && channel.Target.VirtIO.Name == "org.qemu.guest_agent.0" && channel.Target.VirtIO.State == "connected" {
			return true, nil
		}
	}
	return false, nil
}

func (v *VirtualMachines) bindGuestNetwork(ctx context.Context, domain *libvirt.Domain, a api.AssetExecution) error {
	if !strings.EqualFold(a.Template.Os, "linux") || initializationMethod(a.Template) != api.None || a.Template.Hardware.GuestAgent == nil || !*a.Template.Hardware.GuestAgent {
		return nil
	}
	// Installation media boots an installer, before the declared guest agent exists.
	if a.Template.Media != nil {
		for _, media := range *a.Template.Media {
			if media.Id == "installer" {
				return nil
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ready := make(chan struct{}, 1)
	callback, err := v.conn.DomainEventAgentLifecycleRegister(domain, func(_ *libvirt.Connect, _ *libvirt.Domain, event *libvirt.DomainEventAgentLifecycle) {
		if event.State == libvirt.CONNECT_DOMAIN_EVENT_AGENT_LIFECYCLE_STATE_CONNECTED {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
	})
	if err != nil {
		return err
	}
	defer v.conn.DomainEventDeregister(callback)
	connected, err := guestAgentConnected(domain)
	if err != nil {
		return err
	}
	if !connected {
		select {
		case <-ready:
		case <-ctx.Done():
			return fmt.Errorf("guest network initialization: waiting for QGA: %w", ctx.Err())
		}
	}
	interfaces, err := json.Marshal(a.Interfaces)
	if err != nil {
		return err
	}
	var process struct {
		PID int `json:"pid"`
	}
	err = guestAgentCommand(domain, "guest-exec", map[string]any{
		"path": "/bin/sh", "arg": []string{"-c", `if [ -d /etc/netplan ]; then exec /usr/bin/python3 -c "$1" "$2"; fi`, "netlab-network", guestNetplan, string(interfaces)}, "capture-output": true,
	}, &process)
	if err != nil {
		return fmt.Errorf("guest network initialization: %w", err)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var status struct {
			Exited    bool   `json:"exited"`
			ExitCode  *int   `json:"exitcode"`
			Signal    int    `json:"signal"`
			ErrorData string `json:"err-data"`
		}
		if err = guestAgentCommand(domain, "guest-exec-status", map[string]int{"pid": process.PID}, &status); err != nil {
			return err
		}
		if status.Exited {
			if status.ExitCode == nil {
				return fmt.Errorf("guest network initialization terminated by signal %d", status.Signal)
			}
			if *status.ExitCode == 0 {
				return nil
			}
			detail, err := base64.StdEncoding.DecodeString(status.ErrorData)
			if err != nil {
				return err
			}
			return fmt.Errorf("guest network initialization exited %d: %s", *status.ExitCode, strings.TrimSpace(string(detail)))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest network initialization: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
