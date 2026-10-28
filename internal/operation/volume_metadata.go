package operation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
	"netlab.local/core/api"
	"netlab.local/core/db/queries"
	"netlab.local/core/internal/backup"
	"netlab.local/core/internal/environment"
	"netlab.local/core/internal/transport"
)

const ContainerVolumeMigration = "oci-volume-metadata-v1"

// UpgradeContainerVolumes materializes the volume contract once. It reads image
// metadata outside the write transaction; existing guests and disks are untouched.
func UpgradeContainerVolumes(ctx context.Context, pool *pgxpool.Pool, client *transport.Client) error {
	connection, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer connection.Close(context.Background())
	if _, err = connection.Exec(ctx, "SELECT pg_advisory_lock(73421492)"); err != nil {
		return err
	}
	var complete bool
	if err = connection.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)", ContainerVolumeMigration).Scan(&complete); err != nil || complete {
		return err
	}
	documents, err := readVolumeDocuments(ctx, connection)
	if err != nil {
		return err
	}
	metadata, err := prepareVolumeMetadata(ctx, connection, client, documents)
	if err != nil {
		return err
	}
	tx, err := connection.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Only this short data rewrite excludes writers; all remote preparation is over.
	if _, err = tx.Exec(ctx, "LOCK TABLE templates,environments,runtime_assets,operations,recovery_points,backups IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return err
	}
	documents, err = readVolumeDocuments(ctx, tx)
	if err != nil {
		return err
	}
	if err = documents.resolve(metadata); err != nil {
		return err
	}
	if err = documents.save(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(name) VALUES($1)", ContainerVolumeMigration); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type volumeDocument struct {
	kind, id, environment string
	operationKind, phase  string
	current               bool
	value                 any
}

type volumeDocuments []volumeDocument

