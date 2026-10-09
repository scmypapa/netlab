package operation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/environment"
)

func (s Service) enqueueNetworkMove(ctx context.Context, row queries.Environment, request string) (queries.Operation, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return queries.Operation{}, err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	row, err = q.LockEnvironment(ctx, row.ID)
	if err != nil {
		return queries.Operation{}, err
	}
	key := request + "/network/" + row.ID
	previous, err := q.GetOperationByRequest(ctx, queries.GetOperationByRequestParams{EnvironmentID: &row.ID, ClientRequestID: &key})
	if err == nil {
		return previous, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return queries.Operation{}, err
	}
	if pending, err := q.PendingEnvironmentOperations(ctx, row.ID); err != nil {
		return queries.Operation{}, err
	} else if pending {
		return queries.Operation{}, environment.ErrInUse
	}
	var spec api.EnvironmentSpec
	if err = json.Unmarshal(row.AppliedSpec, &spec); err != nil {
		return queries.Operation{}, err
	}
	raw, err := json.Marshal(Payload{Spec: spec, BeforeStatus: row.Status, NetworkSource: *row.NetworkNodeID})
	if err != nil {
		return queries.Operation{}, err
	}
	op, err := q.CreateOperation(ctx, queries.CreateOperationParams{ID: uuid.NewString(), EnvironmentID: &row.ID, ScopeKind: "environment", ScopeID: row.ID, Kind: "move-network", Payload: raw, ExpectedRevision: row.Revision, ClientRequestID: &key})
	if err != nil {
		return queries.Operation{}, err
	}
	if err = q.SetEnvironmentOperation(ctx, queries.SetEnvironmentOperationParams{ID: row.ID, OperationID: &op.ID, Status: "changing"}); err != nil {
		return queries.Operation{}, err
	}
	return op, tx.Commit(ctx)
}

func (w Worker) moveNetwork(ctx context.Context, op *queries.Operation, p *Payload) (failure error) {
	defer func() {
		if failure != nil && !errors.Is(failure, errPersistence) && ctx.Err() == nil {
			failure = errors.Join(failure, w.status(ctx, op, p, failure))
		}
	}()
	if op.Phase == "complete" {
		return nil
	}
	if op.Phase == "queued" {
		nodes, err := w.Queries.ListNodes(ctx)
		if err != nil {
			return err
		}
		var destination queries.ListNodesRow
		var info api.NodeInfo
		p.ExternalChassis = map[string]string{}
		for _, node := range nodes {
			var item api.NodeInfo
			if err = json.Unmarshal(node.Info, &item); err != nil {
				return err
			}
			if item.NetworkChassis != nil {
				p.ExternalChassis[node.ID] = *item.NetworkChassis
			}
			if destination.ID == "" && node.ID != p.NetworkSource && node.State == "ready" && !node.Retiring && slices.Contains(item.Capabilities, "network") && (len(environment.Services(p.Spec)) == 0 || item.ServiceNetwork != nil) {
				destination = node
				info = item
			}
		}
		if destination.ID == "" {
			return environment.Invalid("没有可接替环境入口的网络节点")
		}
		tx, err := w.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		q := w.Queries.WithTx(tx)
		if _, err = q.LockEnvironment(ctx, *op.EnvironmentID); err != nil {
			return err
		}
		locked, err := q.LockNode(ctx, destination.ID)
		if err != nil {
			return err
		}
		if locked.State != "ready" || locked.Retiring {
			return environment.ErrConflict
		}
		p.Owner = &Target{NodeID: destination.ID}
		p.BeforeSpec = &p.Spec
		if err = q.ReleaseAllAccessPorts(ctx, *op.EnvironmentID); err != nil {
			return err
		}
		if err = q.SetNetworkOwner(ctx, queries.SetNetworkOwnerParams{ID: *op.EnvironmentID, NetworkNodeID: &destination.ID}); err != nil {
			return err
		}
		if err = q.SetGatewayAddress(ctx, queries.SetGatewayAddressParams{EnvironmentID: *op.EnvironmentID}); err != nil {
			return err
		}
		if err = w.reserveServices(ctx, q, op, p, info); err != nil {
			return err
		}
		tw := w
		tw.Queries = q
		if err = tw.phase(ctx, op, p, "network-move-copy"); err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
	}
	nodes, err := w.Queries.GetNodeEndpoints(ctx, []string{p.NetworkSource, p.Owner.NodeID})
	if err != nil {
		return err
	}
	endpoints := map[string]string{}
	for _, node := range nodes {
		endpoints[node.ID] = node.Endpoint
	}
	for op.Phase != "complete" {
		next := ""
		switch op.Phase {
		case "network-move-copy":
			var key struct {
				PrivateKey string `json:"privateKey"`
			}
			path := "/node/v1/environments/" + *op.EnvironmentID + "/access-key"
			err = w.Client.Do(ctx, http.MethodGet, endpoints[p.NetworkSource], path, nil, &key)
			if err == nil {
				err = w.Client.Do(ctx, http.MethodPut, endpoints[p.Owner.NodeID], path, struct {
					PrivateKey string              `json:"privateKey"`
					Spec       api.EnvironmentSpec `json:"spec"`
				}{key.PrivateKey, p.Spec}, nil)
			}
			next = "network-move-apply"
		case "network-move-apply":
			err = w.Client.Do(ctx, http.MethodDelete, endpoints[p.NetworkSource], "/node/v1/environments/"+*op.EnvironmentID+"/local-access", nil, nil)
			if err == nil {
				actual, readErr := w.Queries.ListRuntimeAssets(ctx, *op.EnvironmentID)
				err = readErr
				p.Unchanged = nil
				for _, row := range actual {
					if err != nil {
						break
					}
					var execution api.AssetExecution
					if err = json.Unmarshal(row.Execution, &execution); err == nil && row.Current {
						p.Unchanged = append(p.Unchanged, Target{NodeID: row.NodeID, Execution: execution, State: row.State})
					}
				}
			}
			if err == nil {
				err = w.network(ctx, op, p, false)
			}
			next = "network-move-services"
		case "network-move-services":
			p.Bindings, err = w.serviceRules(ctx, op, p, p.Spec, p.Bindings)
			if err == nil {
				err = commitServices(ctx, w.Queries, *op.EnvironmentID, p.Bindings)
			}
			next = "network-move-vpn"
		case "network-move-vpn":
			p.VPNBefore, p.VPNPlan, err = w.prepareVPN(ctx, op, p.Spec, nil)
			if err == nil {
				p.VPNResult, err = w.callVPN(ctx, op, p.Spec, *p.VPNPlan)
			}
			if err == nil {
				err = commitVPN(ctx, w.Queries, *op.EnvironmentID, p)
			}
			if err == nil {
				err = w.status(ctx, op, p, nil)
			}
			next = "complete"
		default:
			return errors.New("未知网络迁移阶段：" + op.Phase)
		}
		if err != nil {
			return err
		}
		if err = w.phase(ctx, op, p, next); err != nil {
			return err
		}
	}
	return nil
}
