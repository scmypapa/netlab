package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/db"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/buildinfo"
	"netlab.local/core/internal/metrics"
	"netlab.local/core/internal/observation"
	"netlab.local/core/internal/operation"
	"netlab.local/core/internal/secret"
	"netlab.local/core/internal/server"
	"netlab.local/core/internal/transport"
	"netlab.local/core/internal/update"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("Netlab %s (%s)\n", buildinfo.Version, buildinfo.Commit)
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("controller stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	address := os.Getenv("NETLAB_LISTEN")
	if address == "" {
		address = ":8090"
	}
	dataDirectory := os.Getenv("NETLAB_DATA_DIR")
	if dataDirectory == "" {
		dataDirectory = "data"
	}
	repository := os.Getenv("NETLAB_RELEASE_REPOSITORY")
	if repository == "" {
		repository = "scmypapa/netlab"
	}
	updateConfig := update.Config{Repository: repository, Token: os.Getenv("NETLAB_GITHUB_TOKEN"), InstallDir: os.Getenv("NETLAB_INSTALL_DIR"), DataDir: dataDirectory, Version: buildinfo.Version}
	updates, err := update.New(updateConfig)
	if err != nil {
		return err
	}
	if len(os.Args) == 2 && os.Args[1] == "update" {
		return update.Execute(ctx, updateConfig, address)
	}
	connection := os.Getenv("NETLAB_DATABASE_URL")
	if connection == "" {
		return fmt.Errorf("请设置 NETLAB_DATABASE_URL")
	}
	pool, err := pgxpool.New(ctx, connection)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err = db.Migrate(ctx, pool); err != nil {
		return err
	}
	q := queries.New(pool)
	if err = (access.Service{Queries: q}).Bootstrap(ctx, os.Getenv("NETLAB_ADMIN_PASSWORD")); err != nil {
		return err
	}
	nodes, err := transport.NewClient(os.Getenv("NETLAB_NODE_CA"), os.Getenv("NETLAB_NODE_CERT"), os.Getenv("NETLAB_NODE_KEY"))
	if err != nil {
		return err
	}
	webDirectory := os.Getenv("NETLAB_WEB_DIR")
	if webDirectory == "" {
		webDirectory = "web/dist"
	}
	controller := server.New(pool, nodes, server.StaticFiles(webDirectory))
	controller.Updates = updates
	metricsURL := os.Getenv("NETLAB_METRICS_URL")
	if metricsURL == "" {
		metricsURL = "http://127.0.0.1:8428"
	}
	controller.Metrics, err = metrics.New(metricsURL)
	if err != nil {
		return err
	}
	controller.Secrets, err = secret.OpenFile(filepath.Join(dataDirectory, "secret.key"))
	if err != nil {
		return err
	}
	defer controller.Close()
	go updates.Run(ctx)
	watchAccess, err := controller.AccessUpdates(ctx)
	if err != nil {
		return err
	}
	worker := operation.Worker{Pool: pool, Queries: q, Client: nodes, Secrets: controller.Secrets}
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); worker.Run(ctx) }()
	observerDone := make(chan struct{})
	go func() {
		defer close(observerDone)
		(observation.Service{Pool: pool, Client: nodes, Metrics: controller.Metrics}).Run(ctx)
	}()
	httpServer := &http.Server{Addr: address, Handler: controller.Handler(), BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	failed := make(chan error, 2)
	go func() { failed <- watchAccess() }()
	go func() { slog.Info("controller listening", "address", address); failed <- httpServer.ListenAndServe() }()
	select {
	case err = <-failed:
	case <-ctx.Done():
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := httpServer.Shutdown(shutdown)
	<-workerDone
	<-observerDone
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, context.Canceled) {
		err = nil
	}
	return errors.Join(err, shutdownErr)
}
