import { Button, Drawer, Modal, TextInput } from "@mantine/core";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useDebouncedValue } from "@mantine/hooks";
import { Cpu, Plus, Search, Server } from "lucide-react";
import { useState } from "react";
import { api, type Node } from "../../api/client";
import { StoragePanel } from "./StoragePanel";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { dateTime, memory } from "../../foundation/format";
import { Status } from "../../foundation/Status";
import { LoadMore } from "../../foundation/LoadMore";
import { useCursorList } from "../../foundation/useCursorList";

export function NodesPage() {
  const client = useQueryClient();
  const [query, setQuery] = useState("");
  const [search] = useDebouncedValue(query, 250);
  const nodes = useCursorList(
    ["nodes", { search }],
    (page) => api.nodes({ ...page, search }),
    { refetchInterval: 15_000 },
  );
  const [adding, setAdding] = useState(false);
  const [selected, setSelected] = useState<Node>();
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
          <h1>计算节点</h1>
        </div>
        <Button
          leftSection={<Plus size={16} />}
          onClick={() => setAdding(true)}
        >
          接入节点
        </Button>
      </div>
      <div className="collection-toolbar">
        <div className="section-label">
          <Server size={16} />
          计算节点
        </div>
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
        <div className="table-surface">
          <table className="data-table">
            <thead>
              <tr>
                <th>节点</th>
                <th>CPU · 已分配 / 总计</th>
                <th>内存 · 已分配 / 总计</th>
                <th>存储 · 已分配 / 总计</th>
                <th>运行能力</th>
                <th>状态</th>
              </tr>
            </thead>
            <tbody>
              {items.map((node) => (
                <tr key={node.id}>
                  <td>
                    <button
                      className="object-link text-link"
                      onClick={() => setSelected(node)}
                    >
                      <span className="object-symbol">
                        <Server size={20} />
                      </span>
                      <span>
                        <strong>{node.name}</strong>
                        <span className="secondary-line">
                          {dateTime(node.observedAt)}
                        </span>
                      </span>
                    </button>
                  </td>
                  <td>
                    <Capacity
                      used={node.reserved.cpu}
                      total={node.capacity.cpu}
                      text={`${node.reserved.cpu} / ${node.capacity.cpu} 核`}
                    />
                  </td>
                  <td>
                    <Capacity
                      used={node.reserved.memoryMiB}
                      total={node.capacity.memoryMiB}
                      text={`${memory(node.reserved.memoryMiB)} / ${memory(node.capacity.memoryMiB)}`}
                    />
                  </td>
                  <td>
                    <Capacity
                      used={node.reserved.diskGiB}
                      total={node.capacity.diskGiB}
                      text={`${node.reserved.diskGiB} / ${node.capacity.diskGiB} GiB`}
                    />
                  </td>
                  <td>
                    <div className="capabilities">
                      {node.capabilities.map((capability) => (
                        <span key={capability}>
                          {capability === "vm"
                            ? "KVM"
                            : capability === "container"
                              ? "OCI"
                              : capability === "network"
                                ? "网络"
                                : capability}
                        </span>
                      ))}
                    </div>
                  </td>
                  <td>
                    <Status value={node.state ?? "unknown"} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        !nodes.error && (
          <Empty
            icon={<Cpu size={30} />}
            title={query ? "没有匹配的节点" : "接入首个计算节点"}
            action={!query ? "接入节点" : undefined}
            onAction={() => setAdding(true)}
          />
        )
      )}
      <LoadMore list={nodes} />
      <Drawer
        opened={Boolean(selected)}
        onClose={() => setSelected(undefined)}
        title={selected?.name}
        position="right"
        size={680}
        closeButtonProps={{ "aria-label": "关闭节点" }}
      >
        {selected && <StoragePanel node={selected} />}
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

function Capacity({
  used,
  total,
  text,
}: {
  used: number;
  total: number;
  text: string;
}) {
  return (
    <div className="capacity">
      <span>{text}</span>
      <div className="capacity-track">
        <span
          style={{
            width: `${total ? Math.min(100, (used / total) * 100) : 0}%`,
          }}
        />
      </div>
    </div>
  );
}
