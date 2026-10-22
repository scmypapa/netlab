package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/access"
)

func (s *Server) environmentSecurity(w http.ResponseWriter, r *http.Request, identity access.Identity) error {
	id, asset := r.PathValue("id"), r.URL.Query().Get("assetId")
	if _, err := s.Environments.Authorized(r.Context(), identity, id, "observe", ""); err != nil {
		return err
	}
	query := r.URL.Query()
	if query.Get("range") == "" {
		query.Set("range", "3600")
	}
	start, end, _, err := metricRange(query, time.Now())
	if err != nil {
		return httpError{http.StatusBadRequest, err.Error()}
	}
	level, search := r.URL.Query().Get("level"), strings.TrimSpace(r.URL.Query().Get("search"))
	protocol := r.URL.Query().Get("protocol")
	if level != "" && level != "high" && level != "medium" && level != "low" || len(search) > 200 || len(protocol) > 32 {
		return httpError{http.StatusBadRequest, "风险级别或搜索条件无效"}
	}
	var before int64
	if value := r.URL.Query().Get("before"); value != "" {
		if before, err = strconv.ParseInt(value, 10, 64); err != nil || before < 1 {
			return httpError{http.StatusBadRequest, "before 应为正整数"}
		}
	}
	parameters := queries.ListSecurityAlertsParams{EnvironmentID: id, AssetID: asset, Level: level, Search: search, BeforeID: before, Protocol: protocol,
		StartAt: pgtype.Timestamptz{Time: start, Valid: true}, EndAt: pgtype.Timestamptz{Time: end, Valid: true}}
	rows, err := s.Queries.ListSecurityAlerts(r.Context(), parameters)
	if err != nil {
		return err
	}
	result := api.SecurityOverview{Start: start, End: end, Alerts: []api.SecurityAlert{}, Sensors: []api.SecuritySensor{}}
	if len(rows) > 100 {
		cursor := rows[99].ID
		result.NextCursor, rows = &cursor, rows[:100]
	}
	for _, row := range rows {
		var alert api.SecurityAlert
		if err = json.Unmarshal(row.Detail, &alert); err != nil {
			return err
		}
		alert.Id, alert.Count, alert.Severity = row.ID, row.Count, int(row.Severity)
		alert.FirstSeen, alert.LastSeen = row.FirstSeen.Time, row.LastSeen.Time
		result.Alerts = append(result.Alerts, alert)
	}
	counts, err := s.Queries.CountSecurityAlerts(r.Context(), queries.CountSecurityAlertsParams{
		EnvironmentID: id, AssetID: asset, Search: search, StartAt: parameters.StartAt, EndAt: parameters.EndAt, Protocol: protocol,
	})
	if err != nil {
		return err
	}
	result.Counts.High, result.Counts.Medium, result.Counts.Low = counts.High, counts.Medium, counts.Low
	sensors, err := s.Queries.ListEnvironmentSecuritySensors(r.Context(), id)
	if err != nil {
		return err
	}
	for _, row := range sensors {
		observation := api.NodeSecurityObservation{Sensor: api.SecuritySensor{NodeId: row.ID, Status: "unavailable"}}
		if len(row.Observation) > 0 {
			if err = json.Unmarshal(row.Observation, &observation); err != nil {
				return err
			}
		}
		sensor := observation.Sensor
		if observation.HistoryGap {
			message := "节点离线超过本地告警留存期，历史记录存在缺口"
			sensor.Error = &message
		}
		sensor.NodeName, sensor.Interfaces = &row.Name, observation.Coverage[id]
		if row.State != "ready" || !row.CheckedAt.Valid || time.Since(row.CheckedAt.Time) > 30*time.Second {
			message := "节点检测状态未更新"
			sensor.Status, sensor.Error = "unavailable", &message
		}
		result.Sensors = append(result.Sensors, sensor)
	}
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusOK, result)
}
