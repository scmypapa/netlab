import { Button, TextInput } from "@mantine/core";
import { useDebouncedValue } from "@mantine/hooks";
import { useQuery } from "@tanstack/react-query";
import { Layers3, Plus, Search } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { dateTime } from "../../foundation/format";
import { Status } from "../../foundation/Status";
import { LoadMore } from "../../foundation/LoadMore";
import { useCursorList } from "../../foundation/useCursorList";
import { CreateEnvironmentDialog } from "./CreateEnvironmentDialog";
import { allowsProject } from "../access/permissions";
import styles from "./Environments.module.css";

export function EnvironmentsPage() {
  const identity = useQuery({ queryKey: ["identity"], queryFn: api.identity });
  const canCreate = allowsProject(identity.data, "compose");
  const [query, setQuery] = useState("");
  const [filter, setFilter] = useState("all");
  const [search] = useDebouncedValue(query, 250);
  const status = filter === "all" ? "" : filter;
  const environments = useCursorList(
    ["environments", { search, status }],
    (page) => api.environments({ ...page, search, status }),
  );
  const [creating, setCreating] = useState(false);
  const items = environments.data ?? [];
  return (
    <main className="collection-page">
      <div className="page-heading">
        <div>
          <h1>环境</h1>
        </div>
        {canCreate && (
          <Button
            leftSection={<Plus size={16} />}
            onClick={() => setCreating(true)}
          >
            新建环境
          </Button>
        )}
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
          <table className={`data-table ${styles.table}`}>
            <thead>
              <tr>
                <th>环境</th>
                <th>状态</th>
                <th>资产</th>
                <th>网段</th>
                <th>最近更新</th>
              </tr>
            </thead>
            <tbody>
              {items.map((environment) => (
                <tr key={environment.id}>
                  <td className={styles.environment}>
                    <Link
                      className="object-link"
                      to={`/environments/${environment.id}`}
                    >
                      <span className={styles.symbol}>
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
                  <td className={styles.state}>
                    <Status value={environment.status} />
                  </td>
                  <td className={styles.assetCount}>
                    {environment.assetCount}
                    <span> 资产</span>
                  </td>
                  <td className={styles.networkCount}>
                    {environment.networkCount}
                    <span> 网段</span>
                  </td>
                  <td className={styles.updated}>
                    {dateTime(environment.updatedAt)}
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
            title={query || filter !== "all" ? "没有匹配的环境" : "暂无环境"}
            action={
              canCreate && !query && filter === "all" ? "新建环境" : undefined
            }
            onAction={() => setCreating(true)}
          />
        )
      )}
      <LoadMore list={environments} />
      {creating && (
        <CreateEnvironmentDialog onClose={() => setCreating(false)} />
      )}
    </main>
  );
}
