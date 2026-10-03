import { expect, test } from "@playwright/test";

test("节点存储登记、引用拒绝与删除任务重试", async ({ page }) => {
  let added = false,
    failed = false,
    tries = 0;
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request(),
      path = new URL(request.url()).pathname.replace("/api/v1", "");
    let response: unknown = [],
      status = 200;
    const storage = {
      path: "/mnt/data/netlab-node/pool",
      filesystem: "root",
      capacityBytes: 100 * 2 ** 30,
      availableBytes: 80 * 2 ** 30,
    };
    if (path === "/identity")
      response = { id: "admin", name: "admin", administrator: true };
    if (path === "/nodes")
      response = [
        {
          id: "node",
          name: "实验节点",
          state: "ready",
          endpoint: "https://node.test",
          observedAt: "2026-10-03T08:00:00Z",
          capacity: { cpu: 8, memoryMiB: 16384, diskGiB: 100 },
          reserved: { cpu: 2, memoryMiB: 1024, diskGiB: 10 },
          capabilities: ["vm", "container"],
          slots: 4,
        },
      ];
    if (path === "/storage-pools") {
      if (request.method() === "POST") {
        expect(request.postDataJSON()).toEqual({
          nodeId: "node",
          name: "数据盘",
          directory: "/mnt/data",
        });
        added = true;
        status = 201;
        response = { id: "pool" };
      } else
        response = [
          {
            id: "default:node",
            nodeId: "node",
            name: "本地存储",
            default: true,
            driver: "directory",
            allocatedGiB: 0,
            capabilities: [],
            storage,
          },
          ...(added
            ? [
                {
                  id: "pool",
                  nodeId: "node",
                  name: "数据盘",
                  default: false,
                  driver: "directory",
                  allocatedGiB: 10,
                  capabilities: [],
                  storage,
                  state: failed ? "deleting" : "ready",
                  operationId: failed ? "delete" : undefined,
                  error: failed ? "存储池仍包含保留数据" : undefined,
                },
              ]
            : []),
        ];
    }
    if (path === "/storage-pools/pool") {
      status = 409;
      response = { detail: "资源仍在使用：环境：实验网络" };
    }
    if (path === "/operations/delete/retry") {
      tries++;
      failed = false;
      status = 202;
      response = { id: "delete" };
    }
    await route.fulfill({
      status,
      contentType: "application/json",
      body: JSON.stringify(response),
    });
  });
  await page.goto("/resources");
  await page.getByRole("button", { name: /实验节点/ }).click();
  await expect(page.getByText("80 / 100 GiB")).toBeVisible();
  await page.getByRole("button", { name: "接入存储" }).click();
  await page.getByRole("textbox", { name: "名称", exact: true }).fill("数据盘");
  await page.getByRole("textbox", { name: "节点上的目录" }).fill("/mnt/data");
  await page.getByRole("button", { name: "接入", exact: true }).click();
  await expect(page.getByText("数据盘", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "移除 数据盘", exact: true }).click();
  await page.getByRole("button", { name: "移除", exact: true }).click();
  await expect(page.getByText("资源仍在使用：环境：实验网络")).toBeVisible();
  await page.getByRole("button", { name: "取消", exact: true }).click();
  failed = true;
  await page.getByRole("button", { name: "关闭节点", exact: true }).click();
  await page.reload();
  await page.getByRole("button", { name: /实验节点/ }).click();
  await expect(page.getByText("存储池仍包含保留数据")).toBeVisible();
  await page.getByRole("button", { name: "重试删除 数据盘" }).click();
  await expect.poll(() => tries).toBe(1);
  await page.screenshot({
    path: "../data/storage-panel-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "../data/storage-panel-mobile.png",
    fullPage: true,
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
});
