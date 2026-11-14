import { useQuery } from "@tanstack/react-query";
import { Progress } from "@mantine/core";
import { ChevronRight, Server } from "lucide-react";
import { api, type Node } from "../../api/client";
import { latestMetric } from "../../foundation/metrics";
import { ErrorMessage } from "../../foundation/Feedback";
import { memory, reading } from "../../foundation/format";
import { Status } from "../../foundation/Status";
import styles from "./NodeInventory.module.css";

const percent = new Intl.NumberFormat("zh-CN", {
  style: "percent",
  maximumFractionDigits: 1,
});

export function NodeInventory({
  nodes,
  onSelect,
}: {
  nodes: Node[];
  onSelect: (node: Node) => void;
}) {
  const metrics = useQuery({
    queryKey: ["node-load"],
    queryFn: ({ signal }) => api.nodeMetrics(60, undefined, signal),
    refetchInterval: 10_000,
  });
  const history = metrics.isError ? undefined : metrics.data;
  return (
    <>
      <ErrorMessage error={metrics.error} />
      <div className={styles.inventory}>
        <div className={styles.columns} aria-hidden="true">
          <span>计算节点</span>
          <span>状态</span>
          <span>实时负载</span>
          <span>资产配置</span>
          <span />
        </div>
        {nodes.map((node) => (
          <button
            key={node.id}
            className={styles.row}
            onClick={() => onSelect(node)}
          >
            <span className={styles.name}>
              <span className={styles.symbol}>
                <Server size={20} />
              </span>
              <span>
                <strong>{node.name}</strong>
                <span>
                  {reading(node.capacity.cpu)} 核 CPU ·{" "}
                  {memory(node.capacity.memoryMiB)} 内存
                </span>
              </span>
            </span>
            <Status value={node.state ?? "unknown"} />
            <span
              className={styles.runtime}
              aria-label={`${node.name} 实时负载`}
            >
              <ResourceLoad
                name={`${node.name} CPU 使用率`}
                label="CPU"
                used={latestMetric(history, "cpu", node.id)}
                total={latestMetric(history, "cpu_capacity", node.id)}
                color="var(--accent)"
              />
              <ResourceLoad
                name={`${node.name} 内存使用率`}
                label="内存"
                used={latestMetric(history, "memory", node.id)}
                total={latestMetric(history, "memory_capacity", node.id)}
                color="var(--chart-2)"
              />
            </span>
            <span
              className={styles.allocation}
              aria-label={`${node.name} 资产配置`}
            >
              <span>
                <span>虚拟 CPU</span>
                <strong>
                  {reading(node.reserved.cpu)} <span>核</span>
                </strong>
              </span>
              <span>
                <span>内存</span>
                <strong>{memory(node.reserved.memoryMiB)}</strong>
              </span>
            </span>
            <ChevronRight size={17} className={styles.arrow} />
          </button>
        ))}
      </div>
    </>
  );
}

function ResourceLoad({
  name,
  label,
  used,
  total,
  color,
}: {
  name: string;
  label: string;
  used: number | undefined;
  total: number | undefined;
  color: string;
}) {
  const ratio = used !== undefined && total ? used / total : undefined;
  return (
    <span className={styles.load}>
      <span>
        <span>{label}</span>
        <strong>{ratio === undefined ? "—" : percent.format(ratio)}</strong>
      </span>
      {ratio === undefined ? (
        <span className={styles.emptyTrack} />
      ) : (
        <Progress
          aria-label={name}
          size={4}
          value={Math.min(100, ratio * 100)}
          color={color}
        />
      )}
    </span>
  );
}
