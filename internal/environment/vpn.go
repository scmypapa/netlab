package environment

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

type VPNChange struct {
	ID     string `json:"id"`
	Remove bool   `json:"remove"`
}

// Aliases are reserved in the caller's short transaction, before node execution.
func VPNRoutes(ctx context.Context, q *queries.Queries, environmentID string, spec api.EnvironmentSpec, peers []api.VPNPeer) ([]api.VPNPeer, error) {
	aliases, err := q.ListVPNAliases(ctx)
	if err != nil {
		return nil, err
	}
	networks := map[string]api.Network{}
	nodes, err := q.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	occupied := []netip.Prefix{}
	for _, node := range nodes {
		var info api.NodeInfo
		if err = json.Unmarshal(node.Info, &info); err != nil {
			return nil, err
		}
		if info.ServiceNetwork != nil {
			occupied = append(occupied, netip.MustParsePrefix(info.ServiceNetwork.Cidr))
		}
	}
	for _, nw := range spec.Networks {
		networks[nw.Id] = nw
		occupied = append(occupied, netip.MustParsePrefix(nw.Cidr))
	}
	for _, alias := range aliases {
		occupied = append(occupied, alias.Prefix)
	}
	result := slices.Clone(peers)
	for index := range result {
		peer := &result[index]
		routes := []api.VPNRoute{}
		for _, route := range peer.Routes {
			nw, exists := networks[route.NetworkId]
			if !exists {
				continue
			}
			prefix := netip.MustParsePrefix(nw.Cidr)
			destination := prefix
			if peer.Mode == api.Translated {
				destination = netip.Prefix{}
				for _, alias := range aliases {
					if alias.EnvironmentID == environmentID && alias.NetworkID == nw.Id && alias.Prefix.Addr().BitLen() == prefix.Addr().BitLen() && alias.Prefix.Bits() == prefix.Bits() {
						destination = alias.Prefix
						break
					}
				}
				if !destination.IsValid() {
					pool := netip.MustParsePrefix("100.64.0.0/10")
					if prefix.Addr().Is6() {
						pool = netip.MustParsePrefix("fd00::/8")
					}
					destination, err = freeVPNPrefix(pool, prefix.Bits(), occupied)
					if err != nil {
						return nil, err
					}
					if err = q.ReserveVPNAlias(ctx, queries.ReserveVPNAliasParams{EnvironmentID: environmentID, NetworkID: nw.Id, Prefix: destination}); err != nil {
						return nil, err
					}
					aliases = append(aliases, queries.VpnAlias{EnvironmentID: environmentID, NetworkID: nw.Id, Prefix: destination})
					occupied = append(occupied, destination)
				}
			}
			routes = append(routes, api.VPNRoute{NetworkId: nw.Id, Cidr: prefix.String(), AccessCidr: destination.String()})
		}
		peer.Routes = routes
	}
	return result, nil
}

func freeVPNPrefix(pool netip.Prefix, bits int, occupied []netip.Prefix) (netip.Prefix, error) {
	if bits < pool.Bits() {
		return netip.Prefix{}, Invalid("网段超出 VPN 独立访问地址池的容量")
	}
	candidate := netip.PrefixFrom(pool.Addr(), bits)
	for pool.Contains(candidate.Addr()) {
		conflict := netip.Prefix{}
		for _, prefix := range occupied {
			if prefix.Overlaps(candidate) {
				conflict = prefix
				break
			}
		}
		if !conflict.IsValid() {
			return candidate, nil
		}
		jump := candidate
		if conflict.Bits() < candidate.Bits() {
			jump = conflict
		}
		bytes := jump.Masked().Addr().As16()
		hostBits := jump.Addr().BitLen() - jump.Bits()
		carry := uint16(1 << (hostBits % 8))
		for i := 15 - hostBits/8; i >= 0 && carry != 0; i-- {
			carry += uint16(bytes[i])
			bytes[i] = byte(carry)
			carry >>= 8
		}
		if carry != 0 {
			break
		}
		next := netip.AddrFrom16(bytes).Unmap()
		if next.Compare(candidate.Addr()) <= 0 {
			break
		}
		candidate = netip.PrefixFrom(next, bits).Masked()
	}
	return netip.Prefix{}, Invalid("VPN 独立访问地址池已分配完")
}

