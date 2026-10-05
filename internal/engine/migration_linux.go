//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/coder/websocket"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
	"netlab.local/core/internal/stream"
)

// Libvirt stays on its Unix socket; native migration RPC and memory use the node mTLS connection.
func (e *Engine) LibvirtTunnel(w http.ResponseWriter, r *http.Request) {
	if e.vm == nil {
		http.Error(w, "virtual machine runtime is not configured", http.StatusConflict)
		return
	}
	uri, err := url.Parse(e.cfg.LibvirtURI)
	if err != nil || uri.Scheme != "qemu" || uri.Path != "/system" {
		http.Error(w, "migration requires the local system libvirt connection", http.StatusConflict)
		return
	}
	path := uri.Query().Get("socket")
	if path == "" {
		path = "/run/libvirt/libvirt-sock"
	}
	connection, err := (&net.Dialer{}).DialContext(r.Context(), "unix", path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer connection.Close()
	socket, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer socket.CloseNow()
	stream.Console(r.Context(), socket, connection, nil)
}

func (e *Engine) migrationConnection(ctx context.Context, endpoint string) (string, func(), error) {
	remote, err := url.Parse(endpoint)
	if err != nil || remote.Scheme != "https" || remote.Host == "" || e.cfg.ArtifactHTTP == nil {
		return "", nil, errors.New("migration requires a node mTLS endpoint")
	}
	remote.Scheme, remote.Path, remote.RawQuery = "wss", "/node/v1/libvirt", ""
	directory, err := os.MkdirTemp(e.cfg.DataDir, "migration-")
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(directory, "libvirt.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		os.RemoveAll(directory)
		return "", nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Go(func() {
				defer connection.Close()
				socket, response, err := websocket.Dial(ctx, remote.String(), &websocket.DialOptions{HTTPClient: e.cfg.ArtifactHTTP})
				if err != nil {
					if response != nil {
						response.Body.Close()
					}
					return
				}
				defer socket.CloseNow()
				stream.Console(ctx, socket, connection, nil)
			})
		}
	})
	close := func() {
		cancel()
		listener.Close()
		workers.Wait()
		os.RemoveAll(directory)
	}
	return "qemu+unix:///system?socket=" + url.QueryEscape(path), close, nil
}

