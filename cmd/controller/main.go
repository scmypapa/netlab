package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/db"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
	"netlab.local/core/internal/operation"
	"netlab.local/core/internal/server"
	"netlab.local/core/internal/transport"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("controller stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	address := os.Getenv("NETLAB_LISTEN")
	if address == "" {
		address = ":8090"
	}
	webDirectory := os.Getenv("NETLAB_WEB_DIR")
	if webDirectory == "" {
		webDirectory = "web/dist"
	}
	controller := server.New(pool, nodes, server.StaticFiles(webDirectory))
	worker := operation.Worker{Pool: pool, Queries: q, Client: nodes}
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); worker.Run(ctx) }()
	httpServer := &http.Server{Addr: address, Handler: controller.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	failed := make(chan error, 1)
	go func() { slog.Info("controller listening", "address", address); failed <- httpServer.ListenAndServe() }()
	select {
	case err = <-failed:
		stop()
		<-workerDone
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = httpServer.Shutdown(shutdown)
		<-workerDone
		return err
	}
}
