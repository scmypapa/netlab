//go:build linux

package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/containerd"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestMetricsRetainHostWhenContainerdUnavailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	connection, err := grpc.NewClient("unix://"+filepath.Join(t.TempDir(), "absent.sock"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	client, err := containerd.NewWithConn(connection)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	e := &Engine{cfg: Config{ID: "worker", DataDir: t.TempDir()}, container: &Containers{client: client, node: "worker"}}
	samples, err := e.Metrics(ctx)
	if err == nil {
		t.Fatal("unavailable containerd was not reported")
	}
	if len(samples) < 7 {
		t.Fatalf("successful host samples were discarded: %v", samples)
	}
	for _, sample := range samples {
		if sample.Environment != "" || sample.Node != "worker" {
			t.Fatalf("fabricated container metrics: %+v", sample)
		}
	}
}
