import { ActionIcon, Button, Select } from "@mantine/core";
import { Plus, Trash2 } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { api, type RolePreset, type Schema } from "../../api/client";
import { ErrorMessage } from "../../foundation/Feedback";
import { useCursorList } from "../../foundation/useCursorList";
import { LoadMore } from "../../foundation/LoadMore";
import { PermissionEditor } from "./PermissionEditor";
import styles from "./AccessWorkspace.module.css";

type Grant = Schema<"ScopeGrant">;
export function GrantEditor({
  value,
  onChange,
  roles,
}: {
  value: Grant[];
  onChange: (value: Grant[]) => void;
  roles: RolePreset[];
}) {
  const environments = useCursorList(
    ["environments", "grant-picker"],
    api.environments,
    { enabled: value.some((grant) => grant.scopeKind !== "project") },
  );
  const update = (index: number, grant: Grant) =>
    onChange(value.map((g, i) => (i === index ? grant : g)));
  return (
    <div className={styles.grants}>
      {value.map((grant, index) => (
        <section className={styles.grant} key={index}>
          <div className={styles.grantHeader}>
            <div
              className={styles.scopeTabs}
              role="group"
              aria-label="授权范围"
            >
              {[
                ["project", "整个项目"],
                ["environment", "指定环境"],
                ["asset", "指定资产"],
              ].map(([kind, label]) => (
                <button
                  key={kind}
                  type="button"
                  aria-pressed={grant.scopeKind === kind}
                  onClick={() =>
                    update(index, {
                      ...grant,
                      scopeKind: kind as Grant["scopeKind"],
                      scopeId: kind === "project" ? "default" : "",
                    })
                  }
                >
                  {label}
                </button>
              ))}
            </div>
            <ActionIcon
              variant="subtle"
              aria-label="移除授权"
              onClick={() => onChange(value.filter((_, i) => i !== index))}
            >
              <Trash2 size={16} />
            </ActionIcon>
          </div>
          {grant.scopeKind !== "project" && (
            <div className={styles.targets}>
              <Select
                label="环境"
                searchable
                value={grant.scopeId.split("/")[0] || null}
                data={(environments.data ?? []).map((e) => ({
                  value: e.id,
                  label: e.name,
                }))}
                onChange={(scopeId) =>
                  update(index, {
                    ...grant,
                    scopeId:
                      grant.scopeKind === "asset" ? `${scopeId}/` : scopeId!,
                  })
                }
              />
              {grant.scopeKind === "asset" && (
                <AssetPicker
                  grant={grant}
                  onChange={(scopeId) => update(index, { ...grant, scopeId })}
                />
              )}
            </div>
          )}
          <PermissionEditor
            value={grant.permissions}
            roles={roles}
            onChange={(permissions) => update(index, { ...grant, permissions })}
          />
        </section>
      ))}
      <ErrorMessage error={environments.error} />
      <LoadMore list={environments} />
      <Button
        className={styles.addGrant}
        variant="subtle"
        leftSection={<Plus size={15} />}
        onClick={() =>
          onChange([
            ...value,
            { scopeKind: "environment", scopeId: "", permissions: ["read"] },
          ])
        }
      >
        添加授权范围
      </Button>
    </div>
  );
}

function AssetPicker({
  grant,
  onChange,
}: {
  grant: Grant;
  onChange: (scope: string) => void;
}) {
  const [environment, asset] = grant.scopeId.split("/");
  const details = useQuery({
    queryKey: ["environment", environment],
    queryFn: () => api.environment(environment),
    enabled: Boolean(environment),
  });
  return (
    <>
      <Select
        label="资产"
        searchable
        value={asset || null}
        data={
          (details.data?.appliedSpec ?? details.data?.spec)?.assets.map(
            (a) => ({ value: a.id, label: a.name }),
          ) ?? []
        }
        onChange={(id) => onChange(`${environment}/${id}`)}
      />
      <ErrorMessage error={details.error} />
    </>
  );
}
