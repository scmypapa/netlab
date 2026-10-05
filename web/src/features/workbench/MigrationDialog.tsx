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
  const nodes = useQuery({
    queryKey: ["migration-destinations", id, asset.id],
    queryFn: () => api.migrationDestinations(id, asset.id),
  });
  const migration = useMutation({
    mutationFn: () =>
      api.migrateAsset(id, asset.id, {
        expectedRevision: revision,
        targetNodeId: target ?? undefined,
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
          onChange={setTarget}
          disabled={migration.isPending}
          data={nodes.data.map((node) => ({
            value: node.id,
            label: `${node.name} · ${node.available.cpu} 核 / ${memory(node.available.memoryMiB)} · ${node.live ? "在线" : "停机"}`,
          }))}
        />
      ) : (
        <Empty icon={<Monitor size={24} />} title="暂无可用迁移节点" />
      )}
      <ErrorMessage error={migration.error} />
      <div className="dialog-actions">
        <Button variant="default" onClick={onClose}>
          取消
        </Button>
        <Button
          disabled={!nodes.data?.length}
          loading={migration.isPending}
          onClick={() => migration.mutate()}
        >
          {!target
            ? "迁移"
            : nodes.data?.find((node) => node.id === target)?.live
              ? "在线迁移"
              : "停机迁移"}
        </Button>
      </div>
    </Modal>
  );
}
