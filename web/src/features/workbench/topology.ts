import dagre from "@dagrejs/dagre";
import type { Edge, Node } from "@xyflow/react";
import type { EnvironmentSpec, Schema, Template } from "../../api/client";

export const networkColors = [
  "#0f9c8d",
  "#6787d2",
  "#a580ce",
  "#cf9657",
  "#68a277",
  "#cc788c",
];
export type TopologyData = {
  label: string;
  secondary: string;
  kind: "network" | "container" | "vm";
  state: string;
  color: string;
  count?: number;
};
export type TopologyNode = Node<TopologyData, "asset" | "network">;

export function topology(
  spec: EnvironmentSpec,
  templates: Template[],
  view: Schema<"CanvasView">,
): { nodes: TopologyNode[]; edges: Edge[] } {
  const graph = new dagre.graphlib.Graph()
    .setGraph({
      rankdir: "TB",
      nodesep: 40,
      ranksep: 100,
      marginx: 56,
      marginy: 48,
    })
    .setDefaultEdgeLabel(() => ({}));
  const networks = new Map(
    spec.networks.map((network, index) => [
      network.id,
      { network, color: networkColors[index % networkColors.length] },
    ]),
  );
  const counts = new Map<string, number>();
  spec.assets.forEach((asset) =>
    new Set(asset.interfaces.map((item) => item.networkId)).forEach((id) =>
      counts.set(id, (counts.get(id) ?? 0) + 1),
    ),
  );
  const templateMap = new Map(
    templates.map((template) => [template.id, template]),
  );
  const nodes: TopologyNode[] = [
    ...spec.networks.map((network) => ({
      id: network.id,
      type: "network" as const,
      position: { x: 0, y: 0 },
      data: {
        label: network.name,
        secondary: network.cidr,
        kind: "network" as const,
        state: "",
        color: networks.get(network.id)!.color,
        count: counts.get(network.id) ?? 0,
      },
    })),
    ...spec.assets.map((asset) => ({
      id: asset.id,
      type: "asset" as const,
      position: { x: 0, y: 0 },
      data: {
        label: asset.name,
        secondary:
          asset.interfaces.find((item) => item.primary)?.address ||
          asset.interfaces[0]?.address ||
          "",
        kind: templateMap.get(asset.templateId)?.kind ?? "container",
        state: "draft",
        color:
          networks.get(asset.interfaces[0]?.networkId)?.color ??
          networkColors[0],
      },
    })),
  ];
  const edges: Edge[] = spec.assets.flatMap((asset) =>
    asset.interfaces
      .filter((item) => networks.has(item.networkId))
      .map((item) => ({
        id: item.id,
        source: item.networkId,
        target: asset.id,
        type: "smoothstep",
        style: {
          stroke: networks.get(item.networkId)!.color,
          strokeWidth: 1.5,
        },
        data: {
          assetId: asset.id,
          networkId: item.networkId,
          address: item.address,
        },
      })),
  );
  nodes.forEach((node) =>
    graph.setNode(node.id, {
      width: 188,
      height: node.type === "network" ? 64 : 100,
    }),
  );
  edges.forEach((edge) => graph.setEdge(edge.source, edge.target));
  dagre.layout(graph);
  return {
    nodes: nodes.map((node) => {
      const point = graph.node(node.id) as { x: number; y: number };
      return {
        ...node,
        position: view.positions?.[node.id] ?? {
          x: point.x - 94,
          y: point.y - (node.type === "network" ? 32 : 50),
        },
      };
    }),
    edges,
  };
}

export function connectAsset(
  spec: EnvironmentSpec,
  assetId: string,
  networkId: string,
): EnvironmentSpec {
  return {
    ...spec,
    assets: spec.assets.map((asset) =>
      asset.id !== assetId ||
      asset.interfaces.some((item) => item.networkId === networkId)
        ? asset
        : {
            ...asset,
            interfaces: [
              ...asset.interfaces,
              {
                id: crypto.randomUUID(),
                networkId,
                address: "",
                mac: "",
                primary: !asset.interfaces.length,
              },
            ],
          },
    ),
  };
}