func (s Service) CreateVPN(ctx context.Context, identity access.Identity, id string, request api.CreateVPNAccess) (api.Operation, error) {
	key, err := base64.StdEncoding.DecodeString(request.PublicKey)
	if err != nil || len(key) != 32 || !slices.ContainsFunc(key, func(value byte) bool { return value != 0 }) {
		return api.Operation{}, Invalid("WireGuard 公钥无效")
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || len([]rune(request.Name)) > 100 || len(request.NetworkIds) == 0 {
		return api.Operation{}, Invalid("请填写连接名称并选择网段")
	}
	if request.Mode != nil && *request.Mode != api.Original && *request.Mode != api.Translated {
		return api.Operation{}, Invalid("VPN 地址模式无效")
	}
	return s.changeVPN(ctx, identity, id, "", &request, request.ExpectedRevision, request.ClientRequestId)
}

func (s Service) RevokeVPN(ctx context.Context, identity access.Identity, id, accessID string, revision int, requestID *string) (api.Operation, error) {
	return s.changeVPN(ctx, identity, id, accessID, nil, revision, requestID)
}

func (s Service) changeVPN(ctx context.Context, identity access.Identity, id, accessID string, request *api.CreateVPNAccess, revision int, requestID *string) (api.Operation, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return api.Operation{}, err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	row, err := q.LockEnvironment(ctx, id)
	if err != nil {
		return api.Operation{}, err
	}
	if !identity.Allows("access", row.ProjectID, id, "", row.OwnerID) {
		return api.Operation{}, access.ErrForbidden
	}
	kind := "vpn-revoke"
	if request != nil {
		kind = "vpn-create"
	}
	if requestID != nil {
		previous, err := existingRequest(ctx, q, id, requestID, kind, "")
		if err == nil {
			return previous, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return api.Operation{}, err
		}
	}
	if row.Revision != int32(revision) {
		return api.Operation{}, ErrConflict
	}
	if row.AppliedSpec == nil || row.Status == "destroyed" || row.Status == "destroying" {
		return api.Operation{}, Invalid("运行环境启动后可管理 VPN")
	}
	var spec api.EnvironmentSpec
	if err = json.Unmarshal(row.AppliedSpec, &spec); err != nil {
		return api.Operation{}, err
	}
	var peer api.VPNPeer
	if request != nil {
		peer = api.VPNPeer{Id: uuid.NewString(), Name: request.Name, PublicKey: request.PublicKey, Mode: api.Original, Routes: []api.VPNRoute{}}
		if request.Mode != nil {
			peer.Mode = *request.Mode
		}
		found := map[string]bool{}
		for _, nw := range spec.Networks {
			found[nw.Id] = true
		}
		for _, networkID := range request.NetworkIds {
			if !found[networkID] {
				return api.Operation{}, Invalid("选择的网段不存在或重复")
			}
			delete(found, networkID)
			peer.Routes = append(peer.Routes, api.VPNRoute{NetworkId: networkID})
		}
		if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(73421494)"); err != nil {
			return api.Operation{}, err
		}
		peers, routeErr := VPNRoutes(ctx, q, id, spec, []api.VPNPeer{peer})
		if routeErr != nil {
			return api.Operation{}, routeErr
		}
		peer = peers[0]
		accessID = peer.Id
	} else if _, err = q.GetVPNAccess(ctx, queries.GetVPNAccessParams{EnvironmentID: id, ID: accessID}); err != nil {
		return api.Operation{}, err
	}
	raw, err := json.Marshal(struct {
		Spec         api.EnvironmentSpec `json:"spec"`
		BeforeStatus string              `json:"beforeStatus"`
		VPNChange    VPNChange           `json:"vpnChange"`
	}{spec, row.Status, VPNChange{accessID, request == nil}})
	if err != nil {
		return api.Operation{}, err
	}
	op, err := submitPayload(ctx, q, row, kind, nil, raw, requestID)
	if err != nil {
		return api.Operation{}, err
	}
	if request != nil {
		definition, _ := json.Marshal(peer)
		err = q.CreateVPNAccess(ctx, queries.CreateVPNAccessParams{ID: accessID, EnvironmentID: id, Definition: definition, OperationID: op.ID})
	} else {
		err = q.SetVPNAccessOperation(ctx, queries.SetVPNAccessOperationParams{EnvironmentID: id, ID: accessID, OperationID: op.ID})
	}
	if err != nil {
		return api.Operation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return api.Operation{}, err
	}
	return Operation(op)
}

