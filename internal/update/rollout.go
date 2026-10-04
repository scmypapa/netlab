package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/transport"
)

// The session lock serializes updaters; the operation keeps claims suspended
// across process or connection loss until that release completes.
func Rollout(ctx context.Context, pool *pgxpool.Pool, client *transport.Client, directory, version string) (func(error) error, error) {
	connection, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	release := func() { connection.Close(context.Background()) }
	var owned bool
	if err := connection.QueryRow(ctx, "SELECT pg_try_advisory_lock(73421494)").Scan(&owned); err != nil {
		release()
		return nil, err
	}
	if !owned {
		release()
		return nil, fmt.Errorf("另一控制面正在更新")
	}
	result, err := connection.Exec(ctx, `UPDATE operations SET payload=jsonb_build_object('version',$1::text),state='running',phase='updating',error=NULL,updated_at=now()
	 WHERE id='system-update' AND (state='succeeded' OR payload->>'version'=$1)`, version)
	if err != nil {
		release()
		return nil, err
	}
	if result.RowsAffected() != 1 {
		release()
		return nil, fmt.Errorf("请先继续完成已中断的发布版本")
	}
	finish := func(failure error) error {
		defer release()
		state := "succeeded"
		var detail *string
		if failure != nil {
			state = "failed"
			message := failure.Error()
			detail = &message
		}
		complete, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := connection.Exec(complete, "UPDATE operations SET state=$1,phase=$1,error=$2,updated_at=now() WHERE id='system-update'", state, detail)
		return err
	}
	if err := rollout(ctx, pool, connection, client, directory, version); err != nil {
		return nil, errors.Join(err, finish(err))
	}
	return finish, nil
}

func rollout(ctx context.Context, pool *pgxpool.Pool, connection *pgx.Conn, client *transport.Client, directory, version string) error {
	if err := writeActivity(directory, version, "waiting_operations", nil); err != nil {
		return err
	}
	drain, stop := context.WithTimeout(ctx, 5*time.Minute)
	defer stop()
	for {
		var active int
		if err := connection.QueryRow(drain, "SELECT count(*) FROM operations WHERE state='running' AND scope_kind<>'system'").Scan(&active); err != nil {
			return err
		}
		if active == 0 {
			break
		}
		select {
		case <-drain.Done():
			return fmt.Errorf("%d 个执行任务未结束，更新已停止：%w", active, drain.Err())
		case <-time.After(time.Second):
		}
	}
	q := queries.New(pool)
	nodes, err := q.ListNodes(ctx)
	if err != nil {
		return err
	}
	for index, node := range nodes {
		name := node.Name
		activity := api.UpdateActivity{Version: version, Phase: "updating_nodes", UpdatedAt: time.Now().UTC(), NodeName: &name, CompletedNodes: index, TotalNodes: len(nodes)}
		if err := saveActivity(directory, activity); err != nil {
			return err
		}
		if err := connection.Ping(ctx); err != nil {
			return err
		}
		if err := updateNode(ctx, client, node.Endpoint, version, connection.Ping); err != nil {
			return fmt.Errorf("更新节点 %s：%w", name, err)
		}
		info, err := client.Info(ctx, node.Endpoint)
		if err != nil {
			return err
		}
		if info.Id != node.ID || info.Version != version {
			return fmt.Errorf("节点 %s 的实际身份或版本与更新目标不一致", name)
		}
		raw, err := json.Marshal(info)
		if err != nil {
			return err
		}
		if err := q.PutNode(ctx, queries.PutNodeParams{ID: node.ID, Name: node.Name, Endpoint: node.Endpoint, Info: raw}); err != nil {
			return err
		}
	}
	return nil
}

func updateNode(ctx context.Context, client *transport.Client, endpoint, version string, keepLease func(context.Context) error) error {
	call, cancel := context.WithTimeout(ctx, 35*time.Second)
	var status api.SystemUpdate
	err := client.Do(call, http.MethodGet, endpoint, "/node/v1/system/update", nil, &status)
	cancel()
	if err != nil {
		return err
	}
	if status.CurrentVersion == version {
		return nil
	}
	if !status.CanApply {
		return fmt.Errorf("节点尚未以正式发布包安装")
	}
	if status.Activity == nil || status.Activity.Version != version || status.Activity.Phase == "failed" || status.Activity.Phase == "succeeded" {
		call, cancel = context.WithTimeout(ctx, 35*time.Second)
		err = client.Do(call, http.MethodPost, endpoint, "/node/v1/system/update", api.ApplySystemUpdate{Version: version}, nil)
		cancel()
		if err != nil {
			return err
		}
	}
	ctx, cancel = context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	for {
		if err := keepLease(ctx); err != nil {
			return err
		}
		call, stop := context.WithTimeout(ctx, 3*time.Second)
		err := client.Do(call, http.MethodGet, endpoint, "/node/v1/system/update", nil, &status)
		stop()
		if err == nil {
			if status.CurrentVersion == version && status.Activity != nil && status.Activity.Version == version && status.Activity.Phase == "succeeded" {
				return nil
			}
			if status.Activity != nil && status.Activity.Version == version && status.Activity.Phase == "failed" {
				if status.Activity.Error != nil {
					return fmt.Errorf("%s", *status.Activity.Error)
				}
				return fmt.Errorf("节点更新失败")
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待节点切换到 %s：%w", version, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
