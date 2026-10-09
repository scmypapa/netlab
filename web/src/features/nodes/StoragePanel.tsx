import {
  Badge,
  Button,
  Menu,
  Modal,
  MultiSelect,
  PasswordInput,
  Progress,
  SegmentedControl,
  Select,
  Tabs,
  TextInput,
} from "@mantine/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, Circle, Database, HardDrive, Plus } from "lucide-react";
import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { api, type Node, type Schema } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { Status } from "../../foundation/Status";
import { useCursorList } from "../../foundation/useCursorList";
import { MigrationDialog } from "../workbench/MigrationDialog";
import { CephDetails } from "./CephDetails";
import { StorageDeviceDialog } from "./StorageDeviceDialog";
import { VolumePanel } from "./VolumePanel";
import styles from "./StorageWorkspace.module.css";

function poolStatus(pool: Schema<"StoragePool">) {
  return pool.error
    ? "failed"
    : pool.operationState === "queued" || pool.operationState === "running"
      ? "preparing"
      : (pool.state ?? "ready");
}

export function StoragePanel() {
  const [search] = useSearchParams();
  const client = useQueryClient();
  const pools = useQuery({
    queryKey: ["storage-pools"],
    queryFn: api.storagePools,
    refetchInterval: 5000,
  });
  const nodes = useCursorList(["nodes", "storage"], (page) => api.nodes(page), {
    refetchInterval: 15000,
  });
  useEffect(() => {
    if (nodes.hasNextPage && !nodes.isFetching) void nodes.fetchNextPage();
  }, [nodes.hasNextPage, nodes.isFetching, nodes.fetchNextPage]);
  const [selected, setSelected] = useState<string | undefined>(
    search.get("pool") ?? undefined,
  );
  const [adding, setAdding] = useState(false);
  const [disks, setDisks] = useState(false);
  const [removing, setRemoving] = useState(false);
  const [name, setName] = useState("");
  const [directory, setDirectory] = useState("");
  const [driver, setDriver] = useState<Schema<"StorageDriver">>("directory");
  const [nodeIds, setNodeIds] = useState<string[]>([]);
  const [monitors, setMonitors] = useState("");
  const [cephPool, setCephPool] = useState("");
  const [user, setUser] = useState("netlab");
  const [key, setKey] = useState("");
  const [migration, setMigration] = useState<Schema<"StoragePoolAsset">>();
  const items = pools.data ?? [];
  const active =
    items.find((p) => p.id === selected) ??
    items.find((p) => p.managed) ??
    items[0];
  const shared = pools.data?.find((p) => p.managed);
  const ready = (nodes.data ?? []).filter(
    (n) => n.state === "ready" && n.capabilities.includes("vm"),
  );
  const assets = useQuery({
    queryKey: ["storage-assets", active?.id],
    queryFn: () => api.storagePoolAssets(active!.id),
    enabled: Boolean(active),
    refetchInterval: 15000,
  });
  const migrationEnvironment = useQuery({
    queryKey: ["environment", migration?.environmentId],
    queryFn: () => api.environment(migration!.environmentId),
    enabled: Boolean(migration),
  });
  const migrationAsset = migrationEnvironment.data?.appliedSpec?.assets.find(
    (asset) => asset.id === migration?.assetId,
  );
  const refresh = () => {
    void client.invalidateQueries({ queryKey: ["storage-pools"] });
    void client.invalidateQueries({ queryKey: ["nodes"] });
    void client.invalidateQueries({ queryKey: ["storage-assets"] });
  };
  const create = useMutation({
    mutationFn: () =>
      api.createStoragePool({
        nodeIds,
        name,
        driver,
        ...(driver === "directory"
          ? { directory }
          : {
              ceph: {
                monitors: monitors.split(/[\s,]+/).filter(Boolean),
                pool: cephPool,
                user,
                key,
              },
            }),
      }),
    onSuccess: (pool) => {
      setSelected(pool.id);
      setAdding(false);
      setName("");
      setDirectory("");
      setKey("");
      refresh();
    },
  });
  const remove = useMutation({
    mutationFn: () => api.deleteStoragePool(active!.id),
    onSuccess: () => {
      setRemoving(false);
      refresh();
    },
  });
  const retry = useMutation({
    mutationFn: api.retryOperation,
    onSuccess: refresh,
  });
  return (
    <section>
      <div className="collection-toolbar">
        <div className="section-label">
          <Database size={17} />
          存储池
        </div>
        <div className={styles.label}>
          <Button
            variant="default"
            size="compact-sm"
            onClick={() => setDisks(true)}
          >
            选择专用盘
          </Button>
          <Button
            variant="default"
            size="compact-sm"
            leftSection={<Plus size={15} />}
            onClick={() => {
              create.reset();
              setNodeIds([]);
              setAdding(true);
            }}
          >
            接入存储
          </Button>
        </div>
      </div>
      <ErrorMessage error={pools.error ?? nodes.error ?? retry.error} />
      {!shared &&
        !nodes.isPending &&
        !nodes.hasNextPage &&
        !pools.isPending &&
        !pools.error &&
        !nodes.error && (
          <section className={styles.setup}>
            <div className={styles.heading}>
              <h2>Ceph 共享存储</h2>
              <Badge variant="light" color="gray">
                未启用
              </Badge>
            </div>
            <div className={styles.checks}>
              <div className={styles.check}>
                {ready.length >= 2 ? (
                  <CheckCircle2 size={18} color="var(--accent)" />
                ) : (
                  <Circle size={18} />
                )}
                两个就绪的 KVM 节点
              </div>
              <div className={styles.check}>
                {ready.some((n) => n.storageDevice) ? (
                  <CheckCircle2 size={18} color="var(--accent)" />
                ) : (
                  <Circle size={18} />
                )}
                至少一块专用盘
              </div>
            </div>
          </section>
        )}
      {pools.isPending ? (
        <Loading />
      ) : (
        <div className={styles.workspace}>
          <div className={styles.pools} role="group" aria-label="选择存储池">
            {items.map((pool) => (
              <button
                key={pool.id}
                aria-pressed={active?.id === pool.id}
                onClick={() => setSelected(pool.id)}
                className={`${styles.pool} ${active?.id === pool.id ? styles.selected : ""}`}
              >
                <span className={styles.label}>
                  {pool.driver === "rbd" ? (
                    <Database size={17} />
                  ) : (
                    <HardDrive size={17} />
                  )}
                  <strong>{pool.default ? "本地存储" : pool.name}</strong>
                </span>
                <span className={styles.meta}>
                  {pool.driver === "rbd"
                    ? pool.managed
                      ? "自动 Ceph"
                      : "外部 Ceph"
                    : nodes.data?.find((n) => n.id === pool.nodeIds[0])?.name}
                </span>
                <Status value={poolStatus(pool)} />
              </button>
            ))}
          </div>
          {active ? (
            <div className={styles.detail}>
              <div className={styles.heading}>
                <div className={styles.label}>
                  <h2>{active.default ? "本地存储" : active.name}</h2>
                  <Badge variant="light">
                    {active.driver === "rbd" ? "虚拟机" : "虚拟机与容器"}
                  </Badge>
                </div>
                <div className={styles.label}>
                  {active.error &&
                    active.operationId &&
                    active.operationState === "failed" && (
                      <Button
                        size="compact-sm"
                        variant="default"
                        loading={retry.isPending}
                        onClick={() => retry.mutate(active.operationId!)}
                      >
                        重试 {active.name}
                      </Button>
                    )}
                  {!active.default &&
                    active.operationState !== "queued" &&
                    active.operationState !== "running" && (
                      <Menu position="bottom-end">
                        <Menu.Target>
                          <Button size="compact-sm" variant="subtle">
                            管理存储池
                          </Button>
                        </Menu.Target>
                        <Menu.Dropdown>
                          <Menu.Item
                            color="red"
                            onClick={() => {
                              remove.reset();
                              setRemoving(true);
                            }}
                          >
                            移除存储池
                          </Menu.Item>
                        </Menu.Dropdown>
                      </Menu>
                    )}
                </div>
              </div>
              {active.error && <ErrorMessage error={new Error(active.error)} />}
              {active.operationState &&
                active.operationState !== "succeeded" && (
                  <div className={styles.task}>
                    <Status value={active.operationState} />
                    <span>存储任务</span>
                  </div>
                )}
              {active.storage && (
                <>
                  <div className={styles.metrics}>
                    <div>
                      <strong>
                        {Math.floor(
                          (active.storage.capacityBytes -
                            active.storage.availableBytes) /
                            2 ** 30,
                        )}{" "}
                        GiB
                      </strong>
                      <span>实际使用</span>
                    </div>
                    <div>
                      <strong>
                        {Math.floor(active.storage.availableBytes / 2 ** 30)}{" "}
                        GiB
                      </strong>
                      <span>可用空间</span>
                    </div>
                    <div>
                      <strong>{active.allocatedGiB} GiB</strong>
                      <span>已分配</span>
                    </div>
                  </div>
                  <Progress
                    aria-label="存储使用率"
                    value={
                      100 *
                      (1 -
                        active.storage.availableBytes /
                          active.storage.capacityBytes)
                    }
                    size={5}
                    mb="lg"
                  />
                  <div className={styles.meta}>
                    总计 {Math.floor(active.storage.capacityBytes / 2 ** 30)}{" "}
                    GiB{active.directory && ` · ${active.directory}`}
                  </div>
                </>
              )}
              <Tabs
                defaultValue="overview"
                key={active.id}
                mt="lg"
                keepMounted={false}
              >
                <Tabs.List>
                  <Tabs.Tab value="overview">概览</Tabs.Tab>
                  <Tabs.Tab value="assets">环境磁盘</Tabs.Tab>
                  <Tabs.Tab value="volumes">持久卷</Tabs.Tab>
                  <Tabs.Tab value="members">节点</Tabs.Tab>
                  {active.managed && (
                    <Tabs.Tab value="services">Ceph 服务</Tabs.Tab>
                  )}
                </Tabs.List>
                <Tabs.Panel value="overview" pt="lg">
                  {active.managed ? (
                    active.state === "ready" ? (
                      <CephDetails pool={active} />
                    ) : (
                      <div>共享池未就绪</div>
                    )
                  ) : (
                    <div className={styles.label}>
                      <Badge variant="light">
                        {active.driver === "rbd"
                          ? "共享虚拟磁盘"
                          : "节点本地存储"}
                      </Badge>
                      {active.storage?.nativeSnapshots && (
                        <Badge variant="light">原生快照</Badge>
                      )}
                    </div>
                  )}
                </Tabs.Panel>
                <Tabs.Panel value="assets" pt="lg">
                  <ErrorMessage error={assets.error} />
                  {assets.isPending ? (
                    <Loading />
                  ) : (
                    <div className={styles.table}>
                      <table className="data-table">
                        <thead>
                          <tr>
                            <th>资产</th>
                            <th>环境</th>
                            <th>容量</th>
                            <th>状态</th>
                            <th />
                          </tr>
                        </thead>
                        <tbody>
                          {assets.data?.map((asset) => (
                            <tr key={asset.environmentId + asset.assetId}>
                              <td>{asset.assetName}</td>
                              <td>
                                <Link
                                  className={styles.link}
                                  to={`/environments/${asset.environmentId}`}
                                >
                                  {asset.environmentName}
                                </Link>
                              </td>
                              <td>{asset.sizeGiB} GiB</td>
                              <td>
                                <Status value={asset.state} />
                              </td>
                              <td>
                                <Button
                                  variant="subtle"
                                  size="compact-sm"
                                  onClick={() => setMigration(asset)}
                                >
                                  迁移
                                </Button>
                              </td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                      {!assets.data?.length && (
                        <Empty
                          icon={<HardDrive size={24} />}
                          title="暂无环境磁盘"
                        />
                      )}
                    </div>
                  )}
                </Tabs.Panel>
                <Tabs.Panel value="volumes" pt="lg">
                  <VolumePanel storagePoolId={active.id} />
                </Tabs.Panel>
                <Tabs.Panel value="members" pt="lg">
                  <div className={styles.table}>
                    <table className="data-table">
                      <thead>
                        <tr>
                          <th>节点</th>
                          <th>角色</th>
                          <th>状态</th>
                        </tr>
                      </thead>
                      <tbody>
                        {(nodes.data ?? [])
                          .filter((n) => active.nodeIds.includes(n.id))
                          .map((n) => (
                            <tr key={n.id}>
                              <td>{n.name}</td>
                              <td>
                                {active.managed && n.storageDevice
                                  ? "存储与计算"
                                  : "计算"}
                              </td>
                              <td>
                                <Status value={n.state ?? "unknown"} />
                              </td>
                            </tr>
                          ))}
                      </tbody>
                    </table>
                  </div>
                </Tabs.Panel>
                {active.managed && (
                  <Tabs.Panel value="services" pt="lg">
                    <CephDetails pool={active} view="services" />
                  </Tabs.Panel>
                )}
              </Tabs>
            </div>
          ) : (
            <Empty icon={<Database size={28} />} title="暂无存储" />
          )}
        </div>
      )}
      {disks && (
        <StorageDeviceDialog
          nodes={nodes.data ?? []}
          onClose={() => setDisks(false)}
        />
      )}
      {migration && migrationEnvironment.data && migrationAsset && (
        <MigrationDialog
          id={migration.environmentId}
          asset={migrationAsset}
          revision={migrationEnvironment.data.revision}
          onClose={() => setMigration(undefined)}
          onSubmitted={refresh}
        />
      )}
      {migration && (
        <ErrorMessage
          error={
            migrationEnvironment.error ??
            (migrationEnvironment.data && !migrationAsset
              ? new Error("运行计划中未找到该资产，请刷新环境磁盘")
              : null)
          }
        />
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
            onChange={(v) => {
              setDriver(v as Schema<"StorageDriver">);
              setNodeIds([]);
            }}
            data={[
              { label: "目录", value: "directory" },
              { label: "已有 Ceph", value: "rbd" },
            ]}
          />
          <TextInput
            label="名称"
            required
            value={name}
            onChange={(e) => setName(e.currentTarget.value)}
          />
          {driver === "directory" ? (
            <Select
              label="节点"
              required
              value={nodeIds[0] ?? null}
              onChange={(id) => setNodeIds(id ? [id] : [])}
              data={(nodes.data ?? []).map((n) => ({
                value: n.id,
                label: n.name,
              }))}
            />
          ) : (
            <MultiSelect
              label="节点"
              required
              value={nodeIds}
              onChange={setNodeIds}
              data={(nodes.data ?? [])
                .filter((n) => n.capabilities.includes("vm"))
                .map((n) => ({ value: n.id, label: n.name }))}
            />
          )}
          {driver === "directory" ? (
            <TextInput
              label="节点上的目录"
              required
              value={directory}
              onChange={(e) => setDirectory(e.currentTarget.value)}
            />
          ) : (
            <>
              <TextInput
                label="MON 地址"
                required
                value={monitors}
                onChange={(e) => setMonitors(e.currentTarget.value)}
              />
              <TextInput
                label="Ceph 存储池"
                required
                value={cephPool}
                onChange={(e) => setCephPool(e.currentTarget.value)}
              />
              <TextInput
                label="客户端"
                required
                value={user}
                onChange={(e) => setUser(e.currentTarget.value)}
              />
              <PasswordInput
                label="客户端密钥"
                required
                value={key}
                autoComplete="off"
                onChange={(e) => setKey(e.currentTarget.value)}
              />
            </>
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
            <Button
              type="submit"
              disabled={!nodeIds.length}
              loading={create.isPending}
            >
              接入
            </Button>
          </div>
        </form>
      </Modal>
      <Modal
        opened={removing}
        onClose={() => setRemoving(false)}
        title={`移除 ${active?.name ?? "存储池"}`}
        centered
        size="sm"
      >
        <ErrorMessage error={remove.error} />
        <div className="dialog-actions">
          <Button variant="default" onClick={() => setRemoving(false)}>
            取消
          </Button>
          <Button
            color="red"
            loading={remove.isPending}
            onClick={() => remove.mutate()}
          >
            移除
          </Button>
        </div>
      </Modal>
    </section>
  );
}

export function NodeStorageSummary({ node }: { node: Node }) {
  const pools = useQuery({
    queryKey: ["storage-pools"],
    queryFn: api.storagePools,
    refetchInterval: 5000,
  });
  return (
    <>
      <ErrorMessage error={pools.error} />
      {pools.isPending ? (
        <Loading />
      ) : (
        <div className={styles.pools}>
          {pools.data
            ?.filter((p) => p.nodeIds.includes(node.id))
            .map((pool) => (
              <Link
                key={pool.id}
                className={`${styles.pool} ${styles.link}`}
                to={`/resources/storage?pool=${encodeURIComponent(pool.id)}`}
              >
                <strong>{pool.default ? "本地存储" : pool.name}</strong>
                <span>
                  {pool.storage
                    ? `${Math.floor(pool.storage.availableBytes / 2 ** 30)} GiB 可用`
                    : "容量暂不可用"}
                </span>
                <Status value={poolStatus(pool)} />
              </Link>
            ))}
        </div>
      )}
    </>
  );
}
