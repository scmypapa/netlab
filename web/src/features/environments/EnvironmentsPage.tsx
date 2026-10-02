import { ActionIcon, Button, TextInput } from "@mantine/core";
import { useQuery } from "@tanstack/react-query";
import { ArrowUpRight, Layers3, Plus, Search } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { dateTime } from "../../foundation/format";
import { Status } from "../../foundation/Status";
import { CreateEnvironmentDialog } from "./CreateEnvironmentDialog";

export function EnvironmentsPage() {
  const environments = useQuery({
    queryKey: ["environments"],
    queryFn: api.environments,
  });
  const [query, setQuery] = useState("");
  const [filter, setFilter] = useState("all");
  const [creating, setCreating] = useState(false);
  const items = (environments.data ?? []).filter(
    (item) =>
      item.status !== "destroyed" &&
      (filter === "all" || item.status === filter) &&
      `${item.name} ${item.externalReference ?? ""}`
        .toLocaleLowerCase()
        .includes(query.toLocaleLowerCase()),
  );
  return (
    <main className="collection-page">
      <div className="page-heading">
        <div>
          <h1>环境</h1>
        </div>
        <Button
          leftSection={<Plus size={16} />}
          onClick={() => setCreating(true)}
        >
          新建环境
        </Button>
      </div>
      <div className="collection-toolbar">
        <div className="filter-tabs" role="group" aria-label="环境状态">
          {[
            ["all", "全部环境"],
            ["running", "运行中"],
            ["stopped", "已停止"],
            ["draft", "待运行"],
          ].map(([value, label]) => (
            <button
              key={value}
              className={filter === value ? "selected" : ""}
              aria-pressed={filter === value}
              onClick={() => setFilter(value)}
            >
              {label}
              {value === "all" && environments.data && (
                <span>
                  {
                    environments.data.filter(
                      (item) => item.status !== "destroyed",
                    ).length
                  }
                </span>
              )}
            </button>
          ))}
        </div>
        <TextInput
          aria-label="搜索环境"
          placeholder="搜索环境"
          leftSection={<Search size={16} />}
          value={query}
          onChange={(event) => setQuery(event.currentTarget.value)}
        />
      </div>
      <ErrorMessage error={environments.error} />
      {environments.isPending ? (
        <Loading />
      ) : items.length ? (
        <div className="table-surface">
          <table className="data-table">
            <thead>
              <tr>
                <th>环境</th>
                <th>状态</th>
                <th>资产 / 网段</th>
                <th>最近更新</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {items.map((environment) => (
                <tr key={environment.id}>
                  <td>
                    <Link
                      className="object-link"
                      to={`/environments/${environment.id}`}
                    >
                      <span className="object-symbol">
                        <Layers3 size={20} />
                      </span>
                      <span>
                        <strong>{environment.name}</strong>
                        {environment.externalReference && (
                          <span className="secondary-line">
                            {environment.externalReference}
                          </span>
                        )}
                      </span>
                    </Link>
                  </td>
                  <td>
                    <Status value={environment.status} />
                  </td>
                  <td className="numeric">
                    {environment.spec.assets.length}
                    <span className="muted">
                      {" "}
                      / {environment.spec.networks.length}
                    </span>
                  </td>
                  <td className="muted">{dateTime(environment.updatedAt)}</td>
                  <td className="table-action">
                    <ActionIcon
                      component={Link}
                      to={`/environments/${environment.id}`}
                      variant="subtle"
                      color="gray"
                      aria-label={`打开${environment.name}`}
                    >
                      <ArrowUpRight size={18} />
                    </ActionIcon>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        !environments.error && (
          <Empty
            icon={<Layers3 size={30} />}
            title={
              query || filter !== "all"
                ? "没有匹配的环境"
                : "创建你的第一个环境"
            }
            action={!query && filter === "all" ? "新建环境" : undefined}
            onAction={() => setCreating(true)}
          />
        )
      )}
      {creating && (
        <CreateEnvironmentDialog onClose={() => setCreating(false)} />
      )}
    </main>
  );
}
