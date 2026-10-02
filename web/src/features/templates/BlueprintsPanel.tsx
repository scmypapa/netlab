import { ActionIcon, Button, Drawer, Select, TextInput } from "@mantine/core";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, Layers3, Network, Search } from "lucide-react";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { api, type Blueprint } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { dateTime } from "../../foundation/format";
import { CreateEnvironmentDialog } from "../environments/CreateEnvironmentDialog";

export function BlueprintsPanel() {
  const blueprints = useQuery({
    queryKey: ["blueprints"],
    queryFn: api.blueprints,
  });
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState<Blueprint>();
  const navigate = useNavigate();
  const items = (blueprints.data ?? []).filter((item) =>
    item.name.toLocaleLowerCase().includes(query.toLocaleLowerCase()),
  );
  return (
    <>
      <div className="collection-toolbar">
        <span className="section-label">
          <Layers3 size={17} />
          环境模板
        </span>
        <TextInput
          aria-label="搜索环境模板"
          placeholder="搜索模板"
          leftSection={<Search size={16} />}
          value={query}
          onChange={(event) => setQuery(event.currentTarget.value)}
        />
      </div>
      <ErrorMessage error={blueprints.error} />
      {blueprints.isPending ? (
        <Loading />
      ) : items.length ? (
        <div className="table-surface">
          <table className="data-table">
            <thead>
              <tr>
                <th>模板</th>
                <th>最新版本</th>
                <th>资产 / 网段</th>
                <th>最近更新</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {items.map((item) => (
                <tr key={item.id}>
                  <td>
                    <button
                      className="object-link object-link-button"
                      onClick={() => setSelected(item)}
                    >
                      <span className="object-symbol">
                        <Layers3 size={20} />
                      </span>
                      <strong>{item.name}</strong>
                    </button>
                  </td>
                  <td>v{item.latestVersion}</td>
                  <td className="numeric">
                    {item.assetCount}
                    <span className="muted"> / {item.networkCount}</span>
                  </td>
                  <td className="muted">{dateTime(item.updatedAt)}</td>
                  <td className="table-action">
                    <ActionIcon
                      variant="subtle"
                      color="gray"
                      aria-label={`查看${item.name}`}
                      onClick={() => setSelected(item)}
                    >
                      <ChevronRight size={18} />
                    </ActionIcon>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        !blueprints.error && (
          <Empty
            icon={<Layers3 size={30} />}
            title={query ? "没有匹配的模板" : "暂无环境模板"}
            action={query ? undefined : "打开环境"}
            onAction={() => navigate("/environments")}
          />
        )
      )}
      {selected && (
        <BlueprintDetails
          key={selected.id}
          blueprint={selected}
          onClose={() => setSelected(undefined)}
        />
      )}
    </>
  );
}

function BlueprintDetails({
  blueprint,
  onClose,
}: {
  blueprint: Blueprint;
  onClose: () => void;
}) {
  const [versionId, setVersionId] = useState(blueprint.latestVersionId);
  const [creating, setCreating] = useState(false);
  const versions = useQuery({
    queryKey: ["blueprint-versions", blueprint.id],
    queryFn: () => api.blueprintVersions(blueprint.id),
  });
  const version = useQuery({
    queryKey: ["blueprint-version", versionId],
    queryFn: () => api.blueprintVersion(versionId),
  });
  return (
    <>
      <Drawer
        opened
        onClose={onClose}
        title={blueprint.name}
        position="right"
        size={440}
      >
        <div className="form-stack">
          <Select
            label="版本"
            allowDeselect={false}
            value={versionId}
            data={(versions.data ?? []).map((item) => ({
              value: item.id,
              label: `v${item.version} · ${dateTime(item.createdAt)}`,
            }))}
            onChange={(value) => setVersionId(value!)}
          />
          <ErrorMessage error={versions.error ?? version.error} />
          {version.isPending ? (
            <Loading />
          ) : (
            version.data && (
              <>
                <div className="blueprint-scale">
                  <Layers3 size={18} />
                  <span>{version.data.assetCount} 个资产</span>
                  <span>{version.data.networkCount} 个网段</span>
                </div>
                <section className="blueprint-objects">
                  <h3>网段</h3>
                  {version.data.spec.networks.map((network) => (
                    <div key={network.id}>
                      <Network size={16} />
                      <strong>{network.name}</strong>
                      <span className="mono">{network.cidr}</span>
                    </div>
                  ))}
                  <h3>资产</h3>
                  {version.data.spec.assets.map((asset) => (
                    <div key={asset.id}>
                      <strong>{asset.name}</strong>
                      <span>
                        {asset.resources.cpu} 核 ·{" "}
                        {asset.resources.memoryMiB / 1024} GiB
                      </span>
                    </div>
                  ))}
                </section>
                <div className="drawer-footer">
                  <Button fullWidth onClick={() => setCreating(true)}>
                    创建环境
                  </Button>
                </div>
              </>
            )
          )}
        </div>
      </Drawer>
      {creating && (
        <CreateEnvironmentDialog
          blueprint={blueprint}
          versionId={versionId}
          onClose={() => setCreating(false)}
        />
      )}
    </>
  );
}
