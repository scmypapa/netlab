import { Button, Drawer, TextInput } from "@mantine/core";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useDebouncedValue } from "@mantine/hooks";
import { Box, Boxes, Monitor, Plus, Search } from "lucide-react";
import { useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { memory } from "../../foundation/format";
import { Status } from "../../foundation/Status";
import { LoadMore } from "../../foundation/LoadMore";
import { useCursorList } from "../../foundation/useCursorList";
import { BlueprintsPanel } from "./BlueprintsPanel";
import { TemplateDetails } from "./TemplateDetails";
import { TemplateImportForm } from "./TemplateImport";

export function TemplatesPage() {
  const [params, setParams] = useSearchParams();
  const section =
    params.get("tab") === "environments" ? "environments" : "assets";
  return (
    <main className="collection-page">
      <div className="page-heading">
        <h1>模板</h1>
        <div className="view-switch" role="group" aria-label="模板分类">
          {[
            ["assets", "资产模板"],
            ["environments", "环境模板"],
          ].map(([value, label]) => (
            <button
              key={value}
              className={section === value ? "selected" : ""}
              aria-pressed={section === value}
              onClick={() =>
                setParams(value === "assets" ? {} : { tab: value })
              }
            >
              {label}
            </button>
          ))}
        </div>
      </div>
      {section === "assets" ? <AssetTemplatesPanel /> : <BlueprintsPanel />}
    </main>
  );
}

function AssetTemplatesPanel() {
  const [kind, setKind] = useState("all");
  const [query, setQuery] = useState("");
  const [search] = useDebouncedValue(query, 250);
  const selectedKind = kind === "all" ? "" : kind;
  const templates = useCursorList(
    ["templates", { search, kind: selectedKind }],
    (page) => api.templates({ ...page, search, kind: selectedKind }),
    {
      refetchInterval: (items) =>
        items.some((template) => template.state === "importing") ? 2500 : false,
    },
  );
  const [creating, setCreating] = useState(false);
  const [selected, setSelected] = useState<string>();
  const identity = useQuery({ queryKey: ["identity"], queryFn: api.identity });
  const client = useQueryClient();
  const items = templates.data ?? [];
  const detail = items.find((template) => template.id === selected);
  return (
    <>
      <div className="collection-toolbar">
        <div className="filter-tabs" role="group" aria-label="模板类型">
          {[
            ["all", "全部模板"],
            ["vm", "虚拟机"],
            ["container", "容器"],
          ].map(([value, label]) => (
            <button
              key={value}
              className={kind === value ? "selected" : ""}
              onClick={() => setKind(value)}
              aria-pressed={kind === value}
            >
              {label}
            </button>
          ))}
        </div>
        <TextInput
          aria-label="搜索模板"
          placeholder="搜索模板"
          leftSection={<Search size={16} />}
          value={query}
          onChange={(event) => setQuery(event.currentTarget.value)}
        />
        {identity.data?.administrator && (
          <Button
            leftSection={<Plus size={16} />}
            onClick={() => setCreating(true)}
          >
            导入模板
          </Button>
        )}
      </div>
      <ErrorMessage error={templates.error} />
      {templates.isPending ? (
        <Loading />
      ) : items.length ? (
        <div className="table-surface">
          <table className="data-table">
            <thead>
              <tr>
                <th>模板</th>
                <th>系统</th>
                <th>默认规格</th>
                <th>格式</th>
                <th>状态</th>
              </tr>
            </thead>
            <tbody>
              {items.map((template) => (
                <tr key={template.id}>
                  <td>
                    <button
                      type="button"
                      className="object-link object-link-button"
                      onClick={() => setSelected(template.id)}
                    >
                      <span className={`object-symbol ${template.kind}`}>
                        {template.kind === "vm" ? (
                          <Monitor size={20} />
                        ) : (
                          <Box size={20} />
                        )}
                      </span>
                      <span>
                        <strong>{template.name}</strong>
                        <span className="secondary-line">
                          {template.kind === "vm" ? "虚拟机" : "容器"} · v
                          {template.version}
                        </span>
                      </span>
                    </button>
                  </td>
                  <td>{template.os}</td>
                  <td className="numeric">
                    {template.resources.cpu} 核
                    <span className="muted">
                      {" "}
                      · {memory(template.resources.memoryMiB)} ·{" "}
                      {template.resources.diskGiB} GiB
                    </span>
                  </td>
                  <td className="muted">
                    {template.format?.toUpperCase() ??
                      (template.kind === "container" ? "OCI" : "—")}
                  </td>
                  <td>
                    <Status value={template.state ?? "importing"} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        !templates.error && (
          <Empty
            icon={<Boxes size={30} />}
            title={
              query || kind !== "all"
                ? "没有匹配的模板"
                : "导入可复用的资产模板"
            }
            action={
              !query && kind === "all" && identity.data?.administrator
                ? "导入模板"
                : undefined
            }
            onAction={() => setCreating(true)}
          />
        )
      )}
      <LoadMore list={templates} />
      <Drawer
        opened={creating}
        onClose={() => setCreating(false)}
        title="导入资产模板"
        position="right"
        size={500}
      >
        <TemplateImportForm
          onCreated={() => {
            setCreating(false);
            void client.invalidateQueries({ queryKey: ["templates"] });
          }}
        />
      </Drawer>
      {detail && (
        <TemplateDetails
          template={detail}
          onClose={() => setSelected(undefined)}
        />
      )}
    </>
  );
}
