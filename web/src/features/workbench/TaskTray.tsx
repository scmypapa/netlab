import { Button } from "@mantine/core";
import {
  ChevronDown,
  ChevronUp,
  CircleCheck,
  ListChecks,
  RotateCcw,
} from "lucide-react";
import { useEffect, useState } from "react";
import type { EnvironmentSpec, Operation } from "../../api/client";
import { actionLabels, dateTime } from "../../foundation/format";
import { Status } from "../../foundation/Status";
import { LoadMore, type CursorPagination } from "../../foundation/LoadMore";
import styles from "./TaskTray.module.css";

const phaseLabels: Record<string, string> = {
  pending: "等待调度",
  scheduling: "分配资源",
  placing: "分配资源",
  network: "配置网络",
  services: "应用服务入口",
  vpn: "配置 VPN",
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
  "backup-complete": "备份完成",
  "prepare-backup-templates": "恢复模板",
  "recovery-quiesce": "暂停写入",
  "recovery-capture": "保存恢复点",
  "recovery-resume": "恢复运行",
  "migration-prepare": "准备目标节点",
  "migration-stop": "停止源资产",
  "migration-capture": "保存迁移数据",
  "migration-copy": "传输数据",
  "migration-apply": "应用资产配置",
  "migration-activate": "启动目标资产",
  "migration-release-capture": "清理迁移数据",
  "migration-transfer": "迁移虚拟机",
  "migration-verify": "确认目标运行",
  "migration-commit": "交接节点归属",
  "migration-cleanup": "清理源节点",
  "migration-services": "更新访问入口",
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
  const recent = operation
    ? [operation, ...operations.filter((item) => item.id !== operation.id)]
    : operations;
  const latest = recent[0];
  const shown = recent.find((item) => item.id === selected) ?? latest;
  const active = recent.filter((item) =>
    ["queued", "running"].includes(item.state),
  ).length;
  useEffect(() => {
    if (latest?.state === "failed") setOpen(true);
  }, [latest?.id, latest?.state]);
  return (
    <section className={styles.tray} aria-label="运行任务">
      <button
        className={styles.toggle}
        onClick={() => setOpen(!open)}
        aria-expanded={open}
      >
        <span>
          <ListChecks size={16} />
          任务
          <span
            className={styles.count}
            aria-label={`${recent.length} 项近期任务`}
          >
            {recent.length}
          </span>
          {latest && (
            <>
              <span className={styles.divider} />
              {actionLabels[latest.kind] ?? latest.kind}
              <Status value={latest.state} />
              {["queued", "running"].includes(latest.state) && (
                <span className={styles.phase}>
                  {phaseLabels[latest.phase] ?? latest.phase}
                </span>
              )}
            </>
          )}
        </span>
        <span>
          {active > 0 && (
            <span className={styles.active}>{active} 项执行中</span>
          )}
          {latest && !active && (
            <time className={styles.timestamp}>
              {dateTime(latest.updatedAt)}
            </time>
          )}
          {latest &&
            latest.total > 0 &&
            ["running", "queued"].includes(latest.state) && (
              <span className={styles.progress}>
                {latest.completed} / {latest.total}
              </span>
            )}
          {open ? <ChevronDown size={16} /> : <ChevronUp size={16} />}
        </span>
      </button>
      {open && (
        <div className={styles.content}>
          <div className={styles.list} role="group" aria-label="任务列表">
            {recent.length ? (
              recent.map((item) => (
                <button
                  key={item.id}
                  aria-current={shown?.id === item.id}
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
              <div className={styles.empty}>
                <CircleCheck size={20} />
                暂无任务
              </div>
            )}
            <LoadMore list={pagination} />
          </div>
          {shown && (
            <div className={styles.detail}>
              <div className={styles.detailHeading}>
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
              {shown.error && (
                <div className={styles.error} role="alert">
                  {shown.error}
                </div>
              )}
              {shown.total > 0 && (
                <div
                  className={styles.completion}
                  role="progressbar"
                  aria-label="任务进度"
                  aria-valuenow={shown.completed}
                  aria-valuemax={shown.total}
                  aria-valuemin={0}
                >
                  <span
                    style={{
                      width: `${(shown.completed / shown.total) * 100}%`,
                    }}
                  />
                </div>
              )}
              <div className={styles.results}>
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
