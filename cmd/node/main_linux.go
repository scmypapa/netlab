//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/internal/engine"
	"netlab.local/core/internal/logfile"
	"netlab.local/core/internal/stream"
	"netlab.local/core/internal/transport"
)

func main() {
	if err := run(); err != nil {
		slog.Error("node stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	var cfg engine.Config
	var address, certificate, key, ca string
	flag.StringVar(&cfg.ID, "id", "", "node ID")
	flag.StringVar(&cfg.Name, "name", "", "node name")
	flag.StringVar(&cfg.DataDir, "data", "/var/lib/netlab", "managed storage directory")
	flag.StringVar(&cfg.ContainerdSocket, "containerd", "/run/containerd/containerd.sock", "containerd socket; empty disables containers")
	flag.StringVar(&cfg.LibvirtURI, "libvirt", "qemu:///system", "libvirt URI; empty disables VMs")
	flag.StringVar(&cfg.OVNEndpoint, "ovn", "unix:/run/ovn/ovnnb_db.sock", "OVN northbound endpoint")
	flag.StringVar(&cfg.OVSEndpoint, "ovs", "unix:/run/openvswitch/db.sock", "local OVS endpoint")
	flag.StringVar(&cfg.Bridge, "bridge", "br-int", "OVN integration bridge")
	flag.StringVar(&cfg.ProviderCIDR, "service-network", "100.127.0.0/16", "reserved IPv4 service provider network")
	flag.StringVar(&cfg.AdvertiseAddress, "advertise-address", "", "client access address; defaults to the default-route source")
	flag.StringVar(&address, "listen", ":9443", "mTLS listen address")
	flag.StringVar(&certificate, "cert", "/etc/netlab/node.crt", "node certificate")
	flag.StringVar(&key, "key", "/etc/netlab/node.key", "node private key")
	flag.StringVar(&ca, "ca", "/etc/netlab/ca.crt", "controller certificate authority")
	flag.Parse()
	if cfg.Name == "" {
		name, err := os.Hostname()
		if err != nil {
			return err
		}
		cfg.Name = name
	}
	if !filepath.IsAbs(cfg.DataDir) {
		return errors.New("data directory must be absolute")
	}
	var err error
	if cfg.ID, err = persistentNodeID(cfg.DataDir, cfg.ID); err != nil {
		return err
	}
	caPEM, err := os.ReadFile(ca)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("invalid controller CA")
	}
	client, err := transport.NewClient(ca, certificate, key)
	if err != nil {
		return err
	}
	cfg.ArtifactHTTP = client.HTTP
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	executor, err := engine.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer executor.Close()
	mux := http.NewServeMux()
	for _, pattern := range []string{
		"POST /node/v1/environments/{environmentId}/ssh/{kind}",
		"GET /node/v1/environments/{environmentId}/ssh/{kind}",
		"PUT /node/v1/environments/{environmentId}/ssh/files/content",
		"GET /node/v1/environments/{environmentId}/ssh/files/content",
	} {
		mux.HandleFunc(pattern, executor.GuestAccess)
	}
	for _, pattern := range []string{
		"GET /node/v1/environments/{environmentId}/assets/{assetId}/instances/{instanceId}/files",
		"POST /node/v1/environments/{environmentId}/assets/{assetId}/instances/{instanceId}/files",
		"GET /node/v1/environments/{environmentId}/assets/{assetId}/instances/{instanceId}/files/content",
		"PUT /node/v1/environments/{environmentId}/assets/{assetId}/instances/{instanceId}/files/content",
	} {
		mux.HandleFunc(pattern, executor.ContainerFiles)
	}
	mux.HandleFunc("POST /node/v1/backup-repositories", func(w http.ResponseWriter, r *http.Request) {
		var repository api.NodeBackupRepository
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&repository); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, err := executor.ConnectBackupRepository(r.Context(), repository, r.URL.Query().Get("initialize") == "true")
		respond(w, map[string]string{"id": id}, err)
	})
	mux.HandleFunc("POST /node/v1/backup-repositories/catalog", func(w http.ResponseWriter, r *http.Request) {
		var repository api.NodeBackupRepository
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&repository); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result, err := executor.BackupCatalog(r.Context(), repository)
		respond(w, result, err)
	})
	mux.HandleFunc("POST /node/v1/backups/templates/restore", func(w http.ResponseWriter, r *http.Request) {
		var input api.NodeRestoreBackupTemplate
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !pathID(input.Template.Id) || input.Template.Version < 1 {
			http.Error(w, "invalid template identity", http.StatusBadRequest)
			return
		}
		result, err := executor.RestoreBackupTemplate(r.Context(), input)
		respond(w, result, err)
	})
	mux.HandleFunc("POST /node/v1/backups/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, err := uuid.Parse(r.PathValue("id")); err != nil {
			http.Error(w, "invalid backup identity", http.StatusBadRequest)
			return
		}
		var plan api.NodeBackupPlan
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&plan); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := uuid.Parse(plan.RecoveryPointId); err != nil {
			http.Error(w, "invalid recovery point identity", http.StatusBadRequest)
			return
		}
		for _, source := range plan.Sources {
			if err := validateExecutionPaths(source.Execution); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		result, err := executor.BackupRecovery(r.Context(), r.PathValue("id"), plan)
		respond(w, result, err)
	})
	mux.HandleFunc("DELETE /node/v1/backups/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, err := uuid.Parse(r.PathValue("id")); err != nil {
			http.Error(w, "invalid backup identity", http.StatusBadRequest)
			return
		}
		var repository api.NodeBackupRepository
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&repository); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := executor.DeleteBackup(r.Context(), r.PathValue("id"), repository); err != nil {
			respond(w, nil, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /node/v1/backups/assets/{assetId}/artifact", func(w http.ResponseWriter, r *http.Request) {
		if !pathID(r.PathValue("assetId")) {
			http.Error(w, "invalid backup asset identity", http.StatusBadRequest)
			return
		}
		var source api.NodeBackupSource
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&source); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reader, size := executor.OpenBackupArtifact(r.Context(), r.PathValue("assetId"), source)
		defer reader.Close()
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		if _, err := io.Copy(w, reader); err != nil {
			slog.Warn("backup transfer interrupted", "error", err)
		}
	})
	mux.HandleFunc("GET /node/v1/storage", func(w http.ResponseWriter, r *http.Request) {
		info, err := engine.StorageInfo(r.URL.Query().Get("path"))
		respond(w, info, err)
	})
	mux.HandleFunc("POST /node/v1/storage/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !pathID(r.PathValue("id")) {
			http.Error(w, "invalid storage identity", http.StatusBadRequest)
			return
		}
		var input api.CreateStoragePool
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		info, err := executor.RegisterStorage(r.PathValue("id"), input.Directory)
		respond(w, info, err)
	})
	mux.HandleFunc("DELETE /node/v1/storage/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !pathID(r.PathValue("id")) {
			http.Error(w, "invalid storage identity", http.StatusBadRequest)
			return
		}
		if err := executor.RemoveStorage(r.PathValue("id"), r.URL.Query().Get("directory")); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /node/v1/environments/{environmentId}/assets/{assetId}/instances/{instanceId}/logs", func(w http.ResponseWriter, r *http.Request) {
		options, err := logfile.Parse(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reader, err := executor.OpenLogs(r.Context(), r.PathValue("environmentId"), r.PathValue("assetId"), r.PathValue("instanceId"), options)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		defer reader.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		control := http.NewResponseController(w)
		if _, err = fmt.Fprint(w, ": connected\n\n"); err != nil {
			return
		}
		if err = control.Flush(); err != nil {
			return
		}
		err = reader.Read(r.Context(), func(chunk api.LogChunk) error {
			raw, err := json.Marshal(chunk)
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintf(w, "event: output\ndata: %s\n\n", raw); err != nil {
				return err
			}
			return control.Flush()
		})
		if err != nil && r.Context().Err() == nil {
			raw, _ := json.Marshal(api.Problem{Status: 500, Title: "Log stream failed", Detail: err.Error()})
			fmt.Fprintf(w, "event: stream-error\ndata: %s\n\n", raw)
			control.Flush()
		}
	})
	mux.HandleFunc("GET /node/v1/observations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		response := http.NewResponseController(w)
		started := false
		err := executor.Observe(r.Context(), func(value api.NodeObservation) error {
			started = true
			if err := response.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
				return err
			}
			if err := json.NewEncoder(w).Encode(value); err != nil {
				return err
			}
			return response.Flush()
		})
		if err != nil && r.Context().Err() == nil {
			slog.Warn("node observation stream closed", "error", err)
			if !started {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
			}
		}
	})
	mux.HandleFunc("GET /node/v1/environments/{environmentId}/assets/{assetId}/instances/{instanceId}/console", func(w http.ResponseWriter, r *http.Request) {
		console, err := executor.OpenConsole(r.Context(), r.PathValue("environmentId"), r.PathValue("assetId"), r.PathValue("instanceId"), api.ConsoleKind(r.URL.Query().Get("kind")))
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		defer console.Close()
		socket, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
		if err != nil {
			return
		}
		defer socket.CloseNow()
		socket.SetReadLimit(1 << 20)
		stream.Console(r.Context(), socket, console, console.Resize)
	})
	mux.HandleFunc("GET /node/v1/info", func(w http.ResponseWriter, r *http.Request) { value, err := executor.Info(); respond(w, value, err) })
	mux.HandleFunc("GET /node/v1/inventory", func(w http.ResponseWriter, r *http.Request) {
		value, err := executor.Inventory(r.Context(), r.URL.Query().Get("environmentId"))
		respond(w, value, err)
	})
	mux.HandleFunc("POST /node/v1/templates/prepare", func(w http.ResponseWriter, r *http.Request) {
		var request api.NodeTemplatePreparation
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !pathID(request.Template.Id) || request.Template.Version < 1 {
			http.Error(w, "invalid template identity", http.StatusBadRequest)
			return
		}
		prepared, err := executor.PrepareTemplate(r.Context(), request)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
		}
		respond(w, prepared, nil)
	})
	mux.HandleFunc("POST /node/v1/templates/{id}/versions/{version}/imports/{uploadId}", func(w http.ResponseWriter, r *http.Request) {
		version, err := strconv.Atoi(r.PathValue("version"))
		if err != nil || version < 1 || !pathID(r.PathValue("id")) || !pathID(r.PathValue("uploadId")) {
			http.Error(w, "invalid template identity", http.StatusBadRequest)
			return
		}
		parts, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		source, err := executor.ReceiveTemplate(r.PathValue("id"), version, r.PathValue("uploadId"), r.URL.Query().Get("source"), parts)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		respond(w, source, nil)
	})
	mux.HandleFunc("DELETE /node/v1/templates/{id}/versions/{version}/imports/{uploadId}", func(w http.ResponseWriter, r *http.Request) {
		version, err := strconv.Atoi(r.PathValue("version"))
		if err != nil || version < 1 || !pathID(r.PathValue("id")) || !pathID(r.PathValue("uploadId")) {
			http.Error(w, "invalid template identity", http.StatusBadRequest)
			return
		}
		if err = executor.RemoveTemplateImport(r.PathValue("id"), version, r.PathValue("uploadId")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /node/v1/templates/{id}/versions/{version}/artifact", func(w http.ResponseWriter, r *http.Request) {
		version, err := strconv.Atoi(r.PathValue("version"))
		if err != nil || version < 1 || !pathID(r.PathValue("id")) {
			http.Error(w, "invalid template identity", http.StatusBadRequest)
			return
		}
		reader, length, err := executor.OpenTemplateArtifact(r.PathValue("id"), version)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, os.ErrNotExist) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		defer reader.Close()
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		if _, err = io.Copy(w, reader); err != nil {
			slog.Warn("template artifact transfer interrupted", "template", r.PathValue("id"), "error", err)
		}
	})
	mux.HandleFunc("POST /node/v1/environments/{environmentId}/recovery-points/{pointId}/assets/{assetId}/artifact", func(w http.ResponseWriter, r *http.Request) {
		for _, id := range []string{r.PathValue("environmentId"), r.PathValue("pointId")} {
			if _, err := uuid.Parse(id); err != nil {
				http.Error(w, "invalid recovery identity", http.StatusBadRequest)
				return
			}
		}
		var execution api.AssetExecution
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&execution); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := validateExecutionPaths(execution); err != nil || execution.Asset.Id != r.PathValue("assetId") {
			http.Error(w, "invalid recovery asset identity", http.StatusBadRequest)
			return
		}
		reader, length, err := executor.OpenRecoveryArtifact(r.PathValue("environmentId"), r.PathValue("pointId"), execution)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		defer reader.Close()
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		if _, err = io.Copy(w, reader); err != nil {
			slog.Warn("recovery transfer interrupted", "error", err)
		}
	})
	mux.HandleFunc("DELETE /node/v1/templates/{id}/versions/{version}", func(w http.ResponseWriter, r *http.Request) {
		version, err := strconv.Atoi(r.PathValue("version"))
		if err != nil || version < 1 || !pathID(r.PathValue("id")) {
			http.Error(w, "invalid template identity", http.StatusBadRequest)
			return
		}
		if err = executor.RemoveTemplate(r.Context(), r.PathValue("id"), version); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /node/v1/plans", func(w http.ResponseWriter, r *http.Request) {
		var plan api.NodePlan
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&plan); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := validatePlanPaths(plan); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		respond(w, executor.Execute(r.Context(), plan), nil)
	})
	server := &http.Server{Addr: address, Handler: mux, BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 10 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}}
	finished := make(chan error, 1)
	go func() { finished <- server.ListenAndServeTLS(certificate, key) }()
	select {
	case err = <-finished:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
	return nil
}
func respond(w http.ResponseWriter, value any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(api.NodeResult{Results: []api.ExecutionResult{}, Error: ptr(err.Error())})
		return
	}
	if err = json.NewEncoder(w).Encode(value); err != nil {
		slog.Warn("response interrupted", "error", err)
	}
}
func ptr[T any](v T) *T { return &v }
func validatePlanPaths(p api.NodePlan) error {
	if p.RecoveryPointId != nil {
		if _, err := uuid.Parse(*p.RecoveryPointId); err != nil {
			return fmt.Errorf("invalid recovery point identity")
		}
	}
	for _, id := range []string{p.OperationId, p.EnvironmentId} {
		if _, err := uuid.Parse(id); err != nil {
			return fmt.Errorf("invalid plan identity")
		}
	}
	for _, a := range p.Assets {
		if err := validateExecutionPaths(a); err != nil {
			return err
		}
		if (p.Phase == api.NodePlanPhasePrepareRecovery || p.Phase == api.NodePlanPhaseApplyRecovery) && (a.DataSetId != p.OperationId || p.RecoveryPointId == nil) {
			return errors.New("recovery target must use the operation data set")
		}
	}
	if p.Phase == api.NodePlanPhasePrepareRecovery && p.RecoverySources == nil {
		return errors.New("missing recovery sources")
	}
	if p.RecoverySources != nil {
		for id, source := range *p.RecoverySources {
			if id != source.Execution.Asset.Id {
				return errors.New("recovery source asset does not match key")
			}
			if _, err := uuid.Parse(source.EnvironmentId); err != nil {
				return errors.New("invalid recovery environment identity")
			}
			if err := validateExecutionPaths(source.Execution); err != nil {
				return err
			}
		}
	}
	return nil
}
func validateExecutionPaths(a api.AssetExecution) error {
	if a.DataSetId != "" && !pathID(a.DataSetId) {
		return errors.New("invalid data set identity")
	}
	if _, err := uuid.Parse(a.InstanceId); err != nil {
		return fmt.Errorf("invalid instance identity")
	}
	for _, id := range []string{a.Asset.Id, a.Template.Id} {
		if !pathID(id) {
			return fmt.Errorf("invalid storage identity")
		}
	}
	for _, i := range a.Interfaces {
		if _, err := uuid.Parse(i.PortName); err != nil {
			return fmt.Errorf("invalid logical port identity")
		}
	}
	if a.Asset.Volumes != nil {
		for _, v := range *a.Asset.Volumes {
			if !pathID(v.Id) {
				return fmt.Errorf("invalid volume identity")
			}
		}
	}
	return nil
}
func pathID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00")
}
func persistentNodeID(directory, override string) (string, error) {
	if err := os.MkdirAll(directory, 0711); err != nil {
		return "", err
	}
	path := filepath.Join(directory, "node-id")
	if override == "" {
		data, err := os.ReadFile(path)
		if err == nil {
			value := strings.TrimSpace(string(data))
			if _, err = uuid.Parse(value); err != nil {
				return "", fmt.Errorf("invalid stored node identity: %w", err)
			}
			return value, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		override = uuid.NewString()
	}
	if _, err := uuid.Parse(override); err != nil {
		return "", err
	}
	return override, os.WriteFile(path, []byte(override+"\n"), 0600)
}
