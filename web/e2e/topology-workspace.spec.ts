import { expect, test, type Page } from "@playwright/test";
import ELK from "elkjs";
import type {
  Environment,
  EnvironmentSpec,
  Operation,
} from "../src/api/client";
import {
  topology,
  type TopologyNode,
} from "../src/features/workbench/topology";
import {
  absolutePositions,
  topologyNodeSize,
} from "../src/features/workbench/topologyLayout";
const engine = new ELK();

function scenario(count = 11): EnvironmentSpec {
  return {
    networks: Array.from({ length: count }, (_, i) => ({
      id: `lan-${i}`,
      name: `区域 ${i + 1}`,
      cidr: `10.${i % 2}.0.0/24`,
    })),
    routers: Array.from({ length: count === 1 ? 0 : count }, (_, i) => ({
      id: `router-${i}`,
      name: `路由器 ${i + 1}`,
      interfaces: [i, (i + 1) % count].map((index, n) => ({
        id: `r${i}-${n}`,
        networkId: `lan-${index}`,
        address: `10.${index % 2}.0.${i + 1}`,
      })),
    })),
    assets: Array.from({ length: count * 4 }, (_, i) => ({
      id: `asset-${i}`,
      name: `设备 ${i + 1}`,
      templateId: "linux",
      resources: { cpu: 0.25, memoryMiB: 128, diskGiB: 2 },
      interfaces: [
        {
          id: `nic-${i}`,
          networkId: `lan-${Math.floor(i / 4)}`,
          address: `10.${Math.floor(i / 4) % 2}.0.${(i % 4) + 32}`,
          mac: "",
          primary: true,
        },
      ],
    })),
  };
}

function rectangle(
  node: TopologyNode,
  positions: ReturnType<typeof absolutePositions>,
) {
  return {
    ...positions[node.id],
    width: Number(node.style?.width ?? topologyNodeSize[node.type!].width),
    height: Number(node.style?.height ?? topologyNodeSize[node.type!].height),
  };
}

for (const count of [1, 11, 64]) {
  test(`layout groups endpoints without collisions for ${count} networks`, async () => {
    const spec = scenario(count),
      graph = await topology(spec, [], engine),
      positions = absolutePositions(graph.nodes);
    expect(graph.nodes).toHaveLength(
      spec.networks.length + spec.assets.length + spec.routers!.length,
    );
    const overlaps: string[] = [];
    for (const node of graph.nodes) {
      const a = rectangle(node, positions);
      expect(Number.isFinite(a.x) && Number.isFinite(a.y)).toBe(true);
      if (node.parentId) {
        const parent = rectangle(
          graph.nodes.find((item) => item.id === node.parentId)!,
          positions,
        );
        expect(a.x).toBeGreaterThan(parent.x);
        expect(a.y).toBeGreaterThan(parent.y + 60);
        expect(a.x + a.width).toBeLessThan(parent.x + parent.width);
        expect(a.y + a.height).toBeLessThan(parent.y + parent.height);
      }
      for (const other of graph.nodes) {
        if (
          node.id >= other.id ||
          node.parentId === other.id ||
          other.parentId === node.id
        )
          continue;
        const b = rectangle(other, positions);
        if (!(
          a.x + a.width <= b.x ||
          b.x + b.width <= a.x ||
          a.y + a.height <= b.y ||
          b.y + b.height <= a.y
        ))
          overlaps.push(`${node.id} overlaps ${other.id}`);
      }
    }
    expect(overlaps).toEqual([]);
    const crossings: string[] = [];
    for (const edge of graph.edges.filter((item) => !item.hidden)) {
      const points = edge.data!.points as { x: number; y: number }[];
      for (const node of graph.nodes.filter(
        (item) =>
          !item.parentId && item.id !== edge.source && item.id !== edge.target,
      )) {
        const box = rectangle(node, positions);
        for (let i = 1; i < points.length; i++) {
          const a = points[i - 1],
            b = points[i];
          const horizontal =
            a.y === b.y &&
            a.y > box.y &&
            a.y < box.y + box.height &&
            Math.max(a.x, b.x) > box.x &&
            Math.min(a.x, b.x) < box.x + box.width;
          const vertical =
            a.x === b.x &&
            a.x > box.x &&
            a.x < box.x + box.width &&
            Math.max(a.y, b.y) > box.y &&
            Math.min(a.y, b.y) < box.y + box.height;
          if (horizontal || vertical)
            crossings.push(`${edge.id} crosses ${node.id}`);
        }
      }
    }
    expect(crossings).toEqual([]);
    expect(graph.edges.filter((edge) => !edge.hidden)).toHaveLength(
      count === 1 ? 0 : count * 2,
    );
    expect(
      new Set(
        graph.nodes
          .filter((node) => node.parentId)
          .map((node) => node.parentId),
      ).size,
    ).toBe(count);
    const reordered = await topology(
      {
        ...spec,
        networks: [...spec.networks].reverse(),
        assets: [...spec.assets].reverse(),
        routers: [...spec.routers!].reverse(),
      },
      [],
      engine,
    );
    expect(absolutePositions(reordered.nodes)).toEqual(positions);
  });
}

