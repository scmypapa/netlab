package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
)

const dataMigrationPhase = "data-migration"

// Initialize takes over the existing update after its installer has released the
// rollout lock. HTTP can serve the update status while business workers wait.
func (s *Service) Initialize(ctx context.Context, pool *pgxpool.Pool, prepare func(context.Context) error) error {
	if s.DataMigration == "" {
		return nil
	}
	connection, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer connection.Close(context.Background())
	if _, err = connection.Exec(ctx, "SELECT pg_advisory_lock(73421494)"); err != nil {
		return err
	}
	q := queries.New(connection)
	operation, err := q.GetOperation(ctx, "system-update")
	if err != nil {
		return err
	}
	applied, err := q.SchemaMigrationApplied(ctx, s.DataMigration)
	if err != nil || applied && operation.Phase != dataMigrationPhase {
		return err
	}
	if operation.State == "failed" {
		if operation.Error != nil {
			return errors.New(*operation.Error)
		}
		return fmt.Errorf("数据初始化失败，请重试本次更新")
	}
	var target api.ApplySystemUpdate
	if err = json.Unmarshal(operation.Payload, &target); err != nil {
		return err
	}
	if operation.State != "succeeded" && target.Version != s.cfg.Version {
		return ErrConflict
	}
	if count, err := q.StartDataMigration(ctx, s.cfg.Version); err != nil {
		return err
	} else if count != 1 {
		return ErrConflict
	}
	if !applied {
		err = prepare(ctx)
		if err == nil {
			applied, err = q.SchemaMigrationApplied(ctx, s.DataMigration)
			if err == nil && !applied {
				err = fmt.Errorf("数据初始化未提交迁移标记 %s", s.DataMigration)
			}
		}
	}
	if ctx.Err() != nil {
		return errors.Join(err, ctx.Err())
	}
	state := "succeeded"
	var detail *string
	if err != nil {
		state = "failed"
		message := err.Error()
		detail = &message
	}
	count, saveErr := q.FinishDataMigration(ctx, queries.FinishDataMigrationParams{Version: s.cfg.Version, State: state, Error: detail})
	if saveErr == nil && count != 1 {
		saveErr = ErrConflict
	}
	return errors.Join(err, saveErr)
}
