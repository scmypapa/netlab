//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
	"netlab.local/core/internal/stream"
)

func (e *Engine) Migration(w http.ResponseWriter, r *http.Request) {
	var value api.NodeVMMigration
	var err error
	if r.Method == http.MethodDelete {
		var input api.NodeMigrationCleanup
		if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&input); err == nil {
			a := input.Source
			if a.InstanceId != r.PathValue("instanceId") || a.Asset.Id != r.PathValue("assetId") || a.InstanceId != input.Target.InstanceId {
				err = errors.New("migration cleanup identity does not match")
			} else {
				err = e.cleanupMigration(r.Context(), r.PathValue("environmentId"), a, input.Target)
			}
		}
	} else if r.Method == http.MethodPost {
		var input api.NodeMigrationPreparation
		if err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&input); err == nil {
			value.DomainXml, err = e.prepareMigration(r.Context(), input)
		}
	} else {
		if e.vm == nil {
			http.Error(w, "virtual machine runtime is not configured", http.StatusConflict)
			return
		}
		env, asset, instance := r.PathValue("environmentId"), r.PathValue("assetId"), r.PathValue("instanceId")
		domain, readErr := e.vm.conn.LookupDomainByUUIDString(instance)
		if readErr != nil {
			err = readErr
		} else {
			defer domain.Free()
			var owner Ownership
			owner, err = e.vm.owned(domain, env, asset)
			if err == nil {
				value.DomainXml, err = domain.GetXMLDesc(libvirt.DOMAIN_XML_MIGRATABLE)
				if err == nil && r.PathValue("file") == "files" {
					var a api.AssetExecution
					if err = json.Unmarshal([]byte(owner.Execution), &a); err == nil {
						var reader io.ReadCloser
						reader, _, err = e.migrationFiles(value.DomainXml, env, a)
						if err == nil {
							defer reader.Close()
							w.Header().Set("Content-Type", "application/x-tar")
							io.Copy(w, reader)
							return
						}
					}
				}
			}
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

func sharedDisk(a, b vmDisk) bool {
	return a.rbd != nil && b.rbd != nil && a.rbd.Fsid == b.rbd.Fsid && a.rbd.Pool == b.rbd.Pool && a.image == b.image
}

func (e *Engine) cleanupMigration(ctx context.Context, env string, source, target api.AssetExecution) error {
	unlock := e.lock(env + "/" + source.Asset.Id)
	defer unlock()
	if source.Template.Kind == api.Container {
		state, err := e.container.Execute(ctx, env, api.NodePlanPhaseInspect, source)
		if state != "absent" {
			if err != nil {
				return err
			}
			if state != "stopped" && state != "prepared" && state != "created" {
				return errors.New("migration instance is still running")
			}
		}
		if _, err := e.container.Execute(ctx, env, api.NodePlanPhaseDestroy, source); err != nil {
			return err
		}
	} else {
		domain, err := e.vm.conn.LookupDomainByUUIDString(source.InstanceId)
		if err == nil {
			defer domain.Free()
			if _, err = e.vm.owned(domain, env, source.Asset.Id); err != nil {
				return err
			}
			state, err := vmState(domain)
			if err != nil {
				return err
			}
			if state != "stopped" {
				return errors.New("migration source is still running")
			}
			if err = domain.UndefineFlags(libvirt.DOMAIN_UNDEFINE_KEEP_NVRAM | libvirt.DOMAIN_UNDEFINE_KEEP_TPM); err != nil {
				return err
			}
		} else if !noDomain(err) {
			return err
		}
		destination := executionDisks(e.cfg.DataDir, env, target)
		for _, disk := range executionDisks(e.cfg.DataDir, env, source) {
			shared := false
			for _, next := range destination {
				if sharedDisk(disk, next) {
					shared = true
					break
				}
			}
			if !shared {
				if err := disk.remove(ctx); err != nil {
					return err
				}
			}
		}
		if err := os.RemoveAll(tpmDirectory(source.InstanceId)); err != nil {
			return err
		}
		if err := os.RemoveAll(assetDirectory(e.cfg.DataDir, env, source)); err != nil {
			return err
		}
	}
	for name, volume := range source.VolumeSources {
		if sharedDisk(persistentDisk(volume), persistentDisk(target.VolumeSources[name])) {
			continue
		}
		if err := e.Volume(ctx, "delete", volume); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) migrationFiles(text, env string, a api.AssetExecution) (io.ReadCloser, int64, error) {
	if a.Rbd == nil {
		return nil, 0, errors.New("native migration boot files require shared VM disks")
	}
	var config libvirtxml.Domain
	if err := config.Unmarshal(text); err != nil {
		return nil, 0, err
	}
	directory := assetDirectory(e.vm.data, env, a)
	files := []string{}
	if config.OS.NVRam != nil {
		files = append(files, filepath.Base(config.OS.NVRam.NVRam))
	}
	for _, disk := range config.Devices.Disks {
		if disk.Device == "cdrom" && disk.Source != nil && disk.Source.File != nil {
			if filepath.Dir(disk.Source.File.File) != directory {
				return nil, 0, errors.New("migration media is outside the managed instance")
			}
			files = append(files, filepath.Base(disk.Source.File.File))
		}
	}
	return openArtifactFiles(directory, files)
}

func (e *Engine) prepareMigration(ctx context.Context, input api.NodeMigrationPreparation) (string, error) {
	if e.vm == nil {
		return "", errors.New("virtual machine runtime is not configured")
	}
	a, env := input.Execution, input.EnvironmentId
	unlock := e.lock(env + "/" + a.Asset.Id)
	defer unlock()
	if existing, err := e.vm.conn.LookupDomainByUUIDString(a.InstanceId); err == nil {
		defer existing.Free()
		if _, err = e.vm.owned(existing, env, a.Asset.Id); err != nil {
			return "", err
		}
		return existing.GetXMLDesc(libvirt.DOMAIN_XML_MIGRATABLE)
	} else if !noDomain(err) {
		return "", err
	}
	var config libvirtxml.Domain
	if err := config.Unmarshal(input.DomainXml); err != nil {
		return "", err
	}
	var owner Ownership
	if config.UUID != a.InstanceId || config.Metadata == nil {
		return "", errors.New("migration identity does not match")
	}
	if err := xml.Unmarshal([]byte(config.Metadata.XML), &owner); err != nil {
		return "", err
	}
	if owner.Environment != env || owner.Asset != a.Asset.Id || owner.Instance != a.InstanceId {
		return "", errors.New("migration ownership does not match")
	}
	var original api.AssetExecution
	if err := json.Unmarshal([]byte(owner.Execution), &original); err != nil {
		return "", err
	}
	if original.Rbd == nil || a.Rbd == nil || original.Rbd.Fsid != a.Rbd.Fsid || original.Rbd.Pool != a.Rbd.Pool || original.DataSetId != a.DataSetId {
		return "", errors.New("migration requires the same shared data set")
	}
	directory := assetDirectory(e.vm.data, env, a)
	if err := os.MkdirAll(directory, 0711); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(e.cfg.DataDir, "migration-files-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(input.SourceEndpoint, "/")+"/node/v1/environments/"+env+"/assets/"+a.Asset.Id+"/instances/"+a.InstanceId+"/migration/files", nil)
	if err != nil {
		return "", err
	}
	response, err := e.cfg.ArtifactHTTP.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 16384))
		return "", fmt.Errorf("migration boot files: %d %s", response.StatusCode, message)
	}
	if err = receiveDirectoryArtifact(response.Body, staging); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return "", err
	}
	for _, file := range entries {
		if err = os.Rename(filepath.Join(staging, file.Name()), filepath.Join(directory, file.Name())); err != nil {
			return "", err
		}
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	metadata, err := xml.Marshal(Ownership{Environment: env, Asset: a.Asset.Id, Instance: a.InstanceId, Execution: string(raw)})
	if err != nil {
		return "", err
	}
	config.Metadata.XML = string(metadata)
	config.SecLabel = nil
	config.Devices.Emulator = ""
	if config.OS.NVRam != nil {
		config.OS.NVRam.NVRam = filepath.Join(directory, filepath.Base(config.OS.NVRam.NVRam))
	}
	for i := range config.Devices.Disks {
		disk := &config.Devices.Disks[i]
		if disk.Device == "cdrom" && disk.Source != nil && disk.Source.File != nil {
			disk.Source.File.File = filepath.Join(directory, filepath.Base(disk.Source.File.File))
		}
	}
	for i := range config.Devices.Interfaces {
		nic := &config.Devices.Interfaces[i]
		if nic.Source != nil && nic.Source.Bridge != nil {
			nic.Source.Bridge.Bridge = e.vm.bridge
		}
		nic.Target = nil
	}
	for i := range config.Devices.Channels {
		ch := &config.Devices.Channels[i]
		if ch.Source != nil && ch.Source.UNIX != nil {
			ch.Source.UNIX.Path = ""
		}
	}
	for i := range config.Devices.Serials {
		ch := &config.Devices.Serials[i]
		if ch.Source != nil && ch.Source.Pty != nil {
			ch.Source.Pty.Path = ""
		}
	}
	for i := range config.Devices.Consoles {
		ch := &config.Devices.Consoles[i]
		if ch.Source != nil && ch.Source.Pty != nil {
			ch.Source.Pty.Path = ""
		}
	}
	for i := range config.Devices.Graphics {
		if vnc := config.Devices.Graphics[i].VNC; vnc != nil {
			vnc.Port = 0
			vnc.AutoPort = "yes"
			vnc.Listen = "127.0.0.1"
		}
	}
	return config.Marshal()
}

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
