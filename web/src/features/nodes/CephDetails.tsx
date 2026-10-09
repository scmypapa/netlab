import { Badge, Button, Modal, NumberInput } from "@mantine/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api, type Schema } from "../../api/client";
import { ErrorMessage, Loading } from "../../foundation/Feedback";
import styles from "./StorageWorkspace.module.css";

export function CephDetails({
  pool,
  view = "overview",
}: {
  pool: Schema<"StoragePool">;
  view?: "overview" | "services";
}) {
  const client = useQueryClient();
  const status = useQuery({
    queryKey: ["ceph-status", pool.id],
    queryFn: () => api.cephStatus(pool.id),
    refetchInterval: 15000,
  });
  const [editing, setEditing] = useState(false);
  const [replicas, setReplicas] = useState(1);
  const configure = useMutation({
    mutationFn: () => api.configureCephPool(pool.id, replicas),
    onSuccess: () => {
      setEditing(false);
      void client.invalidateQueries({ queryKey: ["storage-pools"] });
      void client.invalidateQueries({ queryKey: ["ceph-status", pool.id] });
    },
  });
  if (status.isPending) return <Loading />;
  const value = status.data;
  return (
    <>
      <ErrorMessage error={status.error} />
      {value &&
        (view === "services" ? (
          <div className={styles.table}>
            <table className="data-table">
              <thead>
                <tr>
                  <th>服务</th>
                  <th>角色</th>
                  <th>节点</th>
                  <th>状态</th>
                </tr>
              </thead>
              <tbody>
                {value.daemons.map((d) => (
                  <tr key={d.name}>
                    <td>{d.name}</td>
                    <td>
                      {d.role === "mon"
                        ? "仲裁"
                        : d.role === "mgr"
                          ? "管理"
                          : d.role === "osd"
                            ? "数据存储"
                            : d.role}
                    </td>
                    <td>{d.host}</td>
                    <td>{d.state}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <>
            <div className={styles.heading}>
              <div className={styles.label}>
                <strong>集群健康</strong>
                <Badge
                  color={
                    value.health === "HEALTH_OK"
                      ? "teal"
                      : value.health === "HEALTH_WARN"
                        ? "yellow"
                        : "red"
                  }
                >
                  {value.health === "HEALTH_OK"
                    ? "正常"
                    : value.health === "HEALTH_WARN"
                      ? "警告"
                      : value.health === "HEALTH_ERR"
                        ? "异常"
                        : value.health}
                </Badge>
              </div>
              <Button
                size="compact-sm"
                variant="default"
                disabled={pool.operationState !== "succeeded"}
                onClick={() => {
                  configure.reset();
                  setReplicas(value.replicas);
                  setEditing(true);
                }}
              >
                副本设置
              </Button>
            </div>
            {value.messages.map((message) => (
              <div key={message} className={styles.meta}>
                {message}
              </div>
            ))}
            <div className={styles.metrics}>
              <div>
                <strong>
                  {value.osdsUp} / {value.osdsTotal}
                </strong>
                <span>在线数据盘</span>
              </div>
              <div>
                <strong>{value.replicas}</strong>
                <span>数据副本</span>
              </div>
              <div>
                <strong>{pool.nodeIds.length}</strong>
                <span>接入节点</span>
              </div>
            </div>
            <div className={styles.table}>
              <table className="data-table">
                <thead>
                  <tr>
                    <th>数据盘</th>
                    <th>节点</th>
                    <th>实际使用 / 总计</th>
                    <th>状态</th>
                  </tr>
                </thead>
                <tbody>
                  {value.disks.map((d) => (
                    <tr key={d.name}>
                      <td>{d.name}</td>
                      <td>{d.host}</td>
                      <td>
                        {Math.floor(d.usedBytes / 2 ** 30)} /{" "}
                        {Math.floor(d.capacityBytes / 2 ** 30)} GiB
                      </td>
                      <td>
                        {d.state === "up"
                          ? "在线"
                          : d.state === "down"
                            ? "离线"
                            : d.state}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </>
        ))}
      <Modal
        opened={editing}
        onClose={() => setEditing(false)}
        title="数据副本"
        centered
        size="sm"
      >
        <div className="form-stack">
          <NumberInput
            label="副本数量"
            min={1}
            max={3}
            value={replicas}
            onChange={(value) => setReplicas(Number(value))}
          />
          <ErrorMessage error={configure.error} />
          <div className="dialog-actions">
            <Button variant="default" onClick={() => setEditing(false)}>
              取消
            </Button>
            <Button
              loading={configure.isPending}
              onClick={() => configure.mutate()}
            >
              应用
            </Button>
          </div>
        </div>
      </Modal>
    </>
  );
}
