import { Button, Modal, Select } from "@mantine/core";
import { useMutation, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Monitor } from "lucide-react";
import { api, type Asset } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { memory } from "../../foundation/format";

export function MigrationDialog({
  id,
  asset,
  revision,
  onClose,
  onSubmitted,
}: {
  id: string;
  asset: Asset;
  revision: number;
  onClose: () => void;
  onSubmitted: () => void;
}) {
  const [target, setTarget] = useState<string | null>(null);
  const [storage, setStorage] = useState<string | null>(null);
  const pools = useQuery({
    queryKey: ["storage-pools"],
    queryFn: api.storagePools,
  });
  const template = useQuery({
    queryKey: ["templates", asset.templateId],
    queryFn: () => api.templates({ ids: [asset.templateId] }),
  });
  const nodes = useQuery({
    queryKey: ["migration-destinations", id, asset.id],
    queryFn: () => api.migrationDestinations(id, asset.id),
  });
  const migration = useMutation({
    mutationFn: () =>
      api.migrateAsset(id, asset.id, {
        expectedRevision: revision,
        targetNodeId: target ?? undefined,
        targetStoragePoolId: storage ?? undefined,
        clientRequestId: crypto.randomUUID(),
      }),
    onSuccess: () => {
      onSubmitted();
      onClose();
    },
  });
  return (
    <Modal opened onClose={onClose} title={`迁移 ${asset.name}`} size="sm">
      {nodes.isPending ? (
        <Loading />
      ) : nodes.isError ? (
        <ErrorMessage error={nodes.error} />
      ) : nodes.data.length ? (
        <Select
          label="目标节点"
          placeholder="自动选择"
          clearable
          value={target}
          onChange={(id) => {
            setTarget(id);
            setStorage(null);
          }}
          disabled={migration.isPending}
          data={nodes.data.map((node) => ({
            value: node.id,
            label: `${node.name}${node.current ? "（当前节点）" : ""} · ${node.available.cpu} 核 / ${memory(node.available.memoryMiB)}`,
          }))}
        />
      ) : (
        <Empty icon={<Monitor size={24} />} title="暂无可用迁移节点" />
      )}
      {nodes.data?.length ? (
        <Select
          mt="md"
          label="目标存储池"
          placeholder="保留共享池或使用目标节点本地存储"
          clearable
          value={storage}
          onChange={setStorage}
          data={(pools.data ?? [])
            .filter(
              (pool) =>
                !pool.error &&
                (!nodes.data?.find((node) => node.id === target)?.current ||
                  pool.id !==
                    nodes.data.find((node) => node.id === target)
                      ?.sourceStoragePoolId) &&
                (pool.default || pool.state === "ready") &&
                (template.data?.[0]?.kind === "vm" ||
                  pool.driver === "directory") &&
                (target
                  ? pool.nodeIds.includes(target)
                  : pool.nodeIds.some((id) =>
                      nodes.data?.some((node) => node.id === id),
                    )),
            )
            .map((pool) => ({ value: pool.id, label: pool.name }))}
        />
      ) : null}
      <ErrorMessage error={migration.error ?? pools.error} />
      <div className="dialog-actions">
        <Button variant="default" onClick={onClose}>
          取消
        </Button>
        <Button
          disabled={
            !nodes.data?.length ||
            Boolean(
              nodes.data?.find((node) => node.id === target)?.current &&
              !storage,
            )
          }
          loading={migration.isPending}
          onClick={() => migration.mutate()}
        >
          {target
            ? nodes.data?.find((node) => node.id === target)?.live &&
              (!storage ||
                storage ===
                  nodes.data.find((node) => node.id === target)
                    ?.sourceStoragePoolId)
              ? "在线迁移"
              : "停机迁移"
            : "迁移"}
        </Button>
      </div>
    </Modal>
  );
}
