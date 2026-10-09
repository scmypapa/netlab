import { expect, test } from "@playwright/test";

const nodes = ["one", "two"].map((id) => ({
  id,
  name: `节点 ${id}`,
  state: "ready",
  endpoint: `https://${id}.test`,
  observedAt: "2026-10-09T08:00:00Z",
  slots: 4,
  capabilities: ["vm"],
  capacity: { cpu: 8, memoryMiB: 16384, diskGiB: 100 },
  reserved: { cpu: 0, memoryMiB: 0, diskGiB: 0 },
}));
const shared = {
  id: "shared",
  name: "共享存储",
  driver: "rbd",
  managed: true,
  nodeIds: ["one", "two"],
  allocatedGiB: 20,
  capabilities: ["vm-disks"],
  state: "ready",
  operationState: "succeeded",
  operationId: "configure",
  storage: {
    path: "/pool",
    filesystem: "ceph/netlab",
    capacityBytes: 100 * 2 ** 30,
    availableBytes: 80 * 2 ** 30,
  },
};

test("面板选盘准备节点，已有磁盘不参与初始化", async ({ page }) => {
  let selected = "";
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname.replace("/api/v1", "");
    let response: unknown = [];
    if (path === "/identity")
      response = { id: "admin", name: "admin", administrator: true };
    if (path === "/nodes") response = nodes;
    if (path === "/nodes/one/storage-device") {
      if (route.request().method() === "PUT") {
        expect(route.request().postDataJSON()).toEqual({
          device: "/dev/disk/by-id/empty",
        });
        selected = "/dev/disk/by-id/empty";
        response = {
          id: "prepare",
          state: "queued",
          kind: "configure-node-storage",
        };
      } else
        response = {
          selected,
          devices: [
            {
              path: "/dev/sda",
              model: "系统盘",
              serial: "system",
              sizeBytes: 64 * 2 ** 30,
              available: false,
              reason: "已挂载",
            },
            {
              path: "/dev/disk/by-id/empty",
              model: "空盘",
              serial: "empty",
              sizeBytes: 128 * 2 ** 30,
              available: true,
              reason: "",
            },
          ],
          operation: selected
            ? {
                id: "prepare",
                state: "succeeded",
                kind: "configure-node-storage",
              }
            : undefined,
        };
    }
    await route.fulfill({ json: response });
  });
  await page.goto("/resources/storage");
  await page.getByRole("button", { name: "选择专用盘", exact: true }).click();
  await expect(page.getByRole("radio", { name: /系统盘/ })).toBeDisabled();
  await expect(page.getByRole("radio", { name: /空盘/ })).toBeChecked();
  await expect(
    page.getByRole("button", { name: "确认选盘", exact: true }),
  ).toBeDisabled();
  await page
    .getByRole("checkbox", { name: "将所选空盘交给 Ceph 初始化", exact: true })
    .check();
  await page.getByRole("button", { name: "确认选盘", exact: true }).click();
  await expect(
    page.getByRole("dialog", { name: "Ceph 专用盘", exact: true }),
  ).toHaveCount(0);
  expect(selected).toBe("/dev/disk/by-id/empty");
});

