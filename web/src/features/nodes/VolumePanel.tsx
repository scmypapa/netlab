import {
  ActionIcon,
  Button,
  Menu,
  Modal,
  NumberInput,
  SegmentedControl,
  Select,
  TextInput,
} from "@mantine/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Database, MoreHorizontal, Plus } from "lucide-react";
import { useState } from "react";
import { api, type Node, type Schema } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { Status } from "../../foundation/Status";

export function VolumePanel({
  node,
  storagePoolId,
}: {
  node?: Node;
  storagePoolId?: string;
}) {
  const client = useQueryClient();
  const volumes = useQuery({
    queryKey: ["volumes"],
    queryFn: api.volumes,
    refetchInterval: 3000,
  });
  const pools = useQuery({
    queryKey: ["storage-pools"],
    queryFn: api.storagePools,
  });
  const [editing, setEditing] = useState<"create" | "resize" | "delete">();
  const [selected, setSelected] = useState<Schema<"PersistentVolume">>();
  const [name, setName] = useState("");
  const [kind, setKind] = useState<Schema<"TemplateKind">>("vm");
  const [pool, setPool] = useState<string | null>(
    storagePoolId ?? (node ? `default:${node.id}` : null),
  );
  const [size, setSize] = useState(10);
  const refresh = () => {
    void client.invalidateQueries({ queryKey: ["volumes"] });
    void client.invalidateQueries({ queryKey: ["storage-pools"] });
    void client.invalidateQueries({ queryKey: ["nodes"] });
  };
  const apply = useMutation({
    mutationFn: () =>
      editing === "create"
        ? api.createVolume({ name, kind, storagePoolId: pool!, sizeGiB: size })
        : editing === "resize"
          ? api.resizeVolume(selected!.id, size)
          : api.deleteVolume(selected!.id),
    onSuccess: () => {
      setEditing(undefined);
      refresh();
    },
  });
  const retry = useMutation({
    mutationFn: api.retryOperation,
    onSuccess: refresh,
  });
  const items = (volumes.data ?? []).filter((volume) =>
    storagePoolId
      ? volume.storagePoolId === storagePoolId
      : !node ||
        volume.nodeId === node.id ||
        pools.data?.some(
          (pool) =>
            pool.id === volume.storagePoolId && pool.nodeIds.includes(node!.id),
        ),
  );
  const open = (mode: typeof editing, volume?: Schema<"PersistentVolume">) => {
    apply.reset();
    setSelected(volume);
    setSize(volume?.sizeGiB ?? 10);
    setName("");
    if (mode === "create") {
      setKind("vm");
      setPool(
        storagePoolId ??
          pools.data?.find(
            (p) =>
              (!node || p.nodeIds.includes(node.id)) &&
              p.driver === "rbd" &&
              p.state === "ready" &&
              !p.error,
          )?.id ??
          (node ? `default:${node.id}` : null),
      );
    }
    setEditing(mode);
  };
  return (
    <section>
      <div className="collection-toolbar">
        <div className="section-label">
          <Database size={17} />
          数据卷
        </div>
        <Button
          variant="default"
          size="compact-sm"
          leftSection={<Plus size={15} />}
          onClick={() => open("create")}
        >
          创建数据卷
        </Button>
      </div>
      <ErrorMessage error={volumes.error ?? retry.error} />
      {volumes.isPending ? (
        <Loading />
      ) : items.length ? (
        <div className="table-surface">
          <table className="data-table">
            <thead>
              <tr>
                <th>数据卷</th>
                <th>容量</th>
                <th>状态</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {items.map((volume) => (
                <tr key={volume.id}>
                  <td>
                    <strong>{volume.name}</strong>
                    <span className="secondary-line">
                      {
                        pools.data?.find(
                          (pool) => pool.id === volume.storagePoolId,
                        )?.name
                      }{" "}
                      · {volume.kind === "vm" ? "虚拟磁盘" : "容器目录"}
                    </span>
                  </td>
                  <td>{volume.sizeGiB} GiB</td>
                  <td>
                    {volume.error ? (
                      <ErrorMessage error={new Error(volume.error)} />
                    ) : (
                      <Status value={volume.state} />
                    )}
                    {volume.references.length > 0 && (
                      <span className="secondary-line">
                        {volume.references.join("、").replaceAll("环境：", "")}
                      </span>
                    )}
                  </td>
                  <td>
                    <Menu withinPortal position="bottom-end">
                      <Menu.Target>
                        <ActionIcon
                          variant="subtle"
                          aria-label={`操作 ${volume.name}`}
                        >
                          <MoreHorizontal size={17} />
                        </ActionIcon>
                      </Menu.Target>
                      <Menu.Dropdown>
                        {volume.state === "failed" && volume.operationId && (
                          <Menu.Item
                            onClick={() => retry.mutate(volume.operationId!)}
                          >
                            重试
                          </Menu.Item>
                        )}
                        <Menu.Item
                          disabled={
                            volume.references.length > 0 ||
                            volume.state !== "ready"
                          }
                          onClick={() => open("resize", volume)}
                        >
                          扩容
                        </Menu.Item>
                        <Menu.Item
                          color="red"
                          disabled={
                            volume.references.length > 0 ||
                            !["ready", "failed"].includes(volume.state)
                          }
                          onClick={() => open("delete", volume)}
                        >
                          删除
                        </Menu.Item>
                      </Menu.Dropdown>
                    </Menu>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <Empty icon={<Database size={28} />} title="暂无数据卷" />
      )}
      <Modal
        opened={Boolean(editing)}
        onClose={() => setEditing(undefined)}
        title={
          editing === "create"
            ? "创建数据卷"
            : `${editing === "resize" ? "扩容" : "删除"} ${selected?.name ?? ""}`
        }
        centered
        size="sm"
      >
        <form
          className="form-stack"
          onSubmit={(event) => {
            event.preventDefault();
            apply.mutate();
          }}
        >
          {editing === "create" && (
            <>
              <TextInput
                label="名称"
                required
                value={name}
                onChange={(event) => setName(event.currentTarget.value)}
              />
              <SegmentedControl
                value={kind}
                onChange={(value) => {
                  setKind(value as Schema<"TemplateKind">);
                  if (
                    value === "container" &&
                    pools.data?.find((p) => p.id === pool)?.driver === "rbd"
                  )
                    setPool(null);
                }}
                data={[
                  { label: "虚拟磁盘", value: "vm" },
                  { label: "容器目录", value: "container" },
                ]}
              />
              <Select
                label="存储池"
                required
                value={pool}
                onChange={setPool}
                data={(pools.data ?? [])
                  .filter(
                    (pool) =>
                      (!node || pool.nodeIds.includes(node.id)) &&
                      !pool.error &&
                      (pool.default || pool.state === "ready") &&
                      (kind === "vm" || pool.driver === "directory"),
                  )
                  .map((pool) => ({ value: pool.id, label: pool.name }))}
              />
            </>
          )}
          {editing !== "delete" && (
            <NumberInput
              label="容量 · GiB"
              min={selected?.sizeGiB ?? 1}
              required
              value={size}
              onChange={(value) => setSize(Number(value))}
            />
          )}
          <ErrorMessage error={apply.error ?? pools.error} />
          <div className="dialog-actions">
            <Button variant="default" onClick={() => setEditing(undefined)}>
              取消
            </Button>
            <Button
              color={editing === "delete" ? "red" : undefined}
              type="submit"
              loading={apply.isPending}
              disabled={editing === "create" && !pool}
            >
              {editing === "create"
                ? "创建"
                : editing === "resize"
                  ? "扩容"
                  : "删除"}
            </Button>
          </div>
        </form>
      </Modal>
    </section>
  );
}
