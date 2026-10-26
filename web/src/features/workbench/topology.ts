import { randomUUID } from "../../foundation/id";
import dagre from "@dagrejs/dagre";
import type { Edge, Node } from "@xyflow/react";
import type { EnvironmentSpec, Schema, Template } from "../../api/client";

export const networkColors = [
  "var(--chart-1)",
  "var(--chart-2)",
  "var(--chart-3)",
  "var(--chart-4)",
  "var(--chart-5)",
  "var(--chart-6)",
];
export type TopologyData = {
  label: string;
  secondary: string;
  kind: "network" | "container" | "vm" | "external" | "router";
  state: string;
  color: string;
  count?: number;
};
export type TopologyNode = Node<TopologyData, "asset" | "network" | "router">;
export const topologyNodeSize = {
  asset: { width: 188, height: 76 },
  network: { width: 188, height: 60 },
  router: { width: 188, height: 76 },
};

export function topology(
  spec: EnvironmentSpec,
  templates: Template[],
  view: Schema<"CanvasView">,
): { nodes: TopologyNode[]; edges: Edge[] } {
  const graph = new dagre.graphlib.Graph()
    .setGraph({
      rankdir: "TB",
      nodesep: 40,
      ranksep: 80,
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
  spec.networks.forEach((network) =>
    counts.set(
      network.id,
      (counts.get(network.id) ?? 0) + (network.devices?.length ?? 0),
    ),
  );
  for (const router of spec.routers ?? [])
    for (const iface of router.interfaces)
      counts.set(iface.networkId, (counts.get(iface.networkId) ?? 0) + 1);
  const templateMap = new Map(
    templates.map((template) => [template.id, template]),
  );
  const nodes: TopologyNode[] = [
    ...(spec.routers ?? []).map((router) => ({
      id: router.id,
      type: "router" as const,
      position: { x: 0, y: 0 },
      data: {
        label: router.name,
        secondary: `${router.interfaces.length} 个接口`,
        kind: "router" as const,
        state: "",
        color: "var(--accent)",
      },
    })),
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
    ...spec.networks.flatMap((network) =>
      (network.devices ?? []).map((device) => ({
        id: device.id,
        type: "asset" as const,
        position: { x: 0, y: 0 },
        data: {
          label: device.name,
          secondary: device.address,
          kind: "external" as const,
          state: "",
          color: networks.get(network.id)!.color,
        },
      })),
    ),
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
  for (const router of spec.routers ?? [])
    for (const iface of router.interfaces)
      if (networks.has(iface.networkId))
        edges.push({
          id: iface.id,
          source: router.id,
          target: iface.networkId,
          type: "smoothstep",
          label: iface.address,
          labelStyle: {
            fill: "var(--muted)",
            fontSize: 10,
            fontFamily: "var(--font-mono)",
          },
          labelBgStyle: { fill: "var(--surface)" },
          style: {
            stroke: networks.get(iface.networkId)!.color,
            strokeWidth: 1.5,
          },
        });
  spec.networks.forEach((network) =>
    (network.devices ?? []).forEach((device) =>
      edges.push({
        id: `external-${device.id}`,
        source: network.id,
        target: device.id,
        type: "smoothstep",
        style: {
          stroke: networks.get(network.id)!.color,
          strokeWidth: 1.5,
          strokeDasharray: "5 4",
        },
      }),
    ),
  );
  nodes.forEach((node) =>
    graph.setNode(node.id, { ...topologyNodeSize[node.type!] }),
  );
  edges.forEach((edge) => graph.setEdge(edge.source, edge.target));
  dagre.layout(graph);
  return {
    nodes: nodes.map((node) => {
      const point = graph.node(node.id) as { x: number; y: number };
      return {
        ...node,
        position: view.positions?.[node.id] ?? {
          x: point.x - topologyNodeSize[node.type!].width / 2,
          y: point.y - topologyNodeSize[node.type!].height / 2,
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
                id: randomUUID(),
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

export function connectRouter(
  spec: EnvironmentSpec,
  routerId: string,
  networkId: string,
): EnvironmentSpec {
  return {
    ...spec,
    routers: spec.routers?.map((router) =>
      router.id !== routerId ||
      router.interfaces.some((item) => item.networkId === networkId)
        ? router
        : {
            ...router,
            interfaces: [
              ...router.interfaces,
              { id: randomUUID(), networkId, address: "" },
            ],
          },
    ),
  };
}
