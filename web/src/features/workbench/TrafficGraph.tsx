import { useComputedColorScheme } from "@mantine/core";
import { useEffect, useMemo, type CSSProperties } from "react";
import dagre from "@dagrejs/dagre";
import {
  Background,
  Controls,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  useNodesInitialized,
  useNodesState,
  useReactFlow,
  useStore,
  type Node,
  type NodeProps,
} from "@xyflow/react";
import { Globe2, Server } from "lucide-react";
import type { Environment } from "../../api/client";
import { byteUnit, reading } from "../../foundation/format";
import styles from "./TrafficWorkspace.module.css";

export type TrafficSelection =
  { asset: string } | { source: string; destination: string };
export type TrafficLink = {
  source: string;
  target: string;
  sourceName: string;
  targetName: string;
  value: number;
  label: string;
  measure?: "rate" | "bytes";
  severity?: number;
};
export const riskLevels = [
  { value: "high", label: "高风险", color: "#cf4c62" },
  { value: "medium", label: "中风险", color: "#c1872e" },
  { value: "low", label: "低风险", color: "#547dc4" },
] as const;
export const riskLevel = (severity: number) =>
  riskLevels[severity === 1 ? 0 : severity === 2 ? 1 : 2];

type AssetNode = Node<
  {
    name: string;
    role: string;
    address: string;
    color: string;
    external: boolean;
  },
  "trafficAsset"
>;

function TrafficAsset({ data, selected }: NodeProps<AssetNode>) {
  const Icon = data.external ? Globe2 : Server;
  return (
    <div
      className={styles.assetNode}
      data-selected={selected}
      style={{ "--node-color": data.color } as CSSProperties}
    >
      <Handle type="target" position={Position.Left} />
      <div className={styles.assetNodeHeader}>
        <span>
          <Icon size={24} strokeWidth={1.5} />
        </span>
        <strong title={data.name}>{data.name}</strong>
      </div>
      <div className={styles.assetNodeAddress}>{data.address || "—"}</div>
      <Handle type="source" position={Position.Right} />
    </div>
  );
}
const nodeTypes = { trafficAsset: TrafficAsset };

function FitGraph({ layoutKey }: { layoutKey: string }) {
  const initialized = useNodesInitialized();
  const { fitView } = useReactFlow();
  const width = useStore((state) => state.width);
  const height = useStore((state) => state.height);
  useEffect(() => {
    if (initialized) void fitView({ padding: 0.1, maxZoom: 1.1 });
  }, [initialized, layoutKey, fitView, width, height]);
  return null;
}

