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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/internal/engine"
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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	executor, err := engine.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer executor.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /node/v1/info", func(w http.ResponseWriter, r *http.Request) { value, err := executor.Info(); respond(w, value, err) })
	mux.HandleFunc("GET /node/v1/inventory", func(w http.ResponseWriter, r *http.Request) {
		value, err := executor.Inventory(r.Context(), r.URL.Query().Get("environmentId"))
		respond(w, value, err)
	})
	mux.HandleFunc("POST /node/v1/templates/prepare", func(w http.ResponseWriter, r *http.Request) {
		var template api.Template
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&template); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !pathID(template.Id) {
			http.Error(w, "invalid template identity", http.StatusBadRequest)
			return
		}
		prepared, err := executor.PrepareTemplate(r.Context(), template)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
		}
		respond(w, prepared, nil)
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
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 10 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}}
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
	for _, id := range []string{p.OperationId, p.EnvironmentId} {
		if _, err := uuid.Parse(id); err != nil {
			return fmt.Errorf("invalid plan identity")
		}
	}
	for _, a := range p.Assets {
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