func readVolumeDocuments(ctx context.Context, db queries.DBTX) (volumeDocuments, error) {
	rows, err := db.Query(ctx, `
 SELECT 'template',id,'','','',false,definition FROM templates WHERE definition->>'kind'='container'
 UNION ALL SELECT 'runtime',instance_id,environment_id,'','',current,execution FROM runtime_assets WHERE execution->'template'->>'kind'='container'
 UNION ALL SELECT 'spec',id,id,'','',false,spec FROM environments
 UNION ALL SELECT 'applied',id,id,'','',false,applied_spec FROM environments WHERE applied_spec IS NOT NULL
 UNION ALL SELECT 'draft',id,id,'','',false,draft FROM environments WHERE draft IS NOT NULL
 UNION ALL SELECT 'operation',o.id,COALESCE(o.environment_id,''),o.kind,o.phase,COALESCE(e.operation_id=o.id,false),o.payload
  FROM operations o LEFT JOIN environments e ON e.id=o.environment_id
  WHERE o.scope_kind<>'system' AND o.state<>'succeeded' AND o.phase NOT IN ('complete','rolled-back','cancelled')
 UNION ALL SELECT 'recovery',id,environment_id,'','',false,definition FROM recovery_points
 UNION ALL SELECT 'backup',id,'','','',false,definition FROM backups`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	documents := volumeDocuments{}
	for rows.Next() {
		var document volumeDocument
		var raw []byte
		if err := rows.Scan(&document.kind, &document.id, &document.environment, &document.operationKind, &document.phase, &document.current, &raw); err != nil {
			return nil, err
		}
		switch document.kind {
		case "template":
			document.value = new(api.Template)
		case "runtime":
			document.value = new(api.AssetExecution)
		case "spec", "applied":
			document.value = new(api.EnvironmentSpec)
		case "draft":
			document.value = new(api.Draft)
		case "operation":
			document.value = new(Payload)
		case "recovery":
			document.value = new(Recovery)
		case "backup":
			document.value = new(BackupDefinition)
		}
		if err := json.Unmarshal(raw, document.value); err != nil {
			return nil, fmt.Errorf("%s %s: %w", document.kind, document.id, err)
		}
		documents = append(documents, document)
	}
	return documents, rows.Err()
}

func (document volumeDocument) templates() []*api.Template {
	templates := []*api.Template{}
	add := func(targets []Target) {
		for i := range targets {
			templates = append(templates, &targets[i].Execution.Template)
		}
	}
	switch value := document.value.(type) {
	case *api.Template:
		templates = append(templates, value)
	case *api.AssetExecution:
		templates = append(templates, &value.Template)
	case *Payload:
		if value.Template != nil {
			templates = append(templates, value.Template)
		}
		for _, targets := range [][]Target{value.Before, value.Old, value.Targets, value.Updates, value.Unchanged} {
			add(targets)
		}
		if value.Recovery != nil {
			add(value.Recovery.Assets)
		}
		if value.Migration != nil && value.Migration.Target != nil {
			templates = append(templates, &value.Migration.Target.Execution.Template)
		}
	case *Recovery:
		add(value.Assets)
	case *BackupDefinition:
		add(value.Recovery.Assets)
	}
	return templates
}

func (document volumeDocument) needsSource() bool {
	if document.kind == "backup" {
		return false
	}
	if payload, ok := document.value.(*Payload); ok && payload.BackupID != nil && len(payload.Targets) == 0 && len(payload.Updates) == 0 {
		return false // Archived templates are prepared by the normal backup restore path.
	}
	return true
}

func prepareVolumeMetadata(ctx context.Context, db queries.DBTX, client *transport.Client, documents volumeDocuments) (map[string]api.Template, error) {
	nodes, err := queries.New(db).ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	endpoints := make(map[string]string, len(nodes))
	for _, node := range nodes {
		endpoints[node.ID] = node.Endpoint
	}
	byNode := map[string][]api.Template{}
	metadata := map[string]api.Template{}
	for _, document := range documents {
		if !document.needsSource() {
			continue
		}
		for _, template := range document.templates() {
			if template.Kind != api.Container || template.ArtifactNodeId == nil {
				continue
			}
			key := backup.TemplateKey(*template)
			if _, exists := metadata[key]; exists {
				continue
			}
			metadata[key] = *template
			if template.ImageVolumes == nil {
				owner := *template.ArtifactNodeId
				if endpoints[owner] == "" {
					return nil, fmt.Errorf("模板 %s/%d 的来源节点 %s 不存在", template.Name, template.Version, owner)
				}
				byNode[owner] = append(byNode[owner], *template)
			}
		}
	}
	prepared := make(chan api.Template, len(metadata))
	group, ctx := errgroup.WithContext(ctx)
	for node, templates := range byNode {
		group.Go(func() error {
			for _, template := range templates {
				var result api.Template
				request := api.NodeTemplatePreparation{Template: template}
				if err := client.Do(ctx, http.MethodPost, endpoints[node], "/node/v1/templates/prepare", request, &result); err != nil {
					return fmt.Errorf("准备模板 %s/%d 的卷元数据：%w", template.Name, template.Version, err)
				}
				if result.ImageVolumes == nil {
					return fmt.Errorf("节点 %s 未返回模板 %s/%d 的 OCI 卷元数据", node, template.Name, template.Version)
				}
				prepared <- result
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	close(prepared)
	for template := range prepared {
		metadata[backup.TemplateKey(template)] = template
	}
	return metadata, nil
}

func (documents volumeDocuments) resolve(metadata map[string]api.Template) error {
	catalog := map[string]*api.Template{}
	current := map[string]map[string]*api.Template{}
	for _, document := range documents {
		for _, template := range document.templates() {
			if template.Kind != api.Container {
				continue
			}
			if prepared, exists := metadata[backup.TemplateKey(*template)]; exists {
				template.ImageVolumes = prepared.ImageVolumes
			}
			if template.ArtifactNodeId != nil && template.ImageVolumes == nil && document.needsSource() {
				return fmt.Errorf("模板 %s/%d 的 OCI 卷元数据尚未准备", template.Name, template.Version)
			}
		}
		switch value := document.value.(type) {
		case *api.Template:
			catalog[value.Id] = value
		case *api.AssetExecution:
			if value.Template.Kind == api.Container {
				value.Asset.Volumes = environment.ResolveVolumeDefaults(value.Template, value.Asset.Volumes)
			}
			if document.current {
				if current[document.environment] == nil {
					current[document.environment] = map[string]*api.Template{}
				}
				current[document.environment][value.Asset.Id] = &value.Template
			}
		}
	}
	resolveSpec := func(spec *api.EnvironmentSpec, frozen map[string]*api.Template) {
		if spec == nil {
			return
		}
		for i := range spec.Assets {
			asset := &spec.Assets[i]
			template := frozen[asset.Id]
			if template == nil {
				template = catalog[asset.TemplateId]
			}
			if template != nil && template.Kind == api.Container {
				asset.Volumes = environment.ResolveVolumeDefaults(*template, asset.Volumes)
			}
		}
	}
	for _, document := range documents {
		switch value := document.value.(type) {
		case *api.EnvironmentSpec:
			var frozen map[string]*api.Template
			if document.kind == "applied" {
				frozen = current[document.environment]
			}
			resolveSpec(value, frozen)
		case *api.Draft:
			resolveSpec(&value.Spec, nil)
		case *Payload:
			before := resolveTargetVolumes(value.Before, value.Old)
			after := resolveTargetVolumes(value.Unchanged, value.Targets, value.Updates)
			if value.Recovery != nil {
				recovery := resolveTargetVolumes(value.Recovery.Assets)
				resolveSpec(&value.Recovery.Spec, recovery)
				for id, template := range recovery {
					if after[id] == nil {
						after[id] = template
					}
				}
			}
			if value.Migration != nil && value.Migration.Target != nil {
				execution := &value.Migration.Target.Execution
				if execution.Template.Kind == api.Container {
					execution.Asset.Volumes = environment.ResolveVolumeDefaults(execution.Template, execution.Asset.Volumes)
				}
			}
			resolveSpec(&value.Spec, after)
			resolveSpec(value.BeforeSpec, before)
		case *Recovery:
			resolveSpec(&value.Spec, resolveTargetVolumes(value.Assets))
		case *BackupDefinition:
			resolveSpec(&value.Recovery.Spec, resolveTargetVolumes(value.Recovery.Assets))
		}
	}
	return nil
}

func resolveTargetVolumes(groups ...[]Target) map[string]*api.Template {
	templates := map[string]*api.Template{}
	for _, targets := range groups {
		for i := range targets {
			execution := &targets[i].Execution
			if execution.Template.Kind == api.Container {
				execution.Asset.Volumes = environment.ResolveVolumeDefaults(execution.Template, execution.Asset.Volumes)
			}
			templates[execution.Asset.Id] = &execution.Template
		}
	}
	return templates
}

func (documents volumeDocuments) save(ctx context.Context, tx pgx.Tx) error {
	statements := map[string]string{
		"template":  "UPDATE templates SET definition=$2 WHERE id=$1 AND definition IS DISTINCT FROM $2::jsonb",
		"spec":      "UPDATE environments SET spec=$2 WHERE id=$1 AND spec IS DISTINCT FROM $2::jsonb",
		"applied":   "UPDATE environments SET applied_spec=$2 WHERE id=$1 AND applied_spec IS DISTINCT FROM $2::jsonb",
		"draft":     "UPDATE environments SET draft=$2 WHERE id=$1 AND draft IS DISTINCT FROM $2::jsonb",
		"operation": "UPDATE operations SET payload=$2 WHERE id=$1 AND payload IS DISTINCT FROM $2::jsonb",
		"recovery":  "UPDATE recovery_points SET definition=$2 WHERE id=$1 AND definition IS DISTINCT FROM $2::jsonb",
		"backup":    "UPDATE backups SET definition=$2 WHERE id=$1 AND definition IS DISTINCT FROM $2::jsonb",
	}
	batch := &pgx.Batch{}
	budgets := documents.diskBudgets()
	for _, document := range documents {
		raw, err := json.Marshal(document.value)
		if err != nil {
			return err
		}
		if document.kind == "runtime" {
			budget := budgets[document.id]
			batch.Queue("UPDATE runtime_assets SET execution=$2,disk_gib=$3,system_gib=$4 WHERE instance_id=$1", document.id, raw, budget.system+budget.data, budget.system)
		} else {
			batch.Queue(statements[document.kind], document.id, raw)
		}
	}
	return tx.SendBatch(ctx, batch).Close()
}

func (documents volumeDocuments) diskBudgets() map[string]diskBudget {
	budgets := map[string]diskBudget{}
	for _, document := range documents {
		if execution, ok := document.value.(*api.AssetExecution); ok {
			budgets[execution.InstanceId] = assetDiskBudget(execution.Asset)
		}
	}
	for _, document := range documents {
		payload, ok := document.value.(*Payload)
		if !ok || !document.current || document.phase == "queued" {
			continue
		}
		before := map[string]api.Asset{}
		for _, target := range payload.Before {
			before[target.Execution.InstanceId] = target.Execution.Asset
		}
		for _, target := range payload.Updates {
			id := target.Execution.InstanceId
			if _, exists := budgets[id]; exists {
				budgets[id] = peakDiskBudget(before[id], target.Execution.Asset, restoresData(document.operationKind))
			}
		}
	}
	return budgets
}
