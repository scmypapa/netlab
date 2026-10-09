import { Button } from "@mantine/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { useEffect } from "react";
import { api, type Node } from "../../api/client";
import { ErrorMessage, Loading } from "../../foundation/Feedback";
import { Status } from "../../foundation/Status";

const phases: Record<string, string> = {
  queued: "等待迁出",
  "retire-assets": "迁移资产",
  "retire-network": "迁移网络入口",
  "retire-artifacts": "迁移镜像",
  "retire-storage": "退出存储集群",
  "retire-storage-cleanup": "清理节点存储登记",
  "retire-complete": "移除节点",
};

export function RetirementPanel({
  node,
  onRemoved,
}: {
  node: Node;
  onRemoved: () => void;
}) {
  const client = useQueryClient();
  const plan = useQuery({
    queryKey: ["node-retirement", node.id],
    queryFn: () => api.nodeRetirement(node.id),
  });
  const refresh = () => {
    void client.invalidateQueries({ queryKey: ["nodes"] });
    void plan.refetch();
  };
  const retire = useMutation({
    mutationFn: () => api.retireNode(node.id),
    onSuccess: refresh,
  });
  const resume = useMutation({
    mutationFn: () => api.resumeNodeScheduling(node.id),
    onSuccess: refresh,
  });
  const retry = useMutation({
    mutationFn: () => api.retryOperation(plan.data!.operation!.id),
    onSuccess: refresh,
  });
  const operation = useQuery({
    queryKey: ["operation", retire.data?.id ?? plan.data?.operation?.id],
    queryFn: () => api.operation((retire.data ?? plan.data!.operation!).id),
    enabled: Boolean(retire.data ?? plan.data?.operation),
    refetchInterval: (query) =>
      query.state.data?.state === "queued" ||
      query.state.data?.state === "running"
        ? 1500
        : false,
  });
  useEffect(() => {
    if (operation.data && operation.data.state !== "succeeded")
      void client.invalidateQueries({ queryKey: ["node-retirement", node.id] });
  }, [operation.data?.phase, operation.data?.state, client, node.id]);
  if (operation.data?.state === "succeeded")
    return (
      <div className="form-stack">
        <strong>节点已退出</strong>
        <Button
          onClick={() => {
            void client.invalidateQueries({ queryKey: ["nodes"] });
            void client.invalidateQueries({ queryKey: ["storage-pools"] });
            onRemoved();
          }}
        >
          完成
        </Button>
      </div>
    );
  if (plan.isPending) return <Loading />;
  const current = operation.data ?? plan.data?.operation;
  const busy = current?.state === "queued" || current?.state === "running";
  const storageDeparture = [
    "retire-storage",
    "retire-storage-cleanup",
    "retire-complete",
  ].includes(current?.phase ?? "");
  return (
    <div className="form-stack">
      <ErrorMessage
        error={
          plan.error ??
          operation.error ??
          retire.error ??
          resume.error ??
          retry.error
        }
      />
      {plan.data && (
        <>
          <h3>{plan.data.draining ? "正在退出节点" : "退出节点"}</h3>
          {current && current.phase !== "cancelled" && (
            <div>
              <Status value={current.state} /> {phases[current.phase]}
            </div>
          )}
          {current?.error && current.phase !== "cancelled" && (
            <ErrorMessage error={new Error(current.error)} />
          )}
          {plan.data.assets.length > 0 && (
            <div className="table-surface">
              <table className="data-table">
                <thead>
                  <tr>
                    <th>环境</th>
                    <th>待迁出资产</th>
                  </tr>
                </thead>
                <tbody>
                  {plan.data.assets.map((asset) => (
                    <tr key={`${asset.environmentId}/${asset.assetId}`}>
                      <td>
                        <Link to={`/environments/${asset.environmentId}`}>
                          {asset.environmentName}
                        </Link>
                      </td>
                      <td>{asset.assetName}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {plan.data.blockers.length > 0 && (
            <ul>
              {plan.data.blockers.map((item) => (
                <li key={item}>{item}</li>
              ))}
            </ul>
          )}
          <div className="dialog-actions">
            {plan.data.draining ? (
              <>
                <Button
                  variant="default"
                  disabled={busy || storageDeparture}
                  loading={resume.isPending}
                  onClick={() => resume.mutate()}
                >
                  恢复调度
                </Button>
                {current?.retryable && (
                  <Button
                    loading={retry.isPending}
                    onClick={() => retry.mutate()}
                  >
                    继续退出
                  </Button>
                )}
              </>
            ) : (
              <Button
                color="red"
                disabled={plan.data.blockers.length > 0}
                loading={retire.isPending}
                onClick={() => retire.mutate()}
              >
                迁出资产并退出
              </Button>
            )}
          </div>
        </>
      )}
    </div>
  );
}
