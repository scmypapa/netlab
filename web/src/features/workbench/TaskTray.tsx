import { Button } from "@mantine/core";
import {
  ChevronDown,
  ChevronUp,
  CircleCheck,
  ListChecks,
  RotateCcw,
} from "lucide-react";
import { useState } from "react";
import type { EnvironmentSpec, Operation } from "../../api/client";
import { actionLabels, dateTime } from "../../foundation/format";
import { Status } from "../../foundation/Status";
import { LoadMore, type CursorPagination } from "../../foundation/LoadMore";

const phaseLabels: Record<string, string> = {
  pending: "等待调度",
  scheduling: "分配资源",
  placing: "分配资源",
  network: "配置网络",
  services: "应用服务入口",
  "revoke-services": "撤销服务入口",
  prepare: "准备资产",
  activate: "启动资产",
  complete: "完成",
  completed: "完成",
  failed: "执行失败",
  cleanup: "清理资源",
  rollback: "恢复变更",
  destroy: "销毁资产",
  queued: "等待执行",
  running: "执行中",
};

export function TaskTray({
  operations,
  spec,
  operation,
  pagination,
  retrying,
  onRetry,
}: {
  operations: Operation[];
  spec: EnvironmentSpec;
  operation?: Operation;
  pagination: CursorPagination;
  retrying: boolean;
  onRetry: (id: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [selected, setSelected] = useState<string>();
  const latest = operation ?? operations[0];
  const shown = operations.find((item) => item.id === selected) ?? latest;
  return (
    <section className={`task-tray ${open ? "is-open" : ""}`}>
      <button
        className="task-tray-toggle"
        onClick={() => setOpen(!open)}
        aria-expanded={open}
      >
        <span>
          <ListChecks size={16} />
          任务
          {latest && (
            <>
              <span className="task-divider" />
              {actionLabels[latest.kind] ?? latest.kind}
              <Status value={latest.state} />
            </>
          )}
        </span>
        <span>
          {latest && ["running", "queued"].includes(latest.state) && (
            <span className="task-progress">
              {latest.completed} / {latest.total}
            </span>
          )}
          {open ? <ChevronDown size={16} /> : <ChevronUp size={16} />}
        </span>
      </button>
      {open && (
        <div className="task-tray-content">
          <div className="task-list">
            {operations.length ? (
              operations.map((item) => (
                <button
                  key={item.id}
                  className={shown?.id === item.id ? "selected" : ""}
                  onClick={() => setSelected(item.id)}
                >
                  <span>
                    {actionLabels[item.kind] ?? item.kind}
                    <Status value={item.state} />
                  </span>
                  <time>{dateTime(item.createdAt)}</time>
                </button>
              ))
            ) : (
              <div className="task-empty">
                <CircleCheck size={20} />
                暂无任务
              </div>
            )}
            <LoadMore list={pagination} />
          </div>
          {shown && (
            <div className="task-detail">
              <div className="task-detail-heading">
                <strong>{actionLabels[shown.kind] ?? shown.kind}</strong>
                <span>{phaseLabels[shown.phase] ?? shown.phase}</span>
                <span>
                  {shown.completed} / {shown.total}
                </span>
                {shown.retryable && (
                  <Button
                    variant="default"
                    size="compact-xs"
                    leftSection={<RotateCcw size={13} />}
                    loading={retrying}
                    onClick={() => onRetry(shown.id)}
                  >
                    重试
                  </Button>
                )}
              </div>
              {shown.error && <div className="error-inline">{shown.error}</div>}
              {shown.total > 0 && (
                <div className="task-completion">
                  <span
                    style={{
                      width: `${(shown.completed / shown.total) * 100}%`,
                    }}
                  />
                </div>
              )}
              <div className="task-results">
                {shown.results?.map((result) => (
                  <div key={result.assetId}>
                    <span>
                      {spec.assets.find((asset) => asset.id === result.assetId)
                        ?.name ?? "已移除资产"}
                    </span>
                    <Status value={result.state} />
                    {result.error && <p>{result.error}</p>}
                  </div>
                ))}
              </div>
            </div>
          )}
        </div>
      )}
    </section>
  );
}