func (s Service) VPNList(ctx context.Context, identity access.Identity, id string) ([]api.VPNAccess, error) {
	if _, err := s.Authorized(ctx, identity, id, "access", ""); err != nil {
		return nil, err
	}
	rows, err := s.Queries.ListVPNAccess(ctx, id)
	if err != nil {
		return nil, err
	}
	result := []api.VPNAccess{}
	for _, row := range rows {
		var value api.VPNAccess
		if err = json.Unmarshal(row.Definition, &value); err != nil {
			return nil, err
		}
		value.State = api.VPNAccessStatePending
		if row.Applied {
			value.State = api.VPNAccessStateActive
		}
		if row.OperationState == "failed" && !row.Applied {
			value.State = api.VPNAccessStateFailed
		}
		if row.OperationKind == "vpn-revoke" && (row.OperationState == "queued" || row.OperationState == "running") {
			value.State = api.VPNAccessStateRevoking
		}
		value.CreatedAt = row.CreatedAt.Time
		value.OperationId = row.OperationID
		value.Error = row.OperationError
		if row.Applied {
			addresses := vpnAddresses(row.Addresses)
			value.Addresses = &addresses
		}
		result = append(result, value)
	}
	return result, nil
}

func vpnAddresses(addresses []netip.Addr) []string {
	result := make([]string, 0, len(addresses))
	for _, addr := range addresses {
		result = append(result, netip.PrefixFrom(addr, addr.BitLen()).String())
	}
	return result
}

func (s Service) VPNConnection(ctx context.Context, identity access.Identity, id, accessID string) (api.VPNConnection, error) {
	row, err := s.Authorized(ctx, identity, id, "access", "")
	if err != nil {
		return api.VPNConnection{}, err
	}
	peer, err := s.Queries.GetVPNAccess(ctx, queries.GetVPNAccessParams{EnvironmentID: id, ID: accessID})
	if err != nil {
		return api.VPNConnection{}, err
	}
	port, err := s.Queries.GetVPNPort(ctx, id)
	if err != nil {
		return api.VPNConnection{}, err
	}
	if !peer.Applied || port.State != "applied" || row.VpnPublicKey == nil || row.VpnMtu == nil {
		return api.VPNConnection{}, Invalid("VPN 连接尚未生效")
	}
	var definition api.VPNPeer
	if err = json.Unmarshal(peer.Definition, &definition); err != nil {
		return api.VPNConnection{}, err
	}
	var info api.NodeInfo
	if err = json.Unmarshal(port.Info, &info); err != nil {
		return api.VPNConnection{}, err
	}
	if info.AccessAddress == nil {
		return api.VPNConnection{}, fmt.Errorf("网络节点没有接入地址")
	}
	result := api.VPNConnection{Endpoint: net.JoinHostPort(*info.AccessAddress, strconv.Itoa(int(*port.Port))), PublicKey: *row.VpnPublicKey, Addresses: vpnAddresses(peer.Addresses), AllowedIPs: []string{}, Mtu: int(*row.VpnMtu)}
	for _, route := range definition.Routes {
		result.AllowedIPs = append(result.AllowedIPs, route.AccessCidr)
	}
	return result, nil
}
