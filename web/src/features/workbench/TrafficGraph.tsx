import { useComputedColorScheme } from "@mantine/core";
import { createPortal } from "react-dom";
import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type RefObject,
} from "react";
import dagre from "@dagrejs/dagre";
import {
  Background,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  ReactFlowProvider,
  useNodesInitialized,
  useNodesState,
  useReactFlow,
  useStore,
  type Node,
  type NodeProps,
} from "@xyflow/react";
import {
  Globe2,
  Maximize,
  Minimize,
  Radio,
  Router,
  Server,
  Shuffle,
  Waypoints,
  ZoomIn,
  ZoomOut,
  Scan,
} from "lucide-react";
import type { Environment } from "../../api/client";
import { byteUnit, reading } from "../../foundation/format";
import styles from "./TrafficGraph.module.css";

export type TrafficSelection =
  | { asset: string }
  | {
      source: string;
      destination: string;
    };
export type EndpointKind =
  "asset" | "gateway" | "external" | "broadcast" | "multicast" | "ethernet";
export type TrafficLink = {
  source: string;
  target: string;
  sourceName: string;
  targetName: string;
  sourceKind?: EndpointKind;
  targetKind?: EndpointKind;
  value: number;
  rate?: number;
  severity?: number;
  alerts?: number;
};
export const riskLevels = [
  { value: "high", label: "高风险", color: "var(--negative)" },
  { value: "medium", label: "中风险", color: "var(--warning)" },
  { value: "low", label: "低风险", color: "var(--accent)" },
] as const;
export const riskLevel = (severity: number) =>
  riskLevels[severity === 1 ? 0 : severity === 2 ? 1 : 2];
const size = { width: 150, height: 94 };
type AssetNode = Node<
  {
    name: string;
    address: string;
    color: string;
    kind: EndpointKind;
  },
  "trafficAsset"
>;
const icons = {
  asset: Server,
  gateway: Router,
  external: Globe2,
  broadcast: Radio,
  multicast: Radio,
  ethernet: Waypoints,
};

