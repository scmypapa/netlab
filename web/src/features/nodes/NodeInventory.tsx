import { useQuery } from "@tanstack/react-query";
import { Progress } from "@mantine/core";
import { ChevronRight, Server } from "lucide-react";
import { api, type Node } from "../../api/client";
import { latestMetric } from "../../foundation/metrics";
import { ErrorMessage } from "../../foundation/Feedback";
import { memory, reading } from "../../foundation/format";
import { Status } from "../../foundation/Status";
import styles from "./NodeInventory.module.css";

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
          <span>CPU 使用</span>
          <span>内存使用</span>
          <span>已分配</span>
          <span />
        </div>
        {nodes.map((node) => {
          const cpu = latestMetric(history, "cpu", node.id);
          const ram = latestMetric(history, "memory", node.id);
          const cpuTotal = latestMetric(history, "cpu_capacity", node.id);
          const ramTotal = latestMetric(history, "memory_capacity", node.id);
          return (
            <button
              key={node.id}
              className={styles.row}
              onClick={() => onSelect(node)}
            >
              <span className={styles.name}>
                <Server size={21} />
                <span>
                  <strong>{node.name}</strong>
                  <span>
                    {node.capabilities
                      .filter((item) => item === "vm" || item === "container")
                      .map((item) => (item === "vm" ? "KVM" : "OCI"))
                      .join(" / ")}
                  </span>
                </span>
              </span>
              <Status value={node.state ?? "unknown"} />
              <span className={styles.load}>
                <span>
                  <strong>{reading(cpu)}</strong>
                  <span> / {reading(cpuTotal)} 核</span>
                </span>
                <Progress
                  aria-label={`${node.name} CPU 使用率`}
                  size={4}
                  value={
                    cpu !== undefined && cpuTotal
                      ? Math.min(100, (cpu / cpuTotal) * 100)
                      : 0
                  }
                  color="var(--accent)"
                />
              </span>
              <span className={styles.load}>
                <span>
                  <strong>{reading(ram, 2 ** 30)}</strong>
                  <span> / {reading(ramTotal, 2 ** 30)} GiB</span>
                </span>
                <Progress
                  aria-label={`${node.name} 内存使用率`}
                  size={4}
                  value={
                    ram !== undefined && ramTotal
                      ? Math.min(100, (ram / ramTotal) * 100)
                      : 0
                  }
                  color="var(--chart-2)"
                />
              </span>
              <span className={styles.reserved} aria-label="已分配">
                {node.reserved.cpu} 核{" "}
                <span>· {memory(node.reserved.memoryMiB)}</span>
              </span>
              <ChevronRight size={17} className={styles.arrow} />
            </button>
          );
        })}
      </div>
    </>
  );
}
