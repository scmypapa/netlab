import { Button, Drawer, Modal, TextInput, Tabs } from "@mantine/core";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useDebouncedValue } from "@mantine/hooks";
import { Cpu, Plus, Search } from "lucide-react";
import { lazy, Suspense, useEffect, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { api, type Node } from "../../api/client";
import { NodeStorageSummary, StoragePanel } from "./StoragePanel";
import { VolumePanel } from "./VolumePanel";
import { BackupRepositoryPanel } from "./BackupRepositoryPanel";
import { RetirementPanel } from "./RetirementPanel";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { NodeInventory } from "./NodeInventory";
import { LoadMore } from "../../foundation/LoadMore";
import { useCursorList } from "../../foundation/useCursorList";
import { Status } from "../../foundation/Status";
import styles from "./NodeInventory.module.css";

const PerformanceWorkspace = lazy(
  () => import("../workbench/PerformanceWorkspace"),
);

export function NodesPage() {
  const storageView = useLocation().pathname === "/resources/storage";
  const performanceView = useLocation().pathname === "/resources/performance";
  const navigate = useNavigate();
  const client = useQueryClient();
  const [query, setQuery] = useState("");
  const [search] = useDebouncedValue(query, 250);
  const nodeSearch = performanceView ? "" : search;
  const nodes = useCursorList(
    ["nodes", { search: nodeSearch }],
    (page) => api.nodes({ ...page, search: nodeSearch }),
    { refetchInterval: 15_000, enabled: !storageView },
  );
  const [adding, setAdding] = useState(false);
  const [selected, setSelected] = useState<Node>();
  useEffect(() => {
    if (storageView) setSelected(undefined);
  }, [storageView]);
  const [name, setName] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const register = useMutation({
    mutationFn: () => api.registerNode({ name, endpoint }),
    onSuccess: () => {
      setAdding(false);
      void client.invalidateQueries({ queryKey: ["nodes"] });
    },
  });
  const items = nodes.data ?? [];
  return (
    <main className="collection-page">
      <div className="page-heading">
        <div>
          <h1>资源</h1>
        </div>
        {!storageView && (
          <Button
            leftSection={<Plus size={16} />}
            onClick={() => setAdding(true)}
          >
            接入节点
          </Button>
        )}
      </div>
      <Tabs
        value={
          storageView ? "storage" : performanceView ? "performance" : "nodes"
        }
        mb="lg"
        onChange={(value) =>
          navigate(
            value === "storage"
              ? "/resources/storage"
              : value === "performance"
                ? "/resources/performance"
                : "/resources",
          )
        }
      >
        <Tabs.List>
          <Tabs.Tab value="nodes">计算节点</Tabs.Tab>
          <Tabs.Tab value="performance">集群性能</Tabs.Tab>
          <Tabs.Tab value="storage">存储</Tabs.Tab>
        </Tabs.List>
      </Tabs>
      {performanceView ? (
        <>
          <Suspense fallback={<Loading />}>
            <PerformanceWorkspace nodes={items} onNode={setSelected} />
          </Suspense>
          <LoadMore list={nodes} />
        </>
      ) : storageView ? (
        <StoragePanel />
      ) : (
        <>
          <div className="collection-toolbar">
            <TextInput
              aria-label="搜索节点"
              placeholder="搜索节点"
              leftSection={<Search size={16} />}
              value={query}
              onChange={(event) => setQuery(event.currentTarget.value)}
            />
          </div>
          <ErrorMessage error={nodes.error} />
          {nodes.isPending ? (
            <Loading />
          ) : items.length ? (
            <NodeInventory nodes={items} onSelect={setSelected} />
          ) : (
            !nodes.error && (
              <Empty
                icon={<Cpu size={30} />}
                title={query ? "没有匹配的节点" : "暂无计算节点"}
                action={!query ? "接入节点" : undefined}
                onAction={() => setAdding(true)}
              />
            )
          )}
          <LoadMore list={nodes} />
        </>
      )}
      <Drawer
        opened={Boolean(selected)}
        onClose={() => setSelected(undefined)}
        title={selected?.name}
        position="right"
        size={840}
        closeButtonProps={{ "aria-label": "关闭节点" }}
      >
        {selected && (
          <>
            <div className={styles.drawerHeader}>
              <div>
                <strong>
                  {selected.capacity.cpu} 核 ·{" "}
                  {selected.capacity.memoryMiB / 1024} GiB 内存 ·{" "}
                  {selected.capacity.diskGiB} GiB 存储
                </strong>
                <span>{selected.endpoint}</span>
              </div>
              <Status value={selected.state ?? "unknown"} />
            </div>
            <Tabs defaultValue="performance" keepMounted={false}>
              <Tabs.List mb="md">
                <Tabs.Tab value="performance">性能</Tabs.Tab>
                <Tabs.Tab value="storage">存储池</Tabs.Tab>
                <Tabs.Tab value="volumes">数据卷</Tabs.Tab>
                <Tabs.Tab value="backups">备份仓库</Tabs.Tab>
                <Tabs.Tab value="retirement">退出节点</Tabs.Tab>
              </Tabs.List>
              <Tabs.Panel value="performance">
                <Suspense fallback={<Loading />}>
                  <PerformanceWorkspace node={selected} />
                </Suspense>
              </Tabs.Panel>
              <Tabs.Panel value="storage">
                <NodeStorageSummary node={selected} />
              </Tabs.Panel>
              <Tabs.Panel value="backups">
                <BackupRepositoryPanel node={selected} />
              </Tabs.Panel>
              <Tabs.Panel value="volumes">
                <VolumePanel node={selected} />
              </Tabs.Panel>
              <Tabs.Panel value="retirement">
                <RetirementPanel
                  node={selected}
                  onRemoved={() => setSelected(undefined)}
                />
              </Tabs.Panel>
            </Tabs>
          </>
        )}
      </Drawer>
      <Modal
        opened={adding}
        onClose={() => setAdding(false)}
        title="接入计算节点"
        centered
        size="sm"
      >
        <form
          className="form-stack"
          onSubmit={(event) => {
            event.preventDefault();
            register.mutate();
          }}
        >
          <TextInput
            label="节点名称"
            required
            value={name}
            onChange={(event) => setName(event.currentTarget.value)}
          />
          <TextInput
            label="节点服务地址"
            placeholder="https://node.example.com:9443"
            required
            value={endpoint}
            onChange={(event) => setEndpoint(event.currentTarget.value)}
          />
          <ErrorMessage error={register.error} />
          <div className="dialog-actions">
            <Button variant="default" onClick={() => setAdding(false)}>
              取消
            </Button>
            <Button type="submit" loading={register.isPending}>
              接入节点
            </Button>
          </div>
        </form>
      </Modal>
    </main>
  );
}