test("multi-homed assets and disconnected networks retain all identities and links", async () => {
  const spec = scenario(3);
  spec.routers = [];
  spec.assets[0].interfaces.push({
    ...spec.assets[0].interfaces[0],
    id: "second",
    networkId: "lan-2",
  });
  spec.networks.push({ id: "empty", name: "独立网段", cidr: "10.0.0.0/24" });
  const graph = await topology(spec, [], engine);
  expect(
    graph.nodes.find((node) => node.id === "asset-0")?.parentId,
  ).toBeUndefined();
  expect(graph.edges.filter((edge) => edge.target === "asset-0")).toHaveLength(
    2,
  );
  expect(
    graph.edges
      .filter((edge) => edge.target === "asset-0")
      .every((edge) => !edge.hidden),
  ).toBe(true);
  expect(graph.nodes.filter((node) => node.type === "network")).toHaveLength(4);
  expect(graph.nodes.find((node) => node.id === "empty")).toBeDefined();
});

test("topology changes automatically place new assets in their subnet", async () => {
  const spec = scenario(3);
  spec.assets.push({
    ...spec.assets[0],
    id: "new-device",
    interfaces: [{ ...spec.assets[0].interfaces[0], id: "new-nic" }],
  });
  const added = await topology(spec, [], engine),
    positions = absolutePositions(added.nodes);
  expect(added.nodes.find((node) => node.id === "new-device")?.parentId).toBe(
    "lan-0",
  );
  expect(positions["new-device"].x).toBeGreaterThan(positions["lan-0"].x);
});

async function fixture(page: Page, readOnly = false) {
  const spec = scenario();
  const operation: Operation = {
    id: "start-task",
    environmentId: "env",
    kind: "start",
    state: "failed",
    phase: "rolled-back",
    completed: 0,
    total: spec.assets.length,
    retryable: true,
    error:
      "network: configure environment access rules: netlink receive: recvmsg: no buffer space available",
    results: spec.assets.map((asset) => ({
      assetId: asset.id,
      instanceId: asset.id,
      state: "destroyed",
    })),
    createdAt: "2026-10-27T08:00:00Z",
    updatedAt: "2026-10-27T08:00:00Z",
  };
  const environment: Environment = {
    id: "env",
    name: "多区域网络",
    projectId: "lab",
    revision: 1,
    spec,
    appliedSpec: spec,
    status: "failed",
    error: operation.error,
    permissions: readOnly ? ["read"] : ["read", "compose", "operate"],
    view: {},
    createdAt: operation.createdAt,
    updatedAt: operation.updatedAt,
  };
  const views: Environment["view"][] = [];
  let generation = 0;
  await page.addInitScript(() => {
    class Events extends EventTarget {
      onopen: (() => void) | null = null;
      constructor() {
        super();
        Object.assign(window, { topologyEvents: this });
        setTimeout(() => this.onopen?.(), 0);
      }
      close() {}
    }
    Object.assign(window, { EventSource: Events });
  });
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname.replace("/api/v1", "");
    let json: unknown = [];
    if (path === "/identity")
      json = { id: "user", name: "operator", administrator: !readOnly };
    else if (path === "/environments/env") json = environment;
    else if (path === "/environments/env/state")
      json = {
        id: "env",
        revision: 1,
        status: environment.status,
        operation,
        assets: spec.assets.map((asset) => ({
          assetId: asset.id,
          state: generation % 2 ? "running" : "stopped",
          instanceId: asset.id,
          nodeId: "node",
          observedAt: new Date().toISOString(),
        })),
        updatedAt: new Date().toISOString(),
      };
    else if (path === "/operations") json = [operation];
    else if (path.endsWith("/view")) {
      environment.view = route.request().postDataJSON();
      views.push(environment.view);
      return route.fulfill({ status: 204 });
    }
    await route.fulfill({ json });
  });
  await page.goto("/environments/env");
  await expect(page.locator(".react-flow__node")).toHaveCount(66);
  return {
    views,
    update: async () => {
      generation++;
      await page.evaluate(() =>
        (
          window as unknown as { topologyEvents: EventTarget }
        ).topologyEvents.dispatchEvent(new Event("runtime.changed")),
      );
    },
  };
}