func (e *Engine) migrateVM(ctx context.Context, env string, a api.AssetExecution, destination api.NodeVMMigration) (api.ExecutionResult, error) {
	if e.vm == nil {
		return executionResult(a, "unknown", nil), errors.New("virtual machine runtime is not configured")
	}
	var config libvirtxml.Domain
	if err := config.Unmarshal(destination.DomainXml); err != nil {
		return executionResult(a, "unknown", nil), err
	}
	var owner Ownership
	if config.Metadata == nil || config.UUID != a.InstanceId {
		return executionResult(a, "unknown", nil), errors.New("migration destination identity does not match source")
	}
	if err := xml.Unmarshal([]byte(config.Metadata.XML), &owner); err != nil {
		return executionResult(a, "unknown", nil), err
	}
	if owner.Environment != env || owner.Asset != a.Asset.Id || owner.Instance != a.InstanceId {
		return executionResult(a, "unknown", nil), errors.New("migration destination ownership does not match source")
	}
	var execution api.AssetExecution
	if err := json.Unmarshal([]byte(owner.Execution), &execution); err != nil {
		return executionResult(a, "unknown", nil), err
	}
	if a.Rbd == nil || execution.Rbd == nil || a.Rbd.Fsid != execution.Rbd.Fsid || a.Rbd.Pool != execution.Rbd.Pool || a.DataSetId != execution.DataSetId {
		return executionResult(a, "unknown", nil), errors.New("live migration requires the same shared disks and data set")
	}
	uri, close, err := e.migrationConnection(ctx, destination.Endpoint)
	if err != nil {
		return executionResult(a, "unknown", nil), err
	}
	defer close()
	target, err := libvirt.NewConnect(uri)
	if err != nil {
		return executionResult(a, "unknown", nil), err
	}
	defer target.Close()
	inspect := func() (api.ExecutionResult, error) {
		domain, err := target.LookupDomainByUUIDString(a.InstanceId)
		if err != nil {
			return executionResult(a, "unknown", nil), err
		}
		defer domain.Free()
		if _, err = e.vm.owned(domain, env, a.Asset.Id); err != nil {
			return executionResult(a, "unknown", nil), err
		}
		state, err := vmState(domain)
		observed, observeErr := e.vm.observedExecution(ctx, domain)
		result := executionResult(a, state, nil)
		result.Execution = observed
		if err == nil && state != "running" && state != "suspended" {
			err = fmt.Errorf("migration destination state is %s", state)
		}
		return result, errors.Join(err, observeErr)
	}
	domain, err := e.vm.conn.LookupDomainByUUIDString(a.InstanceId)
	if noDomain(err) {
		return inspect()
	}
	if err != nil {
		return executionResult(a, "unknown", nil), err
	}
	defer domain.Free()
	if _, err = e.vm.owned(domain, env, a.Asset.Id); err != nil {
		return executionResult(a, "unknown", nil), err
	}
	text, err := domain.GetXMLDesc(libvirt.DOMAIN_XML_MIGRATABLE)
	if err != nil {
		return executionResult(a, "unknown", nil), err
	}
	var source libvirtxml.Domain
	if err = source.Unmarshal(text); err != nil {
		return executionResult(a, "unknown", nil), err
	}
	disks := map[string]string{}
	for _, disk := range source.Devices.Disks {
		if disk.Device == "disk" {
			if disk.Source == nil || disk.Source.Network == nil || disk.Source.Network.Protocol != "rbd" {
				return executionResult(a, "unknown", nil), errors.New("live migration requires shared VM disks")
			}
			disks[disk.Serial] = disk.Source.Network.Name
		}
	}
	for _, disk := range config.Devices.Disks {
		if disk.Device == "disk" {
			if disk.Source == nil || disk.Source.Network == nil || disk.Source.Network.Protocol != "rbd" || disks[disk.Serial] != disk.Source.Network.Name {
				return executionResult(a, "unknown", nil), errors.New("migration destination disks do not match source")
			}
			delete(disks, disk.Serial)
		}
	}
	if len(disks) != 0 {
		return executionResult(a, "unknown", nil), errors.New("migration destination is missing VM disks")
	}
	job, err := domain.GetJobStats(0)
	if err != nil {
		return executionResult(a, "unknown", nil), err
	}
	if job.Type != libvirt.DOMAIN_JOB_NONE {
		return executionResult(a, "unknown", nil), errors.New("virtual machine already has a native job in progress")
	}
	flags := libvirt.MIGRATE_LIVE | libvirt.MIGRATE_PEER2PEER | libvirt.MIGRATE_TUNNELLED | libvirt.MIGRATE_PERSIST_DEST | libvirt.MIGRATE_UNDEFINE_SOURCE
	parameters := libvirt.DomainMigrateParameters{DestXMLSet: true, DestXML: destination.DomainXml}
	done := make(chan error, 1)
	go func() { done <- domain.MigrateToURI3(uri, &parameters, flags) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		err = errors.Join(ctx.Err(), domain.AbortJob(), <-done)
	}
	if err != nil {
		return executionResult(a, "unknown", nil), fmt.Errorf("native VM migration: %w", err)
	}
	result, err := inspect()
	if err == nil {
		remaining, sourceErr := e.vm.conn.LookupDomainByUUIDString(a.InstanceId)
		if sourceErr == nil {
			remaining.Free()
			err = errors.New("migration source domain remains defined")
		} else if !noDomain(sourceErr) {
			err = sourceErr
		}
	}
	return result, err
}
