import { useQuery } from "@tanstack/react-query";
import { RingProgress, Progress } from "@mantine/core";
import { Cpu, MemoryStick, Server } from "lucide-react";
import { api, type Node } from "../../api/client";
import { latestMetric } from "../../foundation/metrics";
import { ErrorMessage } from "../../foundation/Feedback";
import { memory, reading } from "../../foundation/format";
import { Status } from "../../foundation/Status";
import styles from "./NodeCards.module.css";

export function NodeCards({
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
      <div className={styles.grid}>
        {nodes.map((node) => {
          const cpu = latestMetric(history, "cpu", node.id);
          const ram = latestMetric(history, "memory", node.id);
          const cpuTotal = latestMetric(history, "cpu_capacity", node.id);
          const memoryTotal = latestMetric(history, "memory_capacity", node.id);
          const usage =
            cpu !== undefined && cpuTotal ? (cpu / cpuTotal) * 100 : undefined;
          return (
            <button
              key={node.id}
              className={styles.card}
              onClick={() => onSelect(node)}
            >
              <div className={styles.heading}>
                <Server size={18} />
                <strong>{node.name}</strong>
                <Status value={node.state ?? "unknown"} />
              </div>
              <div className={styles.readings}>
                <div className={styles.dial}>
                  <RingProgress
                    size={108}
                    thickness={6}
                    roundCaps
                    rootColor="var(--line)"
                    sections={
                      usage === undefined
                        ? []
                        : [
                            {
                              value: Math.min(100, usage),
                              color: "var(--accent)",
                            },
                          ]
                    }
                  />
                  <span>
                    <strong>{reading(cpu)}</strong>
                    <small>CPU · 核</small>
                  </span>
                </div>
                <div className={styles.memory}>
                  <span>
                    <MemoryStick size={15} />
                    内存
                  </span>
                  <strong>
                    {reading(ram, 2 ** 30)}
                    <small>GiB</small>
                  </strong>
                  <Progress
                    size={4}
                    color="var(--accent)"
                    value={
                      memoryTotal && ram !== undefined
                        ? Math.min(100, (ram / memoryTotal) * 100)
                        : 0
                    }
                  />
                  <span>总计 {reading(memoryTotal, 2 ** 30)} GiB</span>
                </div>
              </div>
              <div className={styles.footer}>
                <span>
                  <Cpu size={13} />
                  已分配 {node.reserved.cpu} 核 ·{" "}
                  {memory(node.reserved.memoryMiB)}
                </span>
                <span>
                  {node.capabilities
                    .filter((item) => item === "vm" || item === "container")
                    .map((item) => (item === "vm" ? "KVM" : "OCI"))
                    .join(" / ")}
                </span>
              </div>
            </button>
          );
        })}
      </div>
    </>
  );
}
