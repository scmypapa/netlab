import { expect, test, type Page } from "@playwright/test";
import type { Schema } from "../src/api/client";

const asset = {
  id: "web",
  name: "应用服务",
  templateId: "linux",
  resources: { cpu: 4, memoryMiB: 8192, diskGiB: 40 },
  interfaces: [
    {
      id: "nic",
      networkId: "lan",
      address: "10.10.0.2",
      mac: "02:00:00:00:00:02",
      primary: true,
    },
  ],
};
const spec = {
  assets: [asset],
  networks: [{ id: "lan", name: "应用网段", cidr: "10.10.0.0/24" }],
};
const environment = {
  id: "env",
  name: "实验环境",
  projectId: "default",
  revision: 1,
  status: "running",
  spec,
  appliedSpec: spec,
  permissions: ["read", "observe", "compose"],
  view: { positions: {} },
  createdAt: new Date().toISOString(),
  updatedAt: new Date().toISOString(),
};
const node = {
  id: "one",
  name: "计算节点 A",
  endpoint: "https://one.test",
  state: "ready",
  capacity: { cpu: 16, memoryMiB: 65536, diskGiB: 500 },
  reserved: { cpu: 8, memoryMiB: 16384, diskGiB: 100 },
  capabilities: ["vm", "container"],
  slots: 8,
  observedAt: new Date().toISOString(),
};
function readings(host: boolean, range: number): Schema<"MetricHistory"> {
  const end = new Date();
  const step = Math.max(10, Math.ceil(range / 600));
  const count = Math.max(1, Math.floor(range / step));
  const values = host
    ? {
        cpu: 2.5,
        cpu_capacity: 16,
        memory: 8 * 2 ** 30,
        memory_capacity: 64 * 2 ** 30,
        memory_available: 56 * 2 ** 30,
        disk_used: 40 * 2 ** 30,
        disk_capacity: 500 * 2 ** 30,
        disk_read: 3 * 2 ** 20,
        disk_write: 2 ** 20,
      }
    : {
        cpu: range > 60 ? 3 : 1.5,
        memory: 2 * 2 ** 30,
        receive: 2 ** 20,
        transmit: 2 ** 19,
        disk_read: 1000,
        disk_write: 500,
      };
  return {
    start: new Date(end.getTime() - range * 1000).toISOString(),
    end: end.toISOString(),
    stepSeconds: step,
    series: Object.entries(values).map(([metric, value]) => ({
      metric: metric as Schema<"MetricSeries">["metric"],
      nodeId: "one",
      assetId: host ? "" : "web",
      instanceId: host ? "" : "instance",
      points: Array.from({ length: count + 1 }, (_, index) => ({
        time: new Date(
          end.getTime() - ((count - index) * range * 1000) / count,
        ).toISOString(),
        value:
          value * (index === count ? 1 : 0.65 + Math.sin(index / 15) * 0.2),
      })),
    })),
  };
}
async function fixture(page: Page) {
  const requests: URL[] = [];
  await page.addInitScript(() => {
    class Events extends EventTarget {
      onopen: (() => void) | null = null;
      constructor() {
        super();
        setTimeout(() => this.onopen?.(), 0);
      }
      close() {}
    }
    Object.assign(window, { EventSource: Events });
  });
  await page.route("**/api/v1/**", async (route) => {
    const url = new URL(route.request().url());
    requests.push(url);
    const path = url.pathname.replace("/api/v1", "");
    let response: unknown = [];
    if (path === "/identity")
      response = { id: "admin", name: "admin", administrator: true };
    if (path === "/environments/env") response = environment;
    if (path === "/environments/env/state")
      response = {
        ...environment,
        assets: [
          {
            assetId: "web",
            instanceId: "instance",
            nodeId: "one",
            state: "running",
          },
        ],
      };
    if (path === "/templates")
      response = [
        {
          id: "linux",
          name: "Linux",
          kind: "container",
          version: 1,
          state: "ready",
          resources: asset.resources,
        },
      ];
    if (path === "/nodes")
      response = [node].filter((item) =>
        item.name.includes(url.searchParams.get("search") ?? ""),
      );
    if (path.endsWith("/metrics")) {
      response = readings(
        path === "/nodes/metrics",
        Number(url.searchParams.get("range")),
      );
    }
    await route.fulfill({ json: response });
  });
  return requests;
}

