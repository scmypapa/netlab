package network

import (
	"context"
	"netlab.local/core/api"
)

func (n *OVN) applyAccess(ctx context.Context, environment string, record *accessRecord, gateway *api.ServiceGateway) error {
	models, err := accessModels(environment, record, gateway, n.provider)
	if err != nil {
		return err
	}
	return n.applyModels(ctx, environment, "access", models)
}
