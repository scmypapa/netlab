import {
  ActionIcon,
  Button,
  Modal,
  MultiSelect,
  NumberInput,
  Select,
  TextInput,
} from "@mantine/core";
import {
  useMutation,
  useQueries,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  ArrowLeft,
  ArrowRight,
  Download,
  Pause,
  Radio,
  Trash2,
  X,
} from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { api, type Environment, type Schema } from "../../api/client";
import styles from "./TrafficWorkspace.module.css";

type Flow = Schema<"CaptureFlow">;
type Segment = Schema<"CaptureSegment">;
const activeCapture = (segment: Segment) =>
  segment.status === "starting" || segment.status === "running";
type Selection = { asset: string } | { source: string; destination: string };
const palette = [
  "#2eada0",
  "#6699e8",
  "#b58ae2",
  "#e7a148",
  "#dd7295",
  "#63aaa8",
];
const endpoint = (flow: Flow, side: "source" | "destination") =>
  flow[side === "source" ? "sourceAssetId" : "destinationAssetId"] ??
  flow[side];
const volume = (bytes: number) =>
  bytes >= 1048576
    ? (bytes / 1048576).toFixed(1) + " MiB"
    : bytes >= 1024
      ? (bytes / 1024).toFixed(1) + " KiB"
      : bytes + " B";
const stateTitle = {
  starting: "等待流量",
  running: "抓包中",
  stopped: "已结束",
  failed: "失败",
};

function CommunicationGraph({
  environment,
  flows,
  groups,
  colors,
  onSelect,
}: {
  environment: Environment;
  flows: Flow[];
  groups: Map<string, string>;
  colors: Map<string, string>;
  onSelect: (selection: Selection) => void;
}) {
  const host = useRef<HTMLDivElement>(null);
  const [ready, setReady] = useState(false);
  const [failure, setFailure] = useState<Error>();
  const chart = useRef<import("echarts/core").ECharts | undefined>(undefined);
  useEffect(() => {
    let disposed = false;
    let resize: ResizeObserver | undefined;
    Promise.all([
      import("echarts/core"),
      import("echarts/charts"),
      import("echarts/components"),
      import("echarts/renderers"),
    ])
      .then(([echarts, charts, components, renderers]) => {
        if (disposed) return;
        echarts.use([
          charts.GraphChart,
          components.TooltipComponent,
          components.AriaComponent,
          renderers.CanvasRenderer,
        ]);
        chart.current = echarts.init(host.current!);
        resize = new ResizeObserver(() => chart.current?.resize());
        resize.observe(host.current!);
        setReady(true);
      })
      .catch((error: Error) => {
        if (!disposed) setFailure(error);
      });
    return () => {
      disposed = true;
      resize?.disconnect();
      chart.current?.dispose();
      chart.current = undefined;
    };
  }, []);
  useEffect(() => {
    if (!ready || !chart.current) return;
    const assets = environment.appliedSpec?.assets ?? environment.spec.assets;
    const nodes = new Map<string, { id: string; name: string; role: string }>();
    for (const asset of assets)
      nodes.set(asset.id, {
        id: asset.id,
        name: asset.name,
        role: groups.get(asset.id)!,
      });
    const links = new Map<
      string,
      { source: string; target: string; bytes: number; rate: number }
    >();
    for (const flow of flows) {
      const source = endpoint(flow, "source"),
        target = endpoint(flow, "destination");
      for (const id of [source, target])
        if (!nodes.has(id)) {
          const capturedName =
            id === source ? flow.sourceAssetName : flow.destinationAssetName;
          const managed =
            id === source ? flow.sourceAssetId : flow.destinationAssetId;
          nodes.set(id, {
            id,
            name: capturedName ?? (managed ? "已移除资产" : id),
            role: managed
              ? (environment.view.roles?.[id] ?? "历史资产")
              : "外部",
          });
        }
      const key = source + "\0" + target;
      const link = links.get(key) ?? { source, target, bytes: 0, rate: 0 };
      link.bytes += flow.bytes;
      link.rate += flow.bytesPerSecond;
      links.set(key, link);
    }
    const rows = new Map<string, number>();
    const visibleLinks = [...links.values()]
      .sort((a, b) => b.bytes - a.bytes)
      .slice(0, 300);
    const visibleNodes = new Set(
      visibleLinks.flatMap((link) => [link.source, link.target]),
    );
    const roles = [...new Set([...nodes.values()].map((node) => node.role))];
    const textColor =
      getComputedStyle(host.current!).getPropertyValue("--text").trim() ||
      "#b7c1cc";
    const data = [...nodes.values()]
      .filter((node) => visibleNodes.has(node.id))
      .map((node) => {
        const row = rows.get(node.role) ?? 0;
        rows.set(node.role, row + 1);
        return {
          ...node,
          x: roles.indexOf(node.role) * 250,
          y: row * 95,
          symbolSize: node.role === "外部" ? 24 : 36,
          itemStyle: { color: colors.get(node.role) ?? "#8b94a6" },
          label: {
            show: true,
            position: "bottom",
            color: textColor,
            fontSize: 12,
          },
        };
      });
    chart.current.setOption({
      animation: !window.matchMedia("(prefers-reduced-motion: reduce)").matches,
      aria: { enabled: true },
      tooltip: {
        trigger: "item",
        renderMode: "richText",
        formatter: (item: {
          dataType: string;
          data: { name?: string; bytes?: number };
        }) =>
          item.dataType === "edge"
            ? volume(item.data.bytes ?? 0)
            : item.data.name,
      },
      series: [
        {
          type: "graph",
          layout: "none",
          roam: true,
          data,
          edges: visibleLinks.map((link) => ({
            ...link,
            lineStyle: {
              color: colors.get(groups.get(link.source) ?? "外部") ?? "#8b94a6",
              width: 1 + Math.min(7, Math.log10(1 + link.rate / 1024)),
              opacity: 0.7,
              curveness: 0.12,
            },
          })),
          edgeSymbol: ["none", "arrow"],
          edgeSymbolSize: [0, 8],
          emphasis: { focus: "adjacency" },
          scaleLimit: { min: 0.3, max: 4 },
        },
      ],
    });
    chart.current.off("click");
    chart.current.on("click", (event: unknown) => {
      const item = event as {
        dataType: string;
        data: { id: string; source: string; target: string };
      };
      if (item.dataType === "edge")
        onSelect({ source: item.data.source, destination: item.data.target });
      else onSelect({ asset: item.data.id });
    });
  }, [ready, environment, flows, groups, colors, onSelect]);
  return (
    <>
      <div
        ref={host}
        className={styles.graph}
        role="img"
        aria-label="资产通信图，连接明细可在右侧列表选择"
      />
      {failure && <div role="alert">{failure.message}</div>}
    </>
  );
}

