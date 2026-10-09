import {
  ActionIcon,
  Accordion,
  Badge,
  Button,
  Modal,
  MultiSelect,
  PasswordInput,
  SegmentedControl,
  TextInput,
} from "@mantine/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Database, Plus, RotateCw, Trash2 } from "lucide-react";
import { useState } from "react";
import { api, type Node, type Schema } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { Status } from "../../foundation/Status";
import { useCursorList } from "../../foundation/useCursorList";
import { LoadMore } from "../../foundation/LoadMore";

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
  const [driver, setDriver] = useState<Schema<"StorageDriver">>("directory");
  const [nodeIds, setNodeIds] = useState<string[]>([node.id]);
  const [monitors, setMonitors] = useState("");
  const [pool, setPool] = useState("");
  const [user, setUser] = useState("netlab");
  const [key, setKey] = useState("");
  const nodes = useCursorList(["nodes", "storage"], (page) => api.nodes(page), {
    refetchInterval: 15000,
  });
  const [removing, setRemoving] = useState<Schema<"StoragePool">>();
  const refresh = () => {
    void client.invalidateQueries({ queryKey: ["storage-pools"] });
  };
  const create = useMutation({
    mutationFn: () =>
      api.createStoragePool({
        nodeIds: driver === "directory" ? [node.id] : nodeIds,
        name,
        driver,
        ...(driver === "directory"
          ? { directory }
          : {
              ceph: {
                monitors: monitors.split(/[\s,]+/).filter(Boolean),
                pool,
                user,
                key,
              },
            }),
      }),
    onSuccess: () => {
      setAdding(false);
      setName("");
      setDirectory("");
      setKey("");
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
  const items =
    pools.data?.filter((pool) => pool.nodeIds.includes(node.id)) ?? [];
  const shared = pools.data?.find((pool) => pool.managed);
  const sharedReady =
    shared?.state === "ready" && shared.operationState === "succeeded";
  const vmNodes = (nodes.data ?? []).filter((item) =>
    item.capabilities.includes("vm"),
  );
  const readyNodes = vmNodes.filter((item) => item.state === "ready");
  const diskNodes = readyNodes.filter((item) => item.storageDevice);
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
          onClick={() => {
            create.reset();
            setNodeIds([node.id]);
            setAdding(true);
          }}
        >
          接入存储
        </Button>
      </div>
      <ErrorMessage error={pools.error ?? nodes.error ?? retry.error} />
      {!pools.isPending && !nodes.isPending && !pools.error && !nodes.error && (
        <Accordion variant="contained" mb="md">
          <Accordion.Item value="ceph">
            <Accordion.Control>
              <div className="section-label">
                <Database size={17} />
                Ceph 共享存储
                <Badge
                  size="sm"
                  variant="light"
                  color={shared?.error ? "red" : sharedReady ? "teal" : "gray"}
                >
                  {shared
                    ? shared.error
                      ? shared.operationState === "failed"
                        ? "配置失败"
                        : "连接异常"
                      : shared.state === "deleting"
                        ? "移除中"
                        : sharedReady
                          ? "已启用"
                          : "配置中"
                    : "未启用"}
                </Badge>
              </div>
            </Accordion.Control>
            <Accordion.Panel>
              {shared ? (
                <>
                  <ErrorMessage
                    error={shared.error ? new Error(shared.error) : undefined}
                  />
                  <p>
                    {shared.nodeIds.includes(node.id)
                      ? sharedReady
                        ? "此节点已接入共享池。"
                        : "正在配置共享池成员。"
                      : "此节点尚未接入共享池。"}
                  </p>
                  <p>
                    成员：
                    {shared.nodeIds
                      .map(
                        (id) =>
                          nodes.data?.find((item) => item.id === id)?.name ??
                          "未加载节点",
                      )
                      .join("、")}
                  </p>
                  {shared.operationState === "failed" && shared.operationId && (
                    <Button
                      size="compact-sm"
                      variant="default"
                      loading={retry.isPending}
                      onClick={() => retry.mutate(shared.operationId!)}
                    >
                      重试配置
                    </Button>
                  )}
                </>
              ) : (
                <>
                  <p>
                    {readyNodes.length >= 2 ? "✓" : "○"} 两个就绪的 KVM 节点
                  </p>
                  <p>
                    {diskNodes.length ? "✓" : "○"} 至少一个节点已指定 Ceph
                    专用盘
                  </p>
                  <p>
                    条件齐备后，登记节点即自动建池、配置凭据并接入共享存储。
                  </p>
                </>
              )}
              <table className="data-table">
                <thead>
                  <tr>
                    <th>节点</th>
                    <th>存储角色</th>
                    <th>专用盘</th>
                  </tr>
                </thead>
                <tbody>
                  {vmNodes.map((item) => (
                    <tr key={item.id}>
                      <td>
                        {item.name}
                        <Status value={item.state ?? "unknown"} />
                      </td>
                      <td>{item.storageDevice ? "存储与计算" : "计算"}</td>
                      <td>{item.storageDevice ?? "未指定"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              <LoadMore list={nodes} />
              <details>
                <summary>配置专用盘</summary>
                <p>
                  新节点安装时接受推荐空盘，或指定一块独立空盘；其余节点可选
                  none。
                </p>
                <pre
                  style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}
                >
                  sudo bash netlab-release/scripts/install-node.sh 主站IP
                  /root/netlab-node-certs /var/lib/netlab-node
                  /dev/disk/by-id/专用盘标识
                </pre>
                <p>
                  已有节点：安装
                  cephadm、podman、openssh-server、lvm2、chrony；在
                  /etc/netlab-node/node.env 的 NETLAB_NODE_ARGS 末尾添加
                  --storage-device /dev/disk/by-id/专用盘标识，启用 ssh 和
                  chrony 服务，重启 netlab-node 后重新登记同一节点地址。
                </p>
                <p>
                  专用盘交由 Ceph
                  初始化。当前数据目录和已挂载磁盘继续用作本地存储。
                </p>
                <p>
                  默认单副本。副本数由 Ceph
                  官方工具管理；模板基础盘共享，各环境写入层独立。
                </p>
              </details>
            </Accordion.Panel>
          </Accordion.Item>
        </Accordion>
      )}
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
                    {pool.driver === "rbd" && (
                      <Badge size="xs" variant="light">
                        {pool.managed ? "自动 Ceph" : "外部 Ceph"}
                      </Badge>
                    )}
                    {pool.storage?.nativeSnapshots && (
                      <Badge size="xs" variant="light">
                        原生快照
                      </Badge>
                    )}
                    {pool.error ? (
                      <ErrorMessage error={new Error(pool.error)} />
                    ) : (
                      <span className="secondary-line">{pool.directory}</span>
                    )}
                    {((pool.state && pool.state !== "ready") ||
                      pool.operationState === "queued" ||
                      pool.operationState === "running") && (
                      <Status
                        value={
                          pool.error
                            ? "failed"
                            : pool.state === "deleting"
                              ? "deleting"
                              : "preparing"
                        }
                      />
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
                      (pool.state !== "ready" ||
                      (pool.operationState &&
                        pool.operationState !== "succeeded") ? (
                        pool.error &&
                        pool.operationId && (
                          <ActionIcon
                            variant="subtle"
                            aria-label={`重试 ${pool.name}`}
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
        onClose={() => {
          setAdding(false);
          setKey("");
        }}
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
          <SegmentedControl
            value={driver}
            onChange={(value) => setDriver(value as Schema<"StorageDriver">)}
            data={[
              { label: "目录", value: "directory" },
              { label: "已有 Ceph", value: "rbd" },
            ]}
          />
          <TextInput
            label="名称"
            required
            value={name}
            onChange={(event) => setName(event.currentTarget.value)}
          />
          {driver === "rbd" ? (
            <>
              <MultiSelect
                label="节点"
                required
                value={nodeIds}
                onChange={setNodeIds}
                data={(nodes.data ?? [])
                  .filter((item) => item.capabilities.includes("vm"))
                  .map((item) => ({ value: item.id, label: item.name }))}
              />
              <TextInput
                label="MON 地址"
                placeholder="10.0.0.10:3300, 10.0.0.11:3300"
                required
                value={monitors}
                onChange={(event) => setMonitors(event.currentTarget.value)}
              />
              <TextInput
                label="Ceph 存储池"
                required
                value={pool}
                onChange={(event) => setPool(event.currentTarget.value)}
              />
              <TextInput
                label="客户端"
                required
                value={user}
                onChange={(event) => setUser(event.currentTarget.value)}
              />
              <PasswordInput
                label="客户端密钥"
                required
                value={key}
                onChange={(event) => setKey(event.currentTarget.value)}
                autoComplete="off"
              />
            </>
          ) : (
            <TextInput
              label="节点上的目录"
              placeholder="/mnt/storage"
              required
              value={directory}
              onChange={(event) => setDirectory(event.currentTarget.value)}
            />
          )}
          <ErrorMessage error={create.error} />
          <div className="dialog-actions">
            <Button
              variant="default"
              onClick={() => {
                setAdding(false);
                setKey("");
              }}
            >
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
