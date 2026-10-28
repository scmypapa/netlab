package observation

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"netlab.local/core/api"
	"netlab.local/core/db"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/server"
)

func TestSecurityImportAndAPIWithPostgreSQL(t *testing.T) {
	dsn := os.Getenv("NETLAB_DATABASE_URL")
	if dsn == "" {
		t.Skip("NETLAB_DATABASE_URL is not set")
	}
	ctx := context.Background()
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	schema := pgx.Identifier{"security_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer base.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO nodes(id,name,endpoint,info) VALUES('one','Node','https://one','{}');
 INSERT INTO environments(id,project_id,name,spec,status) VALUES('blue','default','Blue','{"assets":[],"networks":[]}','running'),('red','default','Red','{"assets":[],"networks":[]}','running');
 INSERT INTO runtime_assets(environment_id,asset_id,instance_id,node_id,execution,cpu,memory_mib,disk_gib,current,state)
 VALUES('blue','web','i-blue','one','{"interfaces":[{"id":"nic-blue"}]}',1,512,1,true,'running'),('red','web','i-red','one','{"interfaces":[{"id":"nic-red"}]}',1,512,1,true,'running');`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	alert := api.SecurityAlert{EnvironmentId: "blue", NodeId: "one", NetworkId: "lan", InterfaceId: "nic-blue", GeneratorId: 1, SignatureId: 10001, Severity: 1, Signature: "test detection", Category: "Web Application Attack", Source: "10.0.0.2", Destination: "10.0.0.3", SourceAssetId: "web", Protocol: "TCP", SourcePort: 50000, DestinationPort: 80, Count: 1, FirstSeen: now, LastSeen: now}
	observation := api.NodeSecurityObservation{Cursor: "first", Alerts: []api.SecurityAlert{alert, alert}, Sensor: api.SecuritySensor{NodeId: "one", Status: "ready", CheckedAt: now}, Coverage: map[string]int{"blue": 1, "red": 1}}
	service := Service{Pool: pool}
	if err = service.commitSecurity(ctx, "one", "", observation); err != nil {
		t.Fatal(err)
	}
	if err = service.commitSecurity(ctx, "one", "", observation); err == nil {
		t.Fatal("stale cursor replay was accepted")
	}
	q := queries.New(pool)
	if cursor, err := q.GetSecurityCursor(ctx, "one"); err != nil || cursor != "first" {
		t.Fatalf("cursor %q %v", cursor, err)
	}
	bad := alert
	bad.Severity = 0
	observation.Cursor, observation.Alerts = "second", []api.SecurityAlert{bad}
	if err = service.commitSecurity(ctx, "one", "first", observation); err == nil {
		t.Fatal("invalid severity committed")
	}
	if cursor, err := q.GetSecurityCursor(ctx, "one"); err != nil || cursor != "first" {
		t.Fatalf("failed batch advanced cursor: %q %v", cursor, err)
	}
	red := alert
	red.EnvironmentId, red.InterfaceId = "red", "nic-red"
	medium := alert
	medium.Severity, medium.SignatureId = 2, 10002
	observation.Alerts = []api.SecurityAlert{medium, red}
	if err = service.commitSecurity(ctx, "one", "first", observation); err != nil {
		t.Fatal(err)
	}
	apiServer := server.New(pool, nil, nil)
	if err = apiServer.Access.Bootstrap(ctx, "security-integration-password"); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(apiServer.Handler())
	defer web.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	raw, _ := json.Marshal(api.Login{Name: "admin", Password: "security-integration-password"})
	response, err := client.Post(web.URL+"/api/v1/sessions/login", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("login %d", response.StatusCode)
	}
	response, err = client.Get(web.URL + "/api/v1/environments/blue/security?range=3600&level=high")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("API %d", response.StatusCode)
	}
	var overview api.SecurityOverview
	if err = json.NewDecoder(response.Body).Decode(&overview); err != nil {
		t.Fatal(err)
	}
	if len(overview.Alerts) != 1 || overview.Alerts[0].Count != 2 || overview.Alerts[0].EnvironmentId != "blue" || overview.Counts.High != 2 || overview.Counts.Medium != 1 {
		t.Fatalf("wrong filtered API: %+v", overview)
	}
	if len(overview.Sensors) != 1 || overview.Sensors[0].Interfaces != 1 || overview.Sensors[0].Status != "ready" {
		t.Fatalf("wrong scoped coverage: %+v", overview.Sensors)
	}
	if _, err = pool.Exec(ctx, `UPDATE environments SET status='destroyed' WHERE id='red'; UPDATE security_alerts SET last_seen=now()-interval '31 days' WHERE environment_id='blue'`); err != nil {
		t.Fatal(err)
	}
	if err = q.PruneSecurityAlerts(ctx); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM security_alerts").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("retention: %d %v", remaining, err)
	}
}