test("共享池集中展示真实容量与服务，副本变更复用配置任务", async ({ page }) => {
  let replicas = 1;
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname.replace("/api/v1", "");
    let response: unknown = [];
    if (path === "/identity")
      response = { id: "admin", name: "admin", administrator: true };
    if (path === "/nodes") response = nodes;
    if (path === "/storage-pools") response = [shared];
    if (path === "/storage-pools/shared/ceph") {
      if (route.request().method() === "PUT") {
        expect(route.request().postDataJSON()).toEqual({ replicas: 2 });
        replicas = 2;
        response = { id: "replicas", state: "queued" };
      } else
        response = {
          health: "HEALTH_WARN",
          messages: ["1 pool(s) have no replicas configured"],
          replicas,
          osdsUp: 2,
          osdsTotal: 2,
          disks: [
            {
              name: "osd.0",
              host: "one",
              state: "up",
              capacityBytes: 100 * 2 ** 30,
              usedBytes: 20 * 2 ** 30,
            },
          ],
          daemons: [
            { name: "mon.one", role: "mon", host: "one", state: "running" },
          ],
        };
    }
    await route.fulfill({ json: response });
  });
  await page.goto("/resources/storage");
  await expect(page.getByText("osd.0", { exact: true })).toBeVisible();
  await expect(page.getByText("警告", { exact: true })).toBeVisible();
  for (const width of [390, 1366, 1920, 2560]) {
    await page.setViewportSize({ width, height: 900 });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    if (width === 1366)
      await page.screenshot({ path: "../data/storage-workspace-desktop.png" });
    if (width === 390)
      await page.screenshot({ path: "../data/storage-workspace-mobile.png" });
  }
  await page.getByRole("button", { name: "副本设置", exact: true }).click();
  await page.getByRole("textbox", { name: "副本数量", exact: true }).fill("2");
  await page.getByRole("button", { name: "应用", exact: true }).click();
  await expect.poll(() => replicas).toBe(2);
  await expect(
    page.getByRole("dialog", { name: "数据副本", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "切换夜间主题", exact: true }).click();
  await page.screenshot({ path: "../data/storage-workspace-dark.png" });
  await page.getByRole("tab", { name: "Ceph 服务", exact: true }).click();
  await expect(page.getByText("mon.one", { exact: true })).toBeVisible();
});

test("本地环境磁盘按需迁入指定共享池", async ({ page }) => {
  let submitted = false;
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname.replace("/api/v1", "");
    let response: unknown = [];
    if (path === "/identity")
      response = { id: "admin", name: "admin", administrator: true };
    if (path === "/nodes") response = nodes;
    if (path === "/storage-pools")
      response = [
        {
          ...shared,
          id: "default:one",
          name: "本地存储",
          managed: false,
          default: true,
          driver: "directory",
          nodeIds: ["one"],
        },
        shared,
      ];
    if (path === "/storage-pools/default:one/assets")
      response = [
        {
          environmentId: "env",
          environmentName: "域环境",
          revision: 3,
          assetId: "asset",
          assetName: "域控",
          nodeId: "one",
          sizeGiB: 20,
          state: "running",
        },
      ];
    if (path === "/environments/env")
      response = {
        id: "env",
        revision: 3,
        status: "running",
        appliedSpec: {
          assets: [
            {
              id: "asset",
              name: "域控",
              templateId: "windows",
              resources: { cpu: 2, memoryMiB: 4096, diskGiB: 20 },
              interfaces: [],
            },
          ],
        },
      };
    if (path === "/templates")
      response = [{ id: "windows", name: "Windows", kind: "vm" }];
    if (path === "/environments/env/assets/asset/migrations") {
      if (route.request().method() === "GET")
        response = [
          {
            id: "two",
            name: "节点 two",
            available: { cpu: 8, memoryMiB: 16384 },
            live: false,
          },
        ];
      else {
        expect(route.request().postDataJSON()).toMatchObject({
          expectedRevision: 3,
          targetNodeId: "two",
          targetStoragePoolId: "shared",
        });
        submitted = true;
        response = { id: "migrate", state: "queued" };
      }
    }
    await route.fulfill({ json: response });
  });
  await page.goto("/resources/storage?pool=default:one");
  await page.getByRole("tab", { name: "环境磁盘", exact: true }).click();
  await page.getByRole("button", { name: "迁移", exact: true }).click();
  await page.getByRole("textbox", { name: "目标节点", exact: true }).click();
  await page.getByRole("option", { name: /节点 two/ }).click();
  await page.getByRole("textbox", { name: "目标存储池", exact: true }).click();
  await page.getByRole("option", { name: "共享存储", exact: true }).click();
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "迁移", exact: true })
    .click();
  await expect.poll(() => submitted).toBe(true);
});
