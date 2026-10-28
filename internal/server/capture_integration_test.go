package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/capture"
	"netlab.local/core/internal/transport"
)

func testCaptureAPI(t *testing.T, ctx context.Context, s *Server, admin string, call func(string, string, string, any, int) []byte) {
	t.Helper()
	var env api.Environment
	if err := json.Unmarshal(call("GET", "/environments/env-a", admin, nil, 200), &env); err != nil {
		t.Fatal(err)
	}
	env.Id = uuid.NewString()
	spec, _ := json.Marshal(env.Spec)
	if _, err := s.Pool.Exec(ctx, "INSERT INTO environments(id,project_id,name,spec,status) VALUES($1,'default','Capture fixture',$2,'running')", env.Id, spec); err != nil {
		t.Fatal(err)
	}
	nodeID := uuid.NewString()
	recentID := base64.RawURLEncoding.EncodeToString([]byte(nodeID + "/" + uuid.NewString() + "/1"))
	var mutex sync.Mutex
	var request api.NodeCaptureRequest
	status := api.CaptureSegmentStatusRunning
	trafficFailure := false
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		recentPath := "/node/v1/environments/" + env.Id + "/packets"
		if r.URL.Path == recentPath {
			if trafficFailure {
				http.Error(w, "recorder unavailable", 503)
				return
			}
			writeJSON(w, 200, api.RecentPacketPage{Packets: []api.CapturePacket{{Id: &recentID, Number: 1, Time: time.Now().UTC(), Protocol: "TCP"}}, Errors: map[string]string{}, Retention: api.PacketRetention{LimitBytes: capture.PacketBudget, UsedBytes: 1024, Status: api.PacketRetentionStatusRecording}})
			return
		}
		if r.URL.Path == recentPath+"/"+recentID {
			writeJSON(w, 200, api.CapturePacketDetail{Packet: api.CapturePacket{Id: &recentID, Number: 1}, Hex: "010203", Layers: []api.PacketField{}})
			return
		}
		if r.URL.Path == recentPath+"/export" {
			w.Header().Set("Content-Type", "application/octet-stream")
			capture.EmptyPacketCapture(w)
			return
		}
		if r.URL.Path == "/node/v1/traffic" {
			var interfaces []api.CaptureInterface
			if err := json.NewDecoder(r.Body).Decode(&interfaces); err != nil {
				t.Error(err)
			}
			if len(interfaces) != 1 || interfaces[0].AssetId != "one" {
				t.Error("traffic query did not use current environment interfaces", interfaces)
			}
			if trafficFailure {
				http.Error(w, "IPFIX unavailable", 503)
				return
			}
			writeJSON(w, 200, api.TrafficObservation{Flows: []api.CaptureFlow{{Source: "10.0.0.1", Destination: "10.0.0.2", Protocol: "UDP", Bytes: 51200, Packets: 512}}, Errors: map[string]string{}, SamplingRate: 1, WindowSeconds: 60, ObservedAt: time.Now()})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/node/v1/environments/"+env.Id+"/captures" {
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
		}
		segment := api.CaptureSegment{Id: request.Id, NodeId: nodeID, EnvironmentId: env.Id, AssetIds: []string{"one"}, StartedAt: time.Now(), Status: status}
		switch {
		case strings.HasSuffix(r.URL.Path, "/packets"):
			if r.URL.Query().Get("source") != "10.0.0.1" || r.URL.Query().Get("after") != "7" {
				t.Error("packet query lost", r.URL)
			}
			writeJSON(w, 200, api.CapturePacketPage{Packets: []api.CapturePacket{{Number: 8, Protocol: "UDP"}}})
		case strings.HasSuffix(r.URL.Path, "/packets/8"):
			writeJSON(w, 200, api.CapturePacketDetail{Packet: api.CapturePacket{Number: 8}, Hex: "010203", Layers: []api.PacketField{}})
		case strings.HasSuffix(r.URL.Path, "/packets/9"):
			http.Error(w, "frame not found", http.StatusNotFound)
		case r.Method == http.MethodDelete:
			w.WriteHeader(204)
		case r.Method == http.MethodGet && r.URL.Path == "/node/v1/environments/"+env.Id+"/captures":
			writeJSON(w, 200, []api.CaptureSegment{segment})
		case r.Method == http.MethodGet:
			writeJSON(w, 200, api.CaptureDetail{Segment: segment, Flows: []api.CaptureFlow{}})
		default:
			if r.URL.Path != "/node/v1/environments/"+env.Id+"/captures" {
				status = api.CaptureSegmentStatusStopped
				segment.Status = status
			}
			writeJSON(w, 200, segment)
		}
	}))
	defer node.Close()
	s.Nodes = &transport.Client{HTTP: node.Client()}
	if err := s.Queries.PutNode(ctx, queries.PutNodeParams{ID: nodeID, Name: "capture-node", Endpoint: node.URL, Info: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Queries.PutNode(ctx, queries.PutNodeParams{ID: uuid.NewString(), Name: "unrelated-offline-node", Endpoint: "http://127.0.0.1:1", Info: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	execution := api.AssetExecution{Asset: env.Spec.Assets[0], InstanceId: uuid.NewString(), Interfaces: []api.ResolvedInterface{{Id: "one-nic", NetworkId: "shared", PortName: uuid.NewString(), Mac: "02:00:00:00:00:01", Address: "10.0.0.1"}}}
	raw, _ := json.Marshal(execution)
	if _, err := s.Pool.Exec(ctx, "INSERT INTO runtime_assets(environment_id,asset_id,instance_id,node_id,execution,cpu,memory_mib,disk_gib,current,state) VALUES($1,$2,$3,$4,$5,1,64,1,true,'running')", env.Id, execution.Asset.Id, execution.InstanceId, nodeID, raw); err != nil {
		t.Fatal(err)
	}
	var token api.IssuedServiceToken
	if err := json.Unmarshal(call("POST", "/service-tokens", admin, api.CreateServiceToken{Name: "capture-asset-observer", Grants: []api.ScopeGrant{{ScopeKind: "asset", ScopeId: env.Id + "/one", Permissions: []api.Permission{"observe"}}}}, 201), &token); err != nil {
		t.Fatal(err)
	}
	path := "/environments/" + env.Id + "/captures"
	trafficPath := "/environments/" + env.Id + "/traffic"
	call("GET", trafficPath, token.Token, nil, 403)
	var observed api.TrafficObservation
	if err := json.Unmarshal(call("GET", trafficPath, admin, nil, 200), &observed); err != nil || len(observed.Flows) != 1 || len(observed.Errors) != 0 || observed.SamplingRate != 1 {
		t.Fatalf("traffic %+v %v", observed, err)
	}
	mutex.Lock()
	trafficFailure = true
	mutex.Unlock()
	if err := json.Unmarshal(call("GET", trafficPath, admin, nil, 200), &observed); err != nil || len(observed.Flows) != 0 || len(observed.Errors) != 1 {
		t.Fatalf("node failure hidden %+v %v", observed, err)
	}
	mutex.Lock()
	trafficFailure = false
	mutex.Unlock()
	settings := api.CreateCapture{AssetIds: []string{"one"}, DurationSeconds: 10, FileSizeMiB: 1}
	call("GET", path, token.Token, nil, 403)
	call("POST", path, token.Token, settings, 403)
	var started api.CaptureList
	if err := json.Unmarshal(call("POST", path, admin, settings, 201), &started); err != nil || len(started.Segments) != 1 || len(started.Errors) != 0 {
		t.Fatalf("start: %+v %v", started, err)
	}
	var listed api.CaptureList
	if err := json.Unmarshal(call("GET", path, admin, nil, 200), &listed); err != nil || len(listed.Segments) != 1 || len(listed.Errors) != 0 {
		t.Fatalf("unrelated node affected capture: %+v %v", listed, err)
	}
	segmentPath := path + "/" + nodeID + "/" + started.Segments[0].Id
	call("GET", segmentPath, token.Token, nil, 403)
	call("GET", segmentPath+"/packets?source=10.0.0.1&after=7", token.Token, nil, 403)
	var packets api.CapturePacketPage
	if err := json.Unmarshal(call("GET", segmentPath+"/packets?source=10.0.0.1&after=7", admin, nil, 200), &packets); err != nil || len(packets.Packets) != 1 || packets.Packets[0].Number != 8 {
		t.Fatal(packets, err)
	}
	var packet api.CapturePacketDetail
	if err := json.Unmarshal(call("GET", segmentPath+"/packets/8", admin, nil, 200), &packet); err != nil || packet.Packet.Number != 8 || packet.Hex != "010203" {
		t.Fatal(packet, err)
	}
	call("GET", segmentPath+"/packets/9", admin, nil, 404)
	call("GET", "/environments/env-b/captures/"+nodeID+"/"+started.Segments[0].Id+"/packets/8", admin, nil, 404)
	call("GET", "/environments/env-b/captures/"+nodeID+"/"+started.Segments[0].Id, admin, nil, 404)
	call("POST", segmentPath, admin, nil, 200)
	call("DELETE", segmentPath, admin, nil, 204)
	refs, err := s.Queries.ListCaptureSegments(ctx, env.Id)
	if err != nil || len(refs) != 0 {
		t.Fatalf("capture ownership remains: %+v %v", refs, err)
	}
	if err = s.Queries.EnsurePacketNodes(ctx, queries.EnsurePacketNodesParams{EnvironmentIds: []string{env.Id}, NodeIds: []string{nodeID}}); err != nil {
		t.Fatal(err)
	}
	info, _ := json.Marshal(api.PacketRetention{LimitBytes: capture.PacketBudget, UsedBytes: 1024, Status: api.PacketRetentionStatusRecording})
	if err = s.Queries.SavePacketNode(ctx, queries.SavePacketNodeParams{EnvironmentID: env.Id, NodeID: nodeID, QuotaBytes: capture.PacketBudget, Observation: info}); err != nil {
		t.Fatal(err)
	}
	recentPath := "/environments/" + env.Id + "/packets"
	call("GET", recentPath, token.Token, nil, 403)
	var recent api.RecentPacketPage
	if err = json.Unmarshal(call("GET", recentPath+"?networkId=shared", admin, nil, 200), &recent); err != nil || len(recent.Packets) != 1 || recent.Packets[0].Id == nil || *recent.Packets[0].Id != recentID {
		t.Fatal(recent, err)
	}
	call("GET", recentPath+"?source=invalid-address", admin, nil, 400)
	call("GET", recentPath+"/"+recentID, admin, nil, 200)
	call("GET", "/environments/env-b/packets/"+recentID, admin, nil, 410)
	if data := call("GET", recentPath+"/export", admin, nil, 200); len(data) < 28 {
		t.Fatal("empty download is not a PCAPNG file")
	}
	mutex.Lock()
	trafficFailure = true
	mutex.Unlock()
	if err = json.Unmarshal(call("GET", recentPath, admin, nil, 200), &recent); err != nil || len(recent.Errors) != 1 || recent.Retention.Status != api.PacketRetentionStatusUnavailable {
		t.Fatal("recorder error hidden", recent, err)
	}
}