test("tasks open on the right without stealing canvas height, with working keyboard and fullscreen", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1920, height: 1080 });
  const { views, update } = await fixture(page);
  const canvas = page.getByLabel("拓扑画布", { exact: true });
  await expect(page.getByRole("dialog", { name: "运行任务" })).toHaveCount(0);
  await expect
    .poll(async () => (await canvas.boundingBox())!.height)
    .toBeGreaterThan(700);
  const viewport = page.locator(".react-flow__viewport");
  await page.getByRole("button", { name: "放大画布", exact: true }).click();
  const transform = await viewport.getAttribute("style");
  const height = (await canvas.boundingBox())!.height;
  await page.getByRole("button", { name: "运行任务", exact: true }).click();
  const drawer = page.getByRole("dialog", { name: "运行任务" });
  await expect(drawer).toContainText("no buffer space available");
  await expect(drawer).toContainText("已回滚");
  expect((await canvas.boundingBox())!.height).toBe(height);
  await page.keyboard.press("Escape");
  await expect(drawer).toHaveCount(0);
  await update();
  await expect.poll(() => viewport.getAttribute("style")).toBe(transform);
  await page.getByRole("button", { name: "全屏拓扑", exact: true }).click();
  await expect
    .poll(() =>
      page.evaluate(() =>
        document.fullscreenElement?.getAttribute("aria-label"),
      ),
    )
    .toBe("拓扑画布");
  await expect
    .poll(async () => (await canvas.boundingBox())!.height)
    .toBe(1080);
  await page.getByRole("button", { name: "退出全屏拓扑", exact: true }).click();
  await expect
    .poll(() => page.evaluate(() => document.fullscreenElement === null))
    .toBe(true);
  await expect(
    page.getByRole("button", { name: "重新布局", exact: true }),
  ).toHaveCount(0);
  const asset = page.locator('.react-flow__node[data-id="asset-0"]');
  const originalPosition = await asset.getAttribute("style");
  const rect = await asset.boundingBox();
  await page.mouse.move(rect!.x + rect!.width / 2, rect!.y + rect!.height / 2);
  await page.mouse.down();
  await page.mouse.move(
    rect!.x + rect!.width / 2 + 90,
    rect!.y + rect!.height / 2 + 60,
    { steps: 8 },
  );
  await page.mouse.up();
  expect(await asset.getAttribute("style")).toBe(originalPosition);
  expect(views).toHaveLength(0);
  for (const width of [390, 1366, 1920, 2560]) {
    await page.setViewportSize({ width, height: 900 });
    await page.getByRole("button", { name: "适应画布", exact: true }).click();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    await page.screenshot({ path: `test-results/topology-${width}.png` });
    await page.getByRole("button", { name: "切换夜间主题" }).click();
    await page.screenshot({ path: `test-results/topology-${width}-dark.png` });
    await page.getByRole("button", { name: "切换日间主题" }).click();
  }
});

test("read-only observers cannot retry tasks", async ({ page }) => {
  await fixture(page, true);
  await expect(
    page.getByRole("button", { name: "重新布局", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "运行任务", exact: true }).click();
  await expect(page.getByRole("dialog", { name: "运行任务" })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "重试", exact: true }),
  ).toHaveCount(0);
});