export default function TrafficWorkspace({
  environment,
  onClose,
  onView,
}: {
  environment: Environment;
  onClose: () => void;
  onView?: (view: Schema<"CanvasView">) => Promise<unknown>;
}) {
  const client = useQueryClient();
  const id = environment.id;
  const [captureId, setCaptureId] = useState<string>();
  const [selection, setSelection] = useState<Selection>();
  const [protocol, setProtocol] = useState<string | null>(null);
  const [role, setRole] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const spec = environment.appliedSpec ?? environment.spec;
  const [assetIds, setAssetIds] = useState<string[]>(
    spec.assets.map((asset) => asset.id),
  );
  const [duration, setDuration] = useState(300);
  const [size, setSize] = useState(256);
  const [filter, setFilter] = useState("");
  const captures = useQuery({
    queryKey: ["captures", id],
    queryFn: ({ signal }) => api.captures(id, signal),
    refetchInterval: 3000,
  });
  const activeId = captureId ?? captures.data?.segments[0]?.id;
  const segments =
    captures.data?.segments.filter((segment) => segment.id === activeId) ?? [];
  const details = useQueries({
    queries: segments.map((segment) => ({
      queryKey: ["capture", id, segment.nodeId, segment.id],
      queryFn: ({ signal }: { signal: AbortSignal }) =>
        api.captureDetail(id, segment.nodeId, segment.id, signal),
      enabled: segment.status !== "failed" || segment.bytes > 0,
      refetchInterval: activeCapture(segment) ? 2000 : false,
    })),
  });
  const refresh = () => {
    void client.invalidateQueries({ queryKey: ["captures", id] });
    void client.invalidateQueries({ queryKey: ["capture", id] });
  };
  const start = useMutation({
    mutationFn: () =>
      api.startCapture(id, {
        assetIds,
        durationSeconds: duration,
        fileSizeMiB: size,
        filter,
      }),
    onSuccess: (result) => {
      if (result.segments.length) {
        setCaptureId(result.segments[0].id);
        setCreating(false);
      }
      refresh();
    },
  });
  const action = useMutation({
    mutationFn: async ({
      segments,
      remove,
    }: {
      segments: Segment[];
      remove: boolean;
    }) => {
      const results = await Promise.allSettled(
        segments.map((segment) =>
          remove
            ? api.deleteCapture(id, segment.nodeId, segment.id)
            : api.stopCapture(id, segment.nodeId, segment.id),
        ),
      );
      const errors = results
        .filter(
          (result): result is PromiseRejectedResult =>
            result.status === "rejected",
        )
        .map((result) => String(result.reason));
      if (errors.length) throw new Error(errors.join("；"));
    },
    onSettled: refresh,
  });
  const saveRole = useMutation({
    mutationFn: ({ asset, value }: { asset: string; value: string }) => {
      const roles = { ...environment.view.roles };
      if (value.trim()) roles[asset] = value.trim();
      else delete roles[asset];
      return onView!({ ...environment.view, roles });
    },
  });
  const groups = useMemo(
    () =>
      new Map(
        spec.assets.map((asset) => [
          asset.id,
          environment.view.roles?.[asset.id] ??
            spec.networks.find(
              (network) => network.id === asset.interfaces[0]?.networkId,
            )?.name ??
            "未分组",
        ]),
      ),
    [spec, environment.view.roles],
  );
  const colors = useMemo(
    () =>
      new Map(
        [...new Set(groups.values())]
          .sort()
          .map((name, index) => [name, palette[index % palette.length]]),
      ),
    [groups],
  );
  const allFlows = details
    .flatMap((query) => query.data?.flows ?? [])
    .sort((a, b) => b.bytes - a.bytes);
  const flows = allFlows.filter(
    (flow) =>
      (!protocol || flow.protocol === protocol) &&
      (!role ||
        groups.get(endpoint(flow, "source")) === role ||
        groups.get(endpoint(flow, "destination")) === role),
  );
  const name = (asset: string) => {
    const current = spec.assets.find((item) => item.id === asset);
    if (current) return current.name;
    const flow = allFlows.find(
      (item) =>
        item.sourceAssetId === asset || item.destinationAssetId === asset,
    );
    return flow
      ? ((flow.sourceAssetId === asset
          ? flow.sourceAssetName
          : flow.destinationAssetName) ?? "已移除资产")
      : asset;
  };
  const selectedFlows = flows.filter(
    (flow) =>
      !selection ||
      ("asset" in selection
        ? [endpoint(flow, "source"), endpoint(flow, "destination")].includes(
            selection.asset,
          )
        : endpoint(flow, "source") === selection.source &&
          endpoint(flow, "destination") === selection.destination),
  );
  const total = allFlows.reduce((sum, flow) => sum + flow.bytes, 0);
  const totalRate = allFlows.reduce(
    (sum, flow) => sum + flow.bytesPerSecond,
    0,
  );
  const protocols = [...new Set(allFlows.map((flow) => flow.protocol))].sort();
  const sessions = [
    ...new Map(
      (captures.data?.segments ?? []).map((segment) => [
        segment.id,
        {
          value: segment.id,
          label: new Date(segment.startedAt).toLocaleTimeString(),
        },
      ]),
    ).values(),
  ];
  const errors = [
    captures.error,
    action.error,
    saveRole.error,
    ...details.map((query) => query.error),
  ].filter((error): error is Error => error instanceof Error);
  return (
    <section className={styles.workspace} aria-label="网络流量">
      <header className={styles.toolbar}>
        <div className={styles.title}>
          <ActionIcon
            variant="subtle"
            color="gray"
            aria-label="返回拓扑"
            onClick={onClose}
          >
            <ArrowLeft size={18} />
          </ActionIcon>
          <strong>网络流量</strong>
          <span
            className={
              segments.some((segment) => activeCapture(segment))
                ? styles.live
                : styles.muted
            }
          >
            {segments.some((segment) => activeCapture(segment))
              ? "实时"
              : "记录"}
          </span>
        </div>
        <div className={styles.controls}>
          <Select
            aria-label="抓包记录"
            placeholder="抓包记录"
            data={sessions}
            value={activeId ?? null}
            onChange={(value) => {
              setCaptureId(value ?? undefined);
              setSelection(undefined);
            }}
            w={130}
            size="xs"
          />
          <Button
            size="xs"
            leftSection={
              captures.data?.segments.some((segment) =>
                activeCapture(segment),
              ) ? (
                <Pause size={14} />
              ) : (
                <Radio size={14} />
              )
            }
            loading={action.isPending}
            onClick={() => {
              const running =
                captures.data?.segments.filter((segment) =>
                  activeCapture(segment),
                ) ?? [];
              if (running.length)
                action.mutate({ segments: running, remove: false });
              else setCreating(true);
            }}
          >
            {captures.data?.segments.some((segment) => activeCapture(segment))
              ? "停止抓包"
              : "开始抓包"}
          </Button>
        </div>
      </header>
      {errors.map((error, index) => (
        <div key={index} className={styles.error} role="alert">
          {error.message}
        </div>
      ))}
      {Object.entries(captures.data?.errors ?? {}).map(([node, error]) => (
        <div key={node} className={styles.error} role="alert">
          {node} · {error}
        </div>
      ))}
      {Object.entries(start.data?.errors ?? {}).map(([node, error]) => (
        <div key={node} className={styles.error} role="alert">
          抓包启动失败 · {error}
        </div>
      ))}
      <div className={styles.body}>
        <div className={styles.visual}>
          <div className={styles.filters}>
            <Select
              aria-label="流量协议"
              placeholder="全部协议"
              clearable
              data={protocols}
              value={protocol}
              onChange={setProtocol}
              size="xs"
              w={140}
            />
            <Select
              aria-label="资产角色"
              placeholder="全部角色"
              clearable
              data={[...new Set(groups.values())].sort()}
              value={role}
              onChange={setRole}
              size="xs"
              w={140}
            />
            <span>
              {volume(total)} · {volume(totalRate)}/s
            </span>
          </div>
          {activeId ? (
            <CommunicationGraph
              environment={environment}
              flows={flows}
              groups={groups}
              colors={colors}
              onSelect={setSelection}
            />
          ) : (
            <div className={styles.empty}>
              <Radio size={32} />
              <strong>选择资产，查看真实通信</strong>
              <Button
                variant="light"
                size="sm"
                onClick={() => setCreating(true)}
              >
                开始抓包
              </Button>
            </div>
          )}
          <div className={styles.legend}>
            {[...colors].map(([name, color]) => (
              <button
                key={name}
                onClick={() => setRole(role === name ? null : name)}
                aria-pressed={role === name}
              >
                <i style={{ background: color }} />
                {name}
              </button>
            ))}
          </div>
        </div>
        <aside className={styles.inspector}>
          <header>
            <strong>
              {selection
                ? "asset" in selection
                  ? name(selection.asset)
                  : name(selection.source) + " → " + name(selection.destination)
                : "通信记录"}
            </strong>
            {selection && "source" in selection && (
              <div className={styles.assetLinks}>
                <button
                  onClick={() => setSelection({ asset: selection.source })}
                >
                  查看 {name(selection.source)}
                </button>
                <button
                  onClick={() => setSelection({ asset: selection.destination })}
                >
                  查看 {name(selection.destination)}
                </button>
              </div>
            )}
            {selection && (
              <ActionIcon
                variant="subtle"
                aria-label="查看全部通信"
                onClick={() => setSelection(undefined)}
              >
                <X size={15} />
              </ActionIcon>
            )}
          </header>
          {selection &&
            "asset" in selection &&
            onView &&
            spec.assets.some((asset) => asset.id === selection.asset) && (
              <TextInput
                key={
                  selection.asset +
                  (environment.view.roles?.[selection.asset] ?? "")
                }
                label="角色"
                placeholder="按网段分组"
                size="xs"
                defaultValue={environment.view.roles?.[selection.asset] ?? ""}
                onBlur={(event) => {
                  const value = event.currentTarget.value;
                  if (
                    value !== (environment.view.roles?.[selection.asset] ?? "")
                  )
                    saveRole.mutate({ asset: selection.asset, value });
                }}
              />
            )}
          <div className={styles.conversations} aria-label="可选择的通信连接">
            {selectedFlows.length === 0 && (
              <span className={styles.noFlows}>
                {activeId ? "暂无匹配通信" : "暂无抓包记录"}
              </span>
            )}
            {selectedFlows.slice(0, 300).map((flow) => (
              <button
                key={[
                  flow.source,
                  flow.destination,
                  flow.protocol,
                  flow.sourcePort,
                  flow.destinationPort,
                  flow.sourceAssetId,
                  flow.destinationAssetId,
                ].join("/")}
                onClick={() =>
                  setSelection({
                    source: endpoint(flow, "source"),
                    destination: endpoint(flow, "destination"),
                  })
                }
              >
                <span className={styles.route}>
                  <strong>{name(endpoint(flow, "source"))}</strong>
                  <ArrowRight size={13} />
                  <strong>{name(endpoint(flow, "destination"))}</strong>
                </span>
                <span className={styles.flowMeta}>
                  <span>
                    {flow.protocol} · {flow.sourcePort || "—"} →{" "}
                    {flow.destinationPort || "—"}
                  </span>
                  <b>{volume(flow.bytes)}</b>
                </span>
                {selection && (
                  <span className={styles.flowMeta}>
                    <span>
                      {flow.source} → {flow.destination}
                    </span>
                    <span>{flow.packets.toLocaleString()} 包</span>
                  </span>
                )}
              </button>
            ))}
            {selectedFlows.length > 300 && (
              <span className={styles.noFlows}>
                {selectedFlows.length} 条连接 · 显示前 300 条
              </span>
            )}
          </div>
          <div className={styles.segments}>
            <h3>抓包分段</h3>
            {segments.map((segment) => (
              <div key={segment.nodeId}>
                <div className={styles.segmentTitle}>
                  <strong>{segment.nodeName ?? "运行节点"}</strong>
                  <span
                    className={
                      segment.status === "failed"
                        ? styles.failed
                        : activeCapture(segment)
                          ? styles.live
                          : styles.muted
                    }
                  >
                    {stateTitle[segment.status]}
                  </span>
                </div>
                <div className={styles.segmentActions}>
                  <span>
                    {volume(segment.bytes)} · {segment.packets.toLocaleString()}{" "}
                    包
                  </span>
                  {activeCapture(segment) ? (
                    <ActionIcon
                      variant="subtle"
                      aria-label="停止此分段"
                      disabled={action.isPending}
                      onClick={() =>
                        action.mutate({ segments: [segment], remove: false })
                      }
                    >
                      <Pause size={15} />
                    </ActionIcon>
                  ) : (
                    <ActionIcon
                      component="a"
                      href={api.captureDownload(id, segment.nodeId, segment.id)}
                      variant="subtle"
                      aria-label="下载抓包文件"
                      disabled={!segment.packets}
                    >
                      <Download size={15} />
                    </ActionIcon>
                  )}
                  <ActionIcon
                    variant="subtle"
                    color="gray"
                    aria-label="删除此分段"
                    disabled={action.isPending || activeCapture(segment)}
                    onClick={() =>
                      action.mutate({ segments: [segment], remove: true })
                    }
                  >
                    <Trash2 size={14} />
                  </ActionIcon>
                </div>
                {segment.error && (
                  <div className={styles.error}>{segment.error}</div>
                )}
                {segment.omittedFlows > 0 && (
                  <div className={styles.error}>
                    在线汇总未纳入 {segment.omittedFlows.toLocaleString()}{" "}
                    个包；PCAP 保留原始记录
                  </div>
                )}
              </div>
            ))}
          </div>
        </aside>
      </div>
      <Modal
        opened={creating}
        onClose={() => {
          setCreating(false);
          start.reset();
        }}
        title="开始抓包"
        centered
      >
        <form
          className={styles.form}
          onSubmit={(event) => {
            event.preventDefault();
            start.mutate();
          }}
        >
          <MultiSelect
            label="资产"
            data={spec.assets.map((asset) => ({
              value: asset.id,
              label: asset.name,
            }))}
            value={assetIds}
            onChange={setAssetIds}
            searchable
          />
          <div className={styles.limits}>
            <NumberInput
              label="时长（秒）"
              min={10}
              max={1800}
              value={duration}
              onChange={(value) => setDuration(Number(value))}
            />
            <NumberInput
              label="每节点文件额度（MiB）"
              min={1}
              max={1024}
              value={size}
              onChange={(value) => setSize(Number(value))}
            />
          </div>
          <details>
            <summary>过滤条件</summary>
            <TextInput
              aria-label="抓包过滤条件"
              placeholder="tcp port 443"
              value={filter}
              onChange={(event) => setFilter(event.currentTarget.value)}
            />
          </details>
          {start.error && (
            <div className={styles.error} role="alert">
              {start.error.message}
            </div>
          )}
          <Button
            type="submit"
            loading={start.isPending}
            disabled={!assetIds.length}
          >
            开始
          </Button>
        </form>
      </Modal>
    </section>
  );
}
