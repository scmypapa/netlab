import { ActionIcon, Button, Modal, TextInput } from "@mantine/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Database, Plus, RotateCw, Trash2 } from "lucide-react";
import { useState } from "react";
import { api, type Node, type Schema } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { Status } from "../../foundation/Status";

export function StoragePanel({ node }: { node: Node }) {
  const client = useQueryClient();
  const pools = useQuery({
    queryKey: ["storage-pools"],
    queryFn: api.storagePools,
    refetchInterval: 15000,
  });
  const [adding, setAdding] = useState(false);
  const [name, setName] = useState("");
  const [directory, setDirectory] = useState("");
  const [removing, setRemoving] = useState<Schema<"StoragePool">>();
  const refresh = () => {
    void client.invalidateQueries({ queryKey: ["storage-pools"] });
  };
  const create = useMutation({
    mutationFn: () =>
      api.createStoragePool({ nodeId: node.id, name, directory }),
    onSuccess: () => {
      setAdding(false);
      setName("");
      setDirectory("");
      refresh();
    },
  });
  const remove = useMutation({
    mutationFn: (pool: Schema<"StoragePool">) => api.deleteStoragePool(pool.id),
    onSuccess: () => {
      setRemoving(undefined);
      refresh();
    },
  });
  const retry = useMutation({
    mutationFn: api.retryOperation,
    onSuccess: refresh,
  });
  const items = pools.data?.filter((pool) => pool.nodeId === node.id) ?? [];
  return (
    <section>
      <div className="collection-toolbar">
        <div className="section-label">
          <Database size={17} />
          存储
        </div>
        <Button
          variant="default"
          size="compact-sm"
          leftSection={<Plus size={15} />}
          onClick={() => setAdding(true)}
        >
          接入存储
        </Button>
      </div>
      <ErrorMessage error={pools.error ?? retry.error} />
      {pools.isPending ? (
        <Loading />
      ) : items.length ? (
        <div className="table-surface">
          <table className="data-table">
            <thead>
              <tr>
                <th>存储池</th>
                <th>已分配</th>
                <th>可用 / 总计</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {items.map((pool) => (
                <tr key={pool.id}>
                  <td>
                    <strong>{pool.default ? "本地存储" : pool.name}</strong>
                    {pool.error ? (
                      <ErrorMessage error={new Error(pool.error)} />
                    ) : (
                      <span className="secondary-line">
                        {pool.directory}
                      </span>
                    )}
                    {pool.state === "deleting" && (
                      <Status value={pool.error ? "failed" : "deleting"} />
                    )}
                  </td>
                  <td>{pool.allocatedGiB} GiB</td>
                  <td>
                    {pool.storage
                      ? `${Math.floor(pool.storage.availableBytes / 2 ** 30)} / ${Math.floor(pool.storage.capacityBytes / 2 ** 30)} GiB`
                      : "—"}
                  </td>
                  <td>
                    {!pool.default &&
                      (pool.state === "deleting" ? (
                        pool.error &&
                        pool.operationId && (
                          <ActionIcon
                            variant="subtle"
                            aria-label={`重试删除 ${pool.name}`}
                            onClick={() => retry.mutate(pool.operationId!)}
                            loading={retry.isPending}
                          >
                            <RotateCw size={16} />
                          </ActionIcon>
                        )
                      ) : (
                        <ActionIcon
                          color="red"
                          variant="subtle"
                          aria-label={`移除 ${pool.name}`}
                          onClick={() => {
                            remove.reset();
                            setRemoving(pool);
                          }}
                        >
                          <Trash2 size={16} />
                        </ActionIcon>
                      ))}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <Empty icon={<Database size={28} />} title="暂无存储" />
      )}
      <Modal
        opened={adding}
        onClose={() => setAdding(false)}
        title="接入存储"
        centered
        size="sm"
      >
        <form
          className="form-stack"
          onSubmit={(event) => {
            event.preventDefault();
            create.mutate();
          }}
        >
          <TextInput
            label="名称"
            required
            value={name}
            onChange={(event) => setName(event.currentTarget.value)}
          />
          <TextInput
            label="节点上的目录"
            placeholder="/mnt/storage"
            required
            value={directory}
            onChange={(event) => setDirectory(event.currentTarget.value)}
          />
          <ErrorMessage error={create.error} />
          <div className="dialog-actions">
            <Button variant="default" onClick={() => setAdding(false)}>
              取消
            </Button>
            <Button type="submit" loading={create.isPending}>
              接入
            </Button>
          </div>
        </form>
      </Modal>
      <Modal
        opened={Boolean(removing)}
        onClose={() => setRemoving(undefined)}
        title={`移除 ${removing?.name ?? "存储池"}`}
        centered
        size="sm"
      >
        <ErrorMessage error={remove.error} />
        <div className="dialog-actions">
          <Button variant="default" onClick={() => setRemoving(undefined)}>
            取消
          </Button>
          <Button
            color="red"
            loading={remove.isPending}
            onClick={() => removing && remove.mutate(removing)}
          >
            移除
          </Button>
        </div>
      </Modal>
    </section>
  );
}
