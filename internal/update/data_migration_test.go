package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

func TestDataMigrationInstallerHandoff(t *testing.T) {
	pool := updateTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := New(Config{Repository: "owner/repo", InstallDir: t.TempDir(), DataDir: t.TempDir(), Version: "v2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	s.Operations, s.DataMigration = queries.New(pool), "test-data-migration"
	var calls, starts atomic.Int64
	s.start = func(context.Context) error { starts.Add(1); return nil }
	stage := func(state, phase string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE operations SET state=$1,phase=$2,payload='{"version":"v2.0.0"}',error=NULL WHERE id='system-update'`, state, phase); err != nil {
			t.Fatal(err)
		}
	}
	status := func(phase api.UpdateActivityPhase) {
		t.Helper()
		value, err := s.Status(ctx)
		if err != nil || value.Activity == nil || value.Activity.Phase != phase {
			t.Fatalf("update status=%+v, want %s: %v", value.Activity, phase, err)
		}
	}
	prepare := func(ctx context.Context) error {
		calls.Add(1)
		_, err := pool.Exec(ctx, "INSERT INTO schema_migrations(name) VALUES($1)", s.DataMigration)
		return err
	}
	installer, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer installer.Close(context.Background())
	if _, err = installer.Exec(ctx, "SELECT pg_advisory_lock(73421494)"); err != nil {
		t.Fatal(err)
	}
	stage("running", "updating")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.Initialize(ctx, pool, func(ctx context.Context) error {
			close(entered)
			select {
			case <-release:
				return prepare(ctx)
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	// The old installer unconditionally commits success before releasing its lock.
	stage("succeeded", "succeeded")
	if err = os.MkdirAll(filepath.Join(s.cfg.DataDir, "update"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = writeActivity(filepath.Join(s.cfg.DataDir, "update"), s.cfg.Version, "succeeded", nil); err != nil {
		t.Fatal(err)
	}
	status("installing")
	select {
	case <-entered:
		t.Fatal("data migration overlapped the old installer")
	default:
	}
	if err = installer.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("data migration did not take over the released installer lock")
	}
	status("installing")
	if err = s.Apply(ctx, s.cfg.Version); err != nil || starts.Load() != 0 {
		t.Fatalf("repeated update interrupted active initialization: %d %v", starts.Load(), err)
	}
	other := make(chan error, 1)
	go func() { other <- s.Initialize(ctx, pool, prepare) }()
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = <-other; err != nil || calls.Load() != 1 {
		t.Fatalf("concurrent controller repeated initialization: %d %v", calls.Load(), err)
	}
	status("succeeded")
	if err = s.Initialize(ctx, pool, prepare); err != nil || calls.Load() != 1 {
		t.Fatalf("completed migration repeated preparation: %d %v", calls.Load(), err)
	}
	// Simulate a process stopping after the marker commit but before the terminal write.
	stage("running", dataMigrationPhase)
	status("installing")
	if err = s.Initialize(ctx, pool, prepare); err != nil || calls.Load() != 1 {
		t.Fatalf("committed migration was prepared again: %d %v", calls.Load(), err)
	}
	status("succeeded")
	if _, err = pool.Exec(ctx, "DELETE FROM schema_migrations WHERE name=$1", s.DataMigration); err != nil {
		t.Fatal(err)
	}
	stage("running", "updating")
	failure := errors.New("volume metadata could not be read")
	failedPrepare := func(context.Context) error { calls.Add(1); return failure }
	if err = s.Initialize(ctx, pool, failedPrepare); !errors.Is(err, failure) {
		t.Fatalf("data preparation failure was lost: %v", err)
	}
	status("failed")
	if err = s.Initialize(ctx, pool, failedPrepare); err == nil || calls.Load() != 2 {
		t.Fatalf("failed migration retried without an update request: %d %v", calls.Load(), err)
	}
	if err = s.Apply(ctx, s.cfg.Version); err != nil || starts.Load() != 1 {
		t.Fatalf("same-version retry did not start the existing updater: %d %v", starts.Load(), err)
	}
	finish, err := Rollout(ctx, pool, nil, filepath.Join(s.cfg.DataDir, "update"), s.cfg.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err = finish(nil); err != nil {
		t.Fatal(err)
	}
	status("installing")
	if err = s.Initialize(ctx, pool, prepare); err != nil || calls.Load() != 3 {
		t.Fatalf("requested retry did not finish the data migration: %d %v", calls.Load(), err)
	}
	status("succeeded")
}

func TestDataMigrationRequiresCommittedMarker(t *testing.T) {
	pool := updateTestPool(t)
	ctx := context.Background()
	s, err := New(Config{Repository: "owner/repo", DataDir: t.TempDir(), Version: "v2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	s.Operations, s.DataMigration = queries.New(pool), "test-data-migration"
	if err = s.Initialize(ctx, pool, func(context.Context) error { return nil }); err == nil {
		t.Fatal("uncommitted migration was reported as successful")
	}
	status, err := s.Status(ctx)
	if err != nil || status.Activity == nil || status.Activity.Phase != "failed" || status.Activity.Error == nil {
		t.Fatalf("missing data initialization failure: %+v %v", status.Activity, err)
	}
}

func TestInterruptedDataMigrationResumes(t *testing.T) {
	pool := updateTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := New(Config{Repository: "owner/repo", DataDir: t.TempDir(), Version: "v2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	s.Operations, s.DataMigration = queries.New(pool), "test-data-migration"
	interrupted, stop := context.WithCancel(ctx)
	entered, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.Initialize(interrupted, pool, func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stop()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	operation, err := s.Operations.GetOperation(ctx, "system-update")
	if err != nil || operation.State != "running" || operation.Phase != dataMigrationPhase {
		t.Fatalf("interrupted initialization lost resumable progress: %+v %v", operation, err)
	}
	if err = s.Initialize(ctx, pool, func(ctx context.Context) error {
		_, err := pool.Exec(ctx, "INSERT INTO schema_migrations(name) VALUES($1)", s.DataMigration)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	operation, err = s.Operations.GetOperation(ctx, "system-update")
	if err != nil || operation.State != "succeeded" || operation.Phase != "succeeded" {
		t.Fatalf("interrupted initialization did not resume: %+v %v", operation, err)
	}
}