function TrafficAsset({ data, selected }: NodeProps<AssetNode>) {
  const Icon = icons[data.kind];
  return (
    <div
      className={styles.node}
      data-selected={selected}
      data-kind={data.kind}
      style={{ "--node-color": data.color } as CSSProperties}
    >
      <Handle type="target" position={Position.Left} />
      <span className={styles.symbol}>
        <Icon size={23} strokeWidth={1.6} />
      </span>
      <strong title={data.name}>{data.name}</strong>
      {data.address && <span className={styles.address}>{data.address}</span>}
      <Handle type="source" position={Position.Right} />
    </div>
  );
}
const nodeTypes = { trafficAsset: TrafficAsset };
type Point = { x: number; y: number };
function arrange(
  nodes: AssetNode[],
  links: TrafficLink[],
  frame: { width: number; height: number },
) {
  const layouts = ["LR", "TB"].map((rankdir) => {
    const graph = new dagre.graphlib.Graph()
      .setGraph({ rankdir, ranksep: 65, nodesep: 24 })
      .setDefaultEdgeLabel(() => ({}));
    for (const node of nodes) graph.setNode(node.id, { ...size });
    for (const link of links) graph.setEdge(link.source, link.target);
    dagre.layout(graph);
    return nodes.map((node) => ({
      ...node,
      position: {
        x: graph.node(node.id).x - size.width / 2,
        y: graph.node(node.id).y - size.height / 2,
      },
    }));
  });
  const score = (layout: AssetNode[]) => {
    const assets = layout.filter((node) => node.data.kind === "asset");
    const focus = assets.length ? assets : layout;
    if (!focus.length) return 1;
    const width =
      Math.max(...focus.map((node) => node.position.x)) +
      size.width -
      Math.min(...focus.map((node) => node.position.x));
    const height =
      Math.max(...focus.map((node) => node.position.y)) +
      size.height -
      Math.min(...focus.map((node) => node.position.y));
    return Math.min(frame.width / width, frame.height / height);
  };
  return score(layouts[0]) >= score(layouts[1]) ? layouts[0] : layouts[1];
}
function GraphActions({
  onArrange,
  fullscreenTarget,
  toolsTarget,
}: {
  onArrange: () => void;
  fullscreenTarget: RefObject<HTMLElement | null>;
  toolsTarget: HTMLElement | null;
}) {
  const initialized = useNodesInitialized(),
    fitted = useRef(false);
  const { fitView, getNodes, zoomIn, zoomOut } = useReactFlow();
  const [fullscreen, setFullscreen] = useState(false),
    [error, setError] = useState<string>();
  useEffect(() => {
    if (initialized && !fitted.current) {
      fitted.current = true;
      void fitView({
        nodes: getNodes().filter((node) => node.data.kind === "asset"),
        padding: 0.18,
        maxZoom: 1.05,
      });
    }
  }, [initialized, fitView, getNodes]);
  useEffect(() => {
    const changed = () =>
      setFullscreen(document.fullscreenElement === fullscreenTarget.current);
    document.addEventListener("fullscreenchange", changed);
    return () => document.removeEventListener("fullscreenchange", changed);
  }, [fullscreenTarget]);
  return (
    toolsTarget &&
    createPortal(
      <>
        <div className={styles.tools}>
          <button
            title="放大通信图"
            aria-label="放大通信图"
            onClick={() => void zoomIn()}
          >
            <ZoomIn size={14} />
          </button>
          <button
            title="缩小通信图"
            aria-label="缩小通信图"
            onClick={() => void zoomOut()}
          >
            <ZoomOut size={14} />
          </button>
          <button
            title="适应画布"
            aria-label="适应画布"
            onClick={() => void fitView({ padding: 0.18, maxZoom: 1.05 })}
          >
            <Scan size={14} />
          </button>
          <button title="重新布局" aria-label="重新布局" onClick={onArrange}>
            <Shuffle size={14} />
          </button>
          <button
            title={fullscreen ? "退出全屏" : "全屏分析"}
            aria-label={fullscreen ? "退出全屏" : "全屏分析"}
            onClick={() => {
              setError(undefined);
              const action = fullscreen
                ? document.exitFullscreen()
                : fullscreenTarget.current!.requestFullscreen();
              void action.catch((reason: Error) => setError(reason.message));
            }}
          >
            {fullscreen ? <Minimize size={14} /> : <Maximize size={14} />}
          </button>
        </div>
        {error && (
          <div className={styles.error} role="alert">
            {error}
          </div>
        )}
      </>,
      toolsTarget,
    )
  );
}
function Graph({
  environment,
  links,
  groups,
  colors,
  selection,
  onSelect,
  fullscreenTarget,
  toolsTarget,
}: {
  environment: Environment;
  links: TrafficLink[];
  groups: Map<string, string>;
  colors: Map<string, string>;
  selection?: TrafficSelection;
  onSelect: (selection?: TrafficSelection) => void;
  fullscreenTarget: RefObject<HTMLElement | null>;
  toolsTarget: HTMLElement | null;
}) {
  const theme = useComputedColorScheme();
  const width = useStore((state) => state.width),
    height = useStore((state) => state.height);
  const zoom = useStore((state) => state.transform[2]);
  const spec = environment.appliedSpec ?? environment.spec;
  const merged = useMemo(() => {
    const pairs = new Map<
      string,
      TrafficLink & { forward: boolean; reverse: boolean }
    >();
    for (const link of links) {
      const [source, target] = [link.source, link.target].sort();
      const forward = source === link.source,
        key = JSON.stringify([source, target]),
        previous = pairs.get(key);
      if (previous) {
        previous.value += link.value;
        previous.rate = (previous.rate ?? 0) + (link.rate ?? 0);
        previous.alerts = (previous.alerts ?? 0) + (link.alerts ?? 0);
        previous.forward ||= forward;
        previous.reverse ||= !forward;
        if (link.severity !== undefined)
          previous.severity = Math.min(previous.severity ?? 255, link.severity);
      } else
        pairs.set(key, {
          ...link,
          source,
          target,
          forward,
          reverse: !forward,
          sourceName: forward ? link.sourceName : link.targetName,
          targetName: forward ? link.targetName : link.sourceName,
          sourceKind: forward ? link.sourceKind : link.targetKind,
          targetKind: forward ? link.targetKind : link.sourceKind,
        });
    }
    return [...pairs.values()].sort((a, b) => b.value - a.value);
  }, [links]);
  const displayed = merged.slice(0, 300);
  const [hovered, setHovered] = useState<string>();
  const models = useMemo(() => {
    const nodes = new Map<string, AssetNode>(),
      neighbors = new Set<string>();
    if (selection && "asset" in selection)
      for (const link of merged)
        if (
          link.source === selection.asset ||
          link.target === selection.asset
        ) {
          neighbors.add(link.source);
          neighbors.add(link.target);
        }
    const involved = (id: string) =>
      !selection ||
      ("asset" in selection
        ? selection.asset === id || neighbors.has(id)
        : selection.source === id || selection.destination === id);
    for (const asset of spec.assets)
      nodes.set(asset.id, {
        id: asset.id,
        type: "trafficAsset",
        position: { x: 0, y: 0 },
        selected: Boolean(
          selection && "asset" in selection && selection.asset === asset.id,
        ),
        style: { opacity: involved(asset.id) ? 1 : 0.35 },
        ariaLabel: `资产 ${asset.name}`,
        data: {
          name: asset.name,
          address:
            asset.interfaces.find((item) => item.primary)?.address ??
            asset.interfaces[0]?.address ??
            "",
          kind: "asset",
          color: colors.get(groups.get(asset.id) ?? "") ?? "var(--chart-1)",
        },
      });
    for (const link of merged.slice(0, 300))
      for (const side of ["source", "target"] as const) {
        const id = link[side];
        if (nodes.has(id)) continue;
        const kind = link[`${side}Kind`] ?? "external";
        nodes.set(id, {
          id,
          type: "trafficAsset",
          position: { x: 0, y: 0 },
          selected: Boolean(
            selection && "asset" in selection && selection.asset === id,
          ),
          ariaLabel: link[`${side}Name`],
          style: { opacity: involved(id) ? 1 : 0.35 },
          data: {
            name: link[`${side}Name`],
            address: "",
            kind,
            color: "var(--muted)",
          },
        });
      }
    return [...nodes.values()];
  }, [spec.assets, merged, selection, groups, colors]);
  const [nodes, setNodes, onNodesChange] = useNodesState<AssetNode>([]);
  useEffect(() => {
    if (!width || !height) return;
    setNodes((current) => {
      const present = new Set(models.map((node) => node.id));
      const placed = new Map(
        current
          .filter((node) => present.has(node.id))
          .map((node) => [node.id, node]),
      );
      if (!placed.size) return arrange(models, displayed, { width, height });
      const occupied = [...placed.values()].map((node) => node.position);
      const free = (point: Point) =>
        !occupied.some(
          (other) =>
            Math.abs(other.x - point.x) < size.width + 40 &&
            Math.abs(other.y - point.y) < size.height + 35,
        );
      return models.map((model) => {
        const previous = placed.get(model.id);
        let position = previous?.position;
        if (!position) {
          const neighbor = displayed.find(
            (link) =>
              (link.source === model.id && placed.has(link.target)) ||
              (link.target === model.id && placed.has(link.source)),
          );
          const anchor = neighbor
            ? placed.get(
                neighbor.source === model.id
                  ? neighbor.target
                  : neighbor.source,
              )!.position
            : { x: 0, y: 0 };
          const x = neighbor
            ? anchor.x + (neighbor.source === model.id ? -250 : 250)
            : anchor.x;
          let row = 0;
          do {
            position = {
              x,
              y: anchor.y + (row % 2 ? -1 : 1) * Math.ceil(row / 2) * 140,
            };
            row++;
          } while (!free(position));
          placed.set(model.id, { ...model, position });
          occupied.push(position);
        }
        return { ...model, position, measured: previous?.measured };
      });
    });
  }, [models, setNodes, width, height]);
  const edges = displayed.map((link) => {
    const id = JSON.stringify([link.source, link.target]);
    const active = Boolean(
      selection &&
      ("asset" in selection
        ? selection.asset === link.source || selection.asset === link.target
        : (selection.source === link.source &&
            selection.destination === link.target) ||
          (selection.source === link.target &&
            selection.destination === link.source)),
    );
    const color =
      link.severity !== undefined
        ? riskLevel(link.severity).color
        : (colors.get(
            groups.get(link.source) ?? groups.get(link.target) ?? "",
          ) ?? "var(--muted)");
    const amount = link.rate && link.rate > 0 ? link.rate : link.value,
      unit = byteUnit(amount);
    const label =
      amount > 0
        ? `${reading(amount, unit.scale)} ${unit.unit}${link.rate && link.rate > 0 ? "/s" : ""}`
        : link.alerts
          ? `${link.alerts} 次告警`
          : "";
    return {
      id,
      source: link.source,
      target: link.target,
      type: "default",
      selected: active,
      label: active || hovered === id ? label : undefined,
      markerEnd: link.forward
        ? { type: MarkerType.ArrowClosed, color, width: 14, height: 14 }
        : undefined,
      markerStart: link.reverse
        ? { type: MarkerType.ArrowClosed, color, width: 14, height: 14 }
        : undefined,
      style: {
        stroke: color,
        strokeWidth: active
          ? 2.5
          : Math.min(2.5, 1 + Math.log10(1 + link.value) / 5),
        opacity: selection && !active ? 0.15 : link.rate ? 0.8 : 0.4,
      },
      labelStyle: { fill: "var(--text)", fontSize: 11 },
      labelBgStyle: { fill: "var(--surface)" },
      labelBgPadding: [6, 4] as [number, number],
      labelBgBorderRadius: 3,
      ariaLabel: `${link.sourceName} ↔ ${link.targetName}${label ? `，${label}` : ""}`,
      data: link,
    };
  });
  const selectNode = (node: AssetNode) => onSelect({ asset: node.id });
  const selectEdge = (edge: (typeof edges)[number]) => {
    onSelect({ source: edge.source, destination: edge.target });
  };
  return (
    <div
      className={styles.graph}
      role="region"
      aria-label="资产通信图"
      data-compact={zoom < 0.85}
      style={{ "--graph-zoom": zoom } as CSSProperties}
      onKeyDown={(event) => {
        if (event.key === "Escape") {
          event.stopPropagation();
          onSelect(undefined);
        }
      }}
    >
      <ReactFlow<AssetNode>
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        onNodesChange={(changes) => {
          onNodesChange(changes);
          for (const change of changes)
            if (change.type === "select" && change.selected)
              selectNode(nodes.find((node) => node.id === change.id)!);
        }}
        onNodeClick={(_, node) => selectNode(node)}
        onEdgeClick={(_, edge) => {
          selectEdge(edge as (typeof edges)[number]);
        }}
        onEdgesChange={(changes) => {
          for (const change of changes)
            if (change.type === "select" && change.selected)
              selectEdge(edges.find((edge) => edge.id === change.id)!);
        }}
        onEdgeMouseEnter={(_, edge) => setHovered(edge.id)}
        onEdgeMouseLeave={() => setHovered(undefined)}
        onPaneClick={() => onSelect(undefined)}
        nodesConnectable={false}
        deleteKeyCode={null}
        minZoom={0.15}
        maxZoom={2}
        colorMode={theme}
        proOptions={{ hideAttribution: true }}
        ariaLabelConfig={{
          "controls.zoomIn.ariaLabel": "放大通信图",
          "controls.zoomOut.ariaLabel": "缩小通信图",
          "controls.fitView.ariaLabel": "适应画布",
        }}
      >
        <Background color="var(--line)" gap={24} size={1} />
        <GraphActions
          fullscreenTarget={fullscreenTarget}
          toolsTarget={toolsTarget}
          onArrange={() => {
            const arranged = arrange(nodes, displayed, { width, height });
            setNodes(arranged);
          }}
        />
      </ReactFlow>
      {merged.length > displayed.length && (
        <span className={styles.limit}>
          {displayed.length} / {merged.length} 组关系
        </span>
      )}
    </div>
  );
}
export function TrafficGraph(props: Parameters<typeof Graph>[0]) {
  return (
    <ReactFlowProvider>
      <Graph {...props} />
    </ReactFlowProvider>
  );
}
