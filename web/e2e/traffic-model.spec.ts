import { expect, test } from "@playwright/test";
import ELK from "elkjs";
import {
  getSmartEdge,
  pathfindingAStarNoDiagonal,
  svgDrawSmoothStepLinePath,
} from "@tisoap/react-flow-smart-edge";
import type { EnvironmentSpec, Schema } from "../src/api/client";
import {
  buildTrafficLinks,
  describeEndpoint,
  selectionNetwork,
  trafficTopology,
} from "../src/features/workbench/trafficModel";
import { topology } from "../src/features/workbench/topology";
import { absolutePositions } from "../src/features/workbench/topologyLayout";

const spec: EnvironmentSpec = {
  networks: [
    {
      id: "a",
      name: "生产交换机",
      cidr: "10.0.0.0/24",
      devices: [
        {
          id: "device",
          name: "现场 PLC",
          address: "10.0.0.20",
          mac: "aa:bb:cc:dd:ee:20",
        },
      ],
    },
    { id: "b", name: "隔离交换机", cidr: "10.0.0.0/24" },
  ],
  routers: [
    {
      id: "router",
      name: "边界路由器",
      interfaces: [{ id: "ra", networkId: "a", address: "10.0.0.1" }],
    },
  ],
  assets: ["a", "b"].map((networkId, i) => ({
    id: `asset-${i}`,
    name: `业务主机${i}`,
    templateId: "t",
    resources: { cpu: 1, memoryMiB: 512, diskGiB: 2 },
    interfaces: [
      {
        id: `nic-${i}`,
        networkId,
        address: "10.0.0.2",
        mac: `02:00:00:00:00:0${i + 1}`,
        primary: true,
      },
    ],
  })),
};
const flow = (
  source: string,
  destination: string,
  more: Partial<Schema<"CaptureFlow">> = {},
): Schema<"CaptureFlow"> => ({
  source,
  destination,
  networkId: "a",
  protocol: "UDP",
  sourcePort: 1,
  destinationPort: 2,
  bytes: 100,
  packets: 1,
  bytesPerSecond: 10,
  firstSeen: "2026-10-27T00:00:00Z",
  lastSeen: "2026-10-27T00:00:01Z",
  ...more,
});

test("通信身份只来自环境，IP和MAC归到同一资产，重叠网段不串联", () => {
  expect(
    describeEndpoint(spec, flow("10.0.0.2", "10.0.0.1"), "source")?.id,
  ).toBe("asset-0");
  expect(
    describeEndpoint(
      spec,
      flow("02:00:00:00:00:01", "ff:ff:ff:ff:ff:ff"),
      "source",
    )?.id,
  ).toBe("asset-0");
  expect(
    describeEndpoint(
      spec,
      flow("10.0.0.2", "10.0.0.1", { networkId: "b" }),
      "source",
    )?.id,
  ).toBe("asset-1");
  expect(
    describeEndpoint(
      spec,
      flow("10.0.0.2", "10.0.0.1", { networkId: undefined }),
      "source",
    ),
  ).toBeUndefined();
  const links = buildTrafficLinks(
    spec,
    [
      flow("10.0.0.2", "10.0.0.1"),
      flow("02:00:00:00:00:01", "ff:ff:ff:ff:ff:ff"),
      flow("10.0.0.2", "224.0.0.251"),
      flow("10.0.0.2", "8.8.8.8"),
      flow("10.0.0.2", "aa:bb:cc:dd:ee:20"),
      flow("192.0.2.3", "198.51.100.4", { networkId: undefined }),
      flow("10.0.0.2", "10.0.0.1", { bytes: 0, packets: 0, bytesPerSecond: 0 }),
    ],
    [],
  );
  const ids = new Set(["asset-0", "router", "a", "device"]);
  expect(links).toHaveLength(5);
  expect(
    links.every((link) => ids.has(link.source) && ids.has(link.target)),
  ).toBe(true);
  expect(links.filter((link) => link.target === "a")).toHaveLength(3);
  expect(
    selectionNetwork(spec, { source: "asset-0", destination: "router" }),
  ).toBe("a");
});

for (const count of [2, 40, 100])
  test(`有实际通信的 ${count} 个资产按交换机紧凑分组，空闲对象不加入`, async () => {
    const star: EnvironmentSpec = {
      networks: [
        { id: "lan", name: "风电生产控制交换机", cidr: "10.0.0.0/24" },
      ],
      routers: [],
      assets: Array.from({ length: count + 1 }, (_, i) => ({
        id: String(i),
        name:
          i % 4
            ? "风电控制设备" + i
            : "海上风电场工业协议转换与通信控制设备" + i,
        templateId: "t",
        resources: { cpu: 1, memoryMiB: 128, diskGiB: 2 },
        interfaces: [
          {
            id: "nic" + i,
            networkId: "lan",
            address: "10.0.0." + (i + 1),
            mac: "",
            primary: true,
          },
        ],
      })),
    };
    const links = buildTrafficLinks(
      star,
      star.assets.slice(1, count).map((asset) =>
        flow("10.0.0.1", asset.interfaces[0].address, {
          sourceAssetId: "0",
          destinationAssetId: asset.id,
          networkId: "lan",
        }),
      ),
      [],
    );
    const active = trafficTopology(star, links);
    expect(active.assets).toHaveLength(count);
    expect(active.assets.some((asset) => asset.id === String(count))).toBe(
      false,
    );
    const graph = await topology(active, [], new ELK()),
      positions = absolutePositions(graph.nodes);
    const children = graph.nodes.filter((node) => node.parentId);
    const source = children.find((node) => node.id === "0")!,
      target = children.find((node) => node.id === String(count - 1))!;
    const route = getSmartEdge({
      sourceX: positions[source.id].x + Number(source.style!.width),
      sourceY: positions[source.id].y + 32,
      targetX: positions[target.id].x,
      targetY: positions[target.id].y + 32,
      sourcePosition: "right",
      targetPosition: "left",
      nodes: children.map((node) => ({
        ...node,
        parentId: undefined,
        position: positions[node.id],
        measured: {
          width: Number(node.style!.width),
          height: Number(node.style!.height),
        },
      })),
      options: {
        nodePadding: 5,
        gridRatio: 4,
        generatePath: pathfindingAStarNoDiagonal,
        drawEdge: svgDrawSmoothStepLinePath(),
      },
    });
    expect(route instanceof Error ? route.message : "routed").toBe("routed");
    for (const [i, a] of children.entries())
      for (const b of children.slice(i + 1))
        expect(
          positions[a.id].x + Number(a.style!.width) <= positions[b.id].x ||
            positions[b.id].x + Number(b.style!.width) <= positions[a.id].x ||
            positions[a.id].y + Number(a.style!.height) <= positions[b.id].y ||
            positions[b.id].y + Number(b.style!.height) <= positions[a.id].y,
        ).toBe(true);
    const group = graph.nodes.find((node) => node.id === "lan")!;
    expect(
      Math.max(
        Number(group.style!.width) / Number(group.style!.height),
        Number(group.style!.height) / Number(group.style!.width),
      ),
    ).toBeLessThan(6);
  });
