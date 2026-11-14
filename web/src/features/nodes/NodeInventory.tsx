import { useQuery } from "@tanstack/react-query";
import { Progress } from "@mantine/core";
import {
  ArrowDown,
  ChevronRight,
  Cpu,
  Layers3,
  MemoryStick,
  Server,
} from "lucide-react";
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
    <section aria-label="节点资源">
      <ErrorMessage error={metrics.error} />
      <div className={styles.inventory}>
        {nodes.map((node) => (
          <article
            key={node.id}
            className={styles.nodeCard}
            aria-label={node.name}
          >
            <div className={styles.nodeHeading}>
              <button
                className={styles.nodeLink}
                aria-label={`查看${node.name}`}
                onClick={() => onSelect(node)}
              >
                <span className={styles.symbol}>
                  <Server size={20} />
                </span>
                <strong>{node.name}</strong>
                <ChevronRight size={16} />
              </button>
              <Status value={node.state ?? "unknown"} />
            </div>
            <fieldset
              className={styles.workloads}
              aria-label={`${node.name} 虚拟机与容器`}
            >
              <legend>
                <span>
                  CPU 分配上限{" "}
                  <strong>
                    {reading(node.capacity.cpu * node.cpuAllocationRatio)} 核
                  </strong>
                </span>
                <span>
                  允许超配{" "}
                  <strong>{reading(node.cpuAllocationRatio)} 倍</strong>
                </span>
              </legend>
              <h3>
                <Layers3 size={18} />
                虚拟机与容器
              </h3>
              <dl className={styles.specifications}>
                <div>
                  <dt>
                    <Cpu size={15} />
                    CPU
                  </dt>
                  <dd>
                    {reading(node.reserved.cpu)} <span>核</span>
                  </dd>
                </div>
                <div>
                  <dt>
                    <MemoryStick size={15} />
                    内存
                  </dt>
                  <dd>{memory(node.reserved.memoryMiB)}</dd>
                </div>
              </dl>
            </fieldset>
            <div className={styles.resourceLink}>
              <span />
              <ArrowDown size={16} />
              <span>共享节点资源</span>
            </div>
            <section
              className={styles.nodeResources}
              aria-label={`${node.name} 计算节点`}
            >
              <h3>
                <Server size={18} />
                计算节点
              </h3>
              <ResourceLoad
                name={`${node.name} CPU 使用率`}
                label="CPU"
                capacity={`${reading(node.capacity.cpu)} 核`}
                used={latestMetric(history, "cpu", node.id)}
                total={latestMetric(history, "cpu_capacity", node.id)}
                color="var(--accent)"
              />
              <ResourceLoad
                name={`${node.name} 内存使用率`}
                label="内存"
                capacity={memory(node.capacity.memoryMiB)}
                used={latestMetric(history, "memory", node.id)}
                total={latestMetric(history, "memory_capacity", node.id)}
                color="var(--chart-2)"
              />
            </section>
          </article>
        ))}
      </div>
    </section>
  );
}

function ResourceLoad({
  name,
  label,
  capacity,
  used,
  total,
  color,
}: {
  name: string;
  label: string;
  capacity: string;
  used: number | undefined;
  total: number | undefined;
  color: string;
}) {
  const ratio = used !== undefined && total ? used / total : undefined;
  const value = ratio === undefined ? "—" : percent.format(ratio);
  return (
    <div className={styles.load} role="group" aria-label={name}>
      <div className={styles.loadHeading}>
        <span>
          {label}
          <strong>{capacity}</strong>
        </span>
        <span>
          <span>使用率</span>
          <strong>{value}</strong>
        </span>
      </div>
      {ratio === undefined ? (
        <div className={styles.emptyTrack} />
      ) : (
        <Progress
          aria-label={name}
          value={Math.min(100, ratio * 100)}
          size={8}
          color={color}
        />
      )}
    </div>
  );
}