export function TrafficGraph({
  environment,
  links,
  groups,
  colors,
  selection,
  onSelect,
}: {
  environment: Environment;
  links: TrafficLink[];
  groups: Map<string, string>;
  colors: Map<string, string>;
  selection?: TrafficSelection;
  onSelect: (selection?: TrafficSelection) => void;
}) {
  const theme = useComputedColorScheme();
  const spec = environment.appliedSpec ?? environment.spec;
  const merged = useMemo(() => {
    const connections = new Map<
      string,
      TrafficLink & { forward: boolean; reverse: boolean }
    >();
    for (const link of links) {
      const [source, target] = [link.source, link.target].sort();
      const forward = source === link.source;
      const key = JSON.stringify([source, target]);
      const previous = connections.get(key);
      if (previous) {
        previous.value += link.value;
        previous.forward ||= forward;
        previous.reverse ||= !forward;
        if (link.severity !== undefined)
          previous.severity = Math.min(previous.severity ?? 255, link.severity);
        const unit = byteUnit(previous.value);
        previous.label =
          previous.severity !== undefined
            ? `${previous.value.toLocaleString()} 次命中`
            : `${reading(previous.value, unit.scale)} ${unit.unit}${previous.measure === "bytes" ? "" : "/s"}`;
      } else
        connections.set(key, {
          ...link,
          source,
          target,
          sourceName: forward ? link.sourceName : link.targetName,
          targetName: forward ? link.targetName : link.sourceName,
          forward,
          reverse: !forward,
        });
    }
    return [...connections.values()].sort((a, b) => b.value - a.value);
  }, [links]);
  const displayed = merged.slice(0, 300);
  // Counts change every collection; only connectivity changes the layout.
  const layoutKey = JSON.stringify(
    displayed.map((link) => [link.source, link.target]).sort(),
  );
  const positions = useMemo(() => {
    const graph = new dagre.graphlib.Graph()
      .setGraph({ rankdir: "LR", ranksep: 160, nodesep: 64 })
      .setDefaultEdgeLabel(() => ({}));
    for (const [source, target] of JSON.parse(layoutKey) as string[][]) {
      for (const id of [source, target])
        if (!graph.hasNode(id)) graph.setNode(id, { width: 144, height: 116 });
      graph.setEdge(source, target);
    }
    dagre.layout(graph);
    const positions = new Map(
      graph
        .nodes()
        .map((id) => [
          id,
          { x: graph.node(id).x - 72, y: graph.node(id).y - 58 },
        ]),
    );
    const bottom = graph.nodeCount()
      ? Math.max(...[...positions.values()].map((position) => position.y)) + 200
      : 0;
    spec.assets
      .filter((asset) => !positions.has(asset.id))
      .forEach((asset, index) => {
        positions.set(asset.id, {
          x: (index % 5) * 200,
          y: bottom + Math.floor(index / 5) * 160,
        });
      });
    return positions;
  }, [spec.assets, layoutKey]);
  const nodes = useMemo(() => {
    const nodes = new Map<string, AssetNode>();
    const neighbors = new Set<string>();
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
        ? selection.asset === id
        : selection.source === id || selection.destination === id);
    for (const asset of spec.assets) {
      const role = groups.get(asset.id) ?? "未分组";
      nodes.set(asset.id, {
        id: asset.id,
        type: "trafficAsset",
        position: positions.get(asset.id)!,
        selected: Boolean(selection && involved(asset.id)),
        style: {
          opacity: involved(asset.id) || neighbors.has(asset.id) ? 1 : 0.3,
        },
        ariaLabel: `资产 ${asset.name}`,
        data: {
          name: asset.name,
          role,
          color: colors.get(role) ?? "#75828f",
          external: false,
          address:
            asset.interfaces.find((item) => item.primary)?.address ??
            asset.interfaces[0]?.address ??
            "",
        },
      });
    }
    for (const link of merged.slice(0, 300))
      for (const [id, name] of [
        [link.source, link.sourceName],
        [link.target, link.targetName],
      ])
        if (!nodes.has(id))
          nodes.set(id, {
            id,
            type: "trafficAsset",
            position: positions.get(id)!,
            selected: Boolean(selection && involved(id)),
            style: { opacity: involved(id) || neighbors.has(id) ? 1 : 0.3 },
            ariaLabel: `地址 ${name}`,
            data: {
              name,
              role: "外部",
              address: id,
              color: "#75828f",
              external: true,
            },
          });
    return [...nodes.values()];
  }, [spec.assets, positions, groups, colors, merged, selection]);
  const [renderedNodes, setNodes, onNodesChange] = useNodesState(nodes);
  useEffect(() => {
    setNodes((current) => {
      const dimensions = new Map(
        current.map((node) => [node.id, node.measured]),
      );
      return nodes.map((node) => ({
        ...node,
        measured: dimensions.get(node.id),
      }));
    });
  }, [nodes, setNodes]);
  const edges = displayed.map((link) => {
    const active =
      !selection ||
      ("asset" in selection
        ? link.source === selection.asset || link.target === selection.asset
        : (link.source === selection.source &&
            link.target === selection.destination) ||
          (link.target === selection.source &&
            link.source === selection.destination));
    const color =
      link.severity !== undefined
        ? riskLevel(link.severity).color
        : (colors.get(groups.get(link.source) ?? "") ?? "#75828f");
    return {
      id: JSON.stringify([link.source, link.target]),
      source: link.source,
      target: link.target,
      type: "default",
      selected: Boolean(selection && "source" in selection && active),
      label:
        active && (merged.length <= 12 || selection) ? link.label : undefined,
      markerEnd: link.forward
        ? { type: MarkerType.ArrowClosed, color, width: 16, height: 16 }
        : undefined,
      markerStart: link.reverse
        ? { type: MarkerType.ArrowClosed, color, width: 16, height: 16 }
        : undefined,
      style: {
        stroke: color,
        strokeWidth: active
          ? Math.min(4, 1.5 + Math.log10(1 + link.value) / 4)
          : 1,
        opacity: active ? 0.85 : 0.18,
      },
      labelStyle: { fill: color, fontSize: 11, fontWeight: 500 },
      labelBgStyle: { fill: "var(--surface)" },
      labelBgPadding: [7, 5] as [number, number],
      labelBgBorderRadius: 5,
      ariaLabel: `${link.sourceName} ${link.reverse ? (link.forward ? "↔" : "←") : "→"} ${link.targetName}，${link.label}`,
    };
  });
  return (
    <div
      className={styles.graph}
      role="region"
      aria-label="资产通信图"
      onKeyDown={(event) => {
        if (event.key === "Escape") onSelect(undefined);
      }}
    >
      <ReactFlow<AssetNode>
        nodes={renderedNodes}
        onNodesChange={(changes) => {
          onNodesChange(changes);
          for (const change of changes)
            if (change.type === "select" && change.selected)
              onSelect({ asset: change.id });
        }}
        edges={edges}
        nodeTypes={nodeTypes}
        onEdgesChange={(changes) => {
          for (const change of changes) {
            if (change.type !== "select" || !change.selected) continue;
            const edge = edges.find((edge) => edge.id === change.id)!;
            onSelect({ source: edge.source, destination: edge.target });
          }
        }}
        onPaneClick={() => onSelect(undefined)}
        nodesDraggable={false}
        nodesConnectable={false}
        deleteKeyCode={null}
        fitView
        fitViewOptions={{ padding: 0.1, maxZoom: 1.1 }}
        minZoom={0.15}
        maxZoom={1.6}
        colorMode={theme}
        proOptions={{ hideAttribution: true }}
        ariaLabelConfig={{
          "controls.zoomIn.ariaLabel": "放大通信图",
          "controls.zoomOut.ariaLabel": "缩小通信图",
          "controls.fitView.ariaLabel": "适应画布",
        }}
      >
        <Background color="var(--line)" gap={24} size={1} />
        <Controls showInteractive={false} position="bottom-right" />
        <FitGraph layoutKey={layoutKey} />
      </ReactFlow>
      {merged.length > displayed.length && (
        <div className={styles.graphLimit}>
          显示 {displayed.length} / {merged.length} 组关系
        </div>
      )}
    </div>
  );
}