test("环境性能区分当前快照和历史，支持切换指标与资产下钻", async ({ page }) => {
  const requests = await fixture(page);
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "性能视图", exact: true }).click();
  const panel = page.getByRole("region", { name: "环境性能", exact: true });
  await expect(panel.getByRole("button", { name: /^CPU/ })).toContainText(
    "1.5",
  );
  await expect(panel).toContainText("已配置 4 核");
  await panel.getByText("30 天", { exact: true }).click();
  await expect
    .poll(() =>
      requests.some(
        (url) =>
          url.searchParams.get("range") === "2592000" &&
          url.searchParams.get("aggregate") === "true",
      ),
    )
    .toBe(true);
  await expect(panel.getByRole("button", { name: /^CPU/ })).toContainText(
    "1.5",
  );
  await panel.getByRole("button", { name: "放大内存", exact: true }).click();
  await expect(panel.getByRole("img", { name: "内存历史曲线" })).toBeVisible();
  await expect(panel.getByRole("img", { name: "CPU历史曲线" })).toHaveCount(0);
  await panel.getByRole("button", { name: "收起内存", exact: true }).click();
  await expect(panel.getByRole("img", { name: "CPU历史曲线" })).toBeVisible();
  await page.setViewportSize({ width: 1366, height: 900 });
  for (const width of [1366, 1920, 2560, 390]) {
    await page.setViewportSize({ width, height: 900 });
    await expect
      .poll(() =>
        page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),
      )
      .toBe(true);
    await page.screenshot({
      path: `test-results/environment-performance-${width}.png`,
    });
  }
  await page.getByRole("button", { name: "切换夜间主题", exact: true }).click();
  await expect(panel.getByRole("img", { name: "内存历史曲线" })).toBeVisible();
  await panel.getByRole("switch", { name: "实时刷新" }).press("Space");
  await panel.getByRole("button", { name: /应用服务/ }).click();
  await expect(
    page.getByRole("region", { name: "环境性能", exact: true }),
  ).toHaveCount(0);
  await expect(page.getByText("资源", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("radio", { name: "30 天", exact: true }),
  ).toBeChecked();
  await expect(
    page.getByRole("switch", { name: "实时刷新" }),
  ).not.toBeChecked();
  await expect
    .poll(() =>
      requests.some(
        (url) =>
          url.searchParams.get("assetId") === "web" &&
          url.searchParams.get("range") === "2592000",
      ),
    )
    .toBe(true);
  await page.getByRole("button", { name: "返回环境性能", exact: true }).click();
  await expect(panel).toBeVisible();
  await expect(
    panel.getByRole("radio", { name: "30 天", exact: true }),
  ).toBeChecked();
  await expect(
    panel.getByRole("switch", { name: "实时刷新" }),
  ).not.toBeChecked();
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await expect(panel).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "拓扑视图", exact: true }),
  ).toHaveAttribute("aria-pressed", "true");
  await expect(
    page.getByRole("button", { name: "预览变更", exact: true }),
  ).toBeVisible();
});

test("集群历史覆盖全量，节点卡片展示实测并下钻", async ({ page }) => {
  const requests = await fixture(page);
  await page.goto("/resources");
  const card = page.getByRole("button", { name: /计算节点 A/ });
  await expect(card).toContainText("2.5");
  await expect(card).toContainText("已分配 8 核");
  await card.click();
  const drawer = page.getByRole("dialog", { name: "计算节点 A" });
  await expect(drawer.getByRole("region", { name: "节点性能" })).toBeVisible();
  await expect
    .poll(() =>
      requests.some((url) => url.searchParams.get("nodeId") === "one"),
    )
    .toBe(true);
  await drawer.getByRole("button", { name: "关闭节点", exact: true }).click();
  await page
    .getByRole("textbox", { name: "搜索节点", exact: true })
    .fill("不存在的节点");
  await expect
    .poll(() =>
      requests.some(
        (url) =>
          url.pathname.endsWith("/nodes") &&
          url.searchParams.get("search") === "不存在的节点",
      ),
    )
    .toBe(true);
  await expect(page.getByText("没有匹配的节点", { exact: true })).toBeVisible();
  await page.getByRole("tab", { name: "集群性能", exact: true }).click();
  const panel = page.getByRole("region", { name: "集群性能", exact: true });
  await expect(panel).toContainText("节点工作盘");
  await expect(panel.getByRole("button", { name: /计算节点 A/ })).toBeVisible();
  await panel.getByText("7 天", { exact: true }).click();
  await expect
    .poll(() =>
      requests.some(
        (url) =>
          url.searchParams.get("range") === "604800" &&
          !url.searchParams.has("nodeId") &&
          url.searchParams.get("aggregate") === "true",
      ),
    )
    .toBe(true);
  for (const width of [1366, 1920, 2560, 390]) {
    await page.setViewportSize({ width, height: 900 });
    await expect
      .poll(() =>
        page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),
      )
      .toBe(true);
    await page.screenshot({
      path: `test-results/cluster-performance-${width}.png`,
    });
  }
});

test("历史仍可查时，实时采集失败不显示旧快照", async ({ page }) => {
  await fixture(page);
  await page.goto("/resources/performance");
  const panel = page.getByRole("region", { name: "集群性能", exact: true });
  await expect(panel.getByRole("button", { name: /^CPU/ })).toContainText(
    "2.5",
  );
  await page.route("**/nodes/metrics?**", async (route) => {
    if (new URL(route.request().url()).searchParams.get("range") === "60")
      return route.fulfill({ status: 502, json: { detail: "采集连接中断" } });
    await route.fallback();
  });
  await panel.getByRole("button", { name: "刷新性能", exact: true }).click();
  await expect(panel.getByRole("alert")).toContainText("采集连接中断", {
    timeout: 15_000,
  });
  await expect(panel.getByRole("button", { name: /^CPU/ })).toContainText("—");
});
