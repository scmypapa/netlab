import type { Edge, XYPosition } from "@xyflow/react";
import type { ELK, ElkNode } from "elkjs/lib/elk-api";
import type { TopologyNode } from "./topology";

export const topologyNodeSize = {
  asset: { width: 184, height: 64 },
  network: { width: 236, height: 84 },
  router: { width: 172, height: 64 },
};

export async function layoutTopology(
  nodes: TopologyNode[],
  edges: Edge[],
  engine: Pick<ELK, "layout">,
) {
  const byId = new Map(nodes.map((node) => [node.id, node]));
  const memberships = new Map<string, Set<string>>();
  for (const edge of edges) {
    if (
      byId.get(edge.source)?.type !== "network" ||
      byId.get(edge.target)?.type !== "asset"
    )
      continue;
    const networks = memberships.get(edge.target) ?? new Set<string>();
    networks.add(edge.source);
    memberships.set(edge.target, networks);
  }
  const groups = new Map<string, string[]>();
  const parents = new Map<string, string>();
  for (const [id, networks] of memberships) {
    if (networks.size !== 1) continue;
    const network = [...networks][0];
    parents.set(id, network);
    const children = groups.get(network) ?? [];
    children.push(id);
    groups.set(network, children);
  }
  const local = new Map<string, XYPosition>();
  const blocks: { id: string; width: number; height: number }[] = [];
  for (const node of nodes) {
    if (parents.has(node.id)) continue;
    const children = (groups.get(node.id) ?? []).sort();
    let size = topologyNodeSize[node.type!];
    if (children.length) {
      const columns = Math.ceil(Math.sqrt(children.length));
      const rows = Math.ceil(children.length / columns);
      size = {
        width: columns * 184 + (columns - 1) * 16 + 36,
        height: rows * 64 + (rows - 1) * 16 + 94,
      };
      children.forEach((id, index) =>
        local.set(id, {
          x: 18 + (index % columns) * 200,
          y: 76 + Math.floor(index / columns) * 80,
        }),
      );
    }
    blocks.push({ id: node.id, ...size });
  }
  const dimensions = new Map(blocks.map((block) => [block.id, block]));
  const links = edges
    .filter(
      (edge) => dimensions.has(edge.source) && dimensions.has(edge.target),
    )
    .sort((a, b) => a.id.localeCompare(b.id));
  const adjacency = new Map(
    blocks.map((block) => [block.id, new Set<string>()]),
  );
  for (const edge of links) {
    adjacency.get(edge.source)!.add(edge.target);
    adjacency.get(edge.target)!.add(edge.source);
  }
  const unvisited = new Set(blocks.map((block) => block.id).sort());
  const depth = new Map<string, number>();
  while (unvisited.size) {
    const component = [unvisited.values().next().value!];
    unvisited.delete(component[0]);
    for (let i = 0; i < component.length; i++)
      for (const neighbor of [...adjacency.get(component[i])!].sort())
        if (unvisited.delete(neighbor)) component.push(neighbor);
    // Orient only the layout graph. Network links and routing stay unchanged.
    const root = [...component].sort(
      (a, b) =>
        adjacency.get(b)!.size - adjacency.get(a)!.size || a.localeCompare(b),
    )[0];
    depth.set(root, 0);
    const queue = [root];
    for (let i = 0; i < queue.length; i++)
      for (const neighbor of [...adjacency.get(queue[i])!].sort()) {
        if (depth.has(neighbor)) continue;
        depth.set(neighbor, depth.get(queue[i])! + 1);
        queue.push(neighbor);
      }
  }
  const graph = await engine.layout<ElkNode>({
    id: "topology",
    layoutOptions: {
      "elk.algorithm": "layered",
      "elk.direction": "RIGHT",
      "elk.aspectRatio": "1.7",
      "elk.edgeRouting": "ORTHOGONAL",
      "elk.spacing.nodeNode": "44",
      "elk.spacing.componentComponent": "64",
      "elk.layered.spacing.nodeNodeBetweenLayers": "72",
      "elk.layered.wrapping.strategy": "MULTI_EDGE",
      "elk.padding": "[top=32,left=32,bottom=32,right=32]",
      "elk.randomSeed": "1",
    },
    children: blocks.sort((a, b) => a.id.localeCompare(b.id)),
    edges: links.map((edge) => {
      const forward =
        depth.get(edge.source)! < depth.get(edge.target)! ||
        (depth.get(edge.source) === depth.get(edge.target) &&
          edge.source < edge.target);
      return {
        id: edge.id,
        sources: [forward ? edge.source : edge.target],
        targets: [forward ? edge.target : edge.source],
      };
    }),
  });
  const positions = new Map(
    (graph.children ?? []).map((node) => [node.id, { x: node.x!, y: node.y! }]),
  );
  const sources = new Map(links.map((edge) => [edge.id, edge.source]));
  const paths = new Map(
    (graph.edges ?? []).map((edge) => {
      const points = edge.sections!.flatMap((section) => [
        section.startPoint,
        ...(section.bendPoints ?? []),
        section.endPoint,
      ]);
      return [
        edge.id,
        edge.sources[0] === sources.get(edge.id) ? points : points.reverse(),
      ];
    }),
  );
  for (const [id, parent] of parents) {
    const origin = positions.get(parent)!,
      point = local.get(id)!;
    positions.set(id, { x: origin.x + point.x, y: origin.y + point.y });
  }
  return { positions, dimensions, parents, paths };
}

export function absolutePositions(nodes: TopologyNode[]) {
  const byId = new Map(nodes.map((node) => [node.id, node]));
  return Object.fromEntries(
    nodes.map((node) => {
      const parent = node.parentId ? byId.get(node.parentId) : undefined;
      return [
        node.id,
        {
          x: node.position.x + (parent?.position.x ?? 0),
          y: node.position.y + (parent?.position.y ?? 0),
        },
      ];
    }),
  );
}
