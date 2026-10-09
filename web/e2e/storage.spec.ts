import { expect, test } from "@playwright/test";

for (const scenario of [
  "missing-disk",
  "preparing",
  "ready",
  "failed",
] as const) {
  test(`Ceph 共享存储显示真实条件和状态：${scenario}`, async ({ page }) => {
    let retries = 0;
    await page.route("**/api/v1/**", async (route) => {
      const path = new URL(route.request().url()).pathname.replace(
        "/api/v1",
        "",
      );
      let response: unknown = [];
      if (path === "/identity")
        response = { id: "admin", name: "admin", administrator: true };
      if (path === "/nodes")
        response = ["one", "two"].map((id) => ({
          id,
          name: `节点 ${id}`,
          state: "ready",
          endpoint: `https://${id}.test`,
          observedAt: "2026-10-09T08:00:00Z",
          slots: 4,
          capabilities: ["vm"],
          capacity: { cpu: 8, memoryMiB: 16384, diskGiB: 100 },
          reserved: { cpu: 0, memoryMiB: 0, diskGiB: 0 },
          storageDevice:
            id === "one" && scenario !== "missing-disk"
              ? "/dev/disk/by-id/ceph"
              : undefined,
        }));
      if (path === "/storage-pools" && scenario !== "missing-disk")
        response = [
          {
            id: "shared",
            name: "共享存储",
            driver: "rbd",
            managed: true,
            nodeIds: ["one", "two"],
            allocatedGiB: 10,
            capabilities: ["vm-disks"],
            state: scenario === "ready" ? "ready" : "preparing",
            operationState:
              scenario === "ready"
                ? "succeeded"
                : scenario === "failed"
                  ? "failed"
                  : "running",
            operationId: "configure",
            error: scenario === "failed" ? "Ceph 专用盘无法访问" : undefined,
          },
        ];
      if (path === "/operations/configure/retry") {
        retries++;
        response = { id: "configure" };
      }
      await route.fulfill({ json: response });
    });
    await page.goto("/resources");
    await page.getByRole("button", { name: /节点 one/ }).click();
    await page.getByRole("button", { name: /Ceph 共享存储/ }).click();
    if (scenario === "missing-disk") {
      await expect(page.getByText("✓ 两个就绪的 KVM 节点")).toBeVisible();
      await expect(
        page.getByText("○ 至少一个节点已指定 Ceph 专用盘"),
      ).toBeVisible();
    } else {
      await expect(
        page.getByText("/dev/disk/by-id/ceph", { exact: true }),
      ).toBeVisible();
      await expect(page.getByText("成员：节点 one、节点 two")).toBeVisible();
      await expect(
        page.getByText(
          scenario === "ready"
            ? "已启用"
            : scenario === "failed"
              ? "配置失败"
              : "配置中",
          { exact: true },
        ),
      ).toBeVisible();
    }
    if (scenario === "failed") {
      await page.getByRole("button", { name: "重试配置", exact: true }).click();
      await expect.poll(() => retries).toBe(1);
    }
    for (const width of [390, 1366]) {
      await page.setViewportSize({ width, height: 900 });
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth,
        ),
      ).toBe(true);
    }
  });
}

test("持久卷创建、扩容和删除使用同一节点工作区", async ({ page }) => {
  let volume: Record<string, unknown> | undefined;
  const requests: { method: string; body: unknown }[] = [];
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname.replace("/api/v1", "");
    let response: unknown = [],
      status = 200;
    if (path === "/identity")
      response = { id: "admin", name: "admin", administrator: true };
    if (path === "/nodes")
      response = [
        {
          id: "node",
          name: "数据节点",
          state: "ready",
          endpoint: "https://node.test",
          observedAt: new Date().toISOString(),
          capacity: { cpu: 8, memoryMiB: 16384, diskGiB: 100 },
          reserved: { cpu: 0, memoryMiB: 0, diskGiB: 0 },
          capabilities: ["vm", "container"],
          slots: 4,
        },
      ];
    if (path === "/storage-pools")
      response = [
        {
          id: "default:node",
          nodeIds: ["node"],
          name: "本地存储",
          default: true,
          driver: "directory",
          allocatedGiB: 0,
          capabilities: ["volumes"],
          storage: {
            path: "/var/lib/netlab",
            filesystem: "root",
            capacityBytes: 100 * 2 ** 30,
            availableBytes: 80 * 2 ** 30,
          },
        },
      ];
    if (path.startsWith("/volumes")) {
      if (request.method() === "GET") response = volume ? [volume] : [];
      else {
        const body =
          request.method() === "DELETE" ? undefined : request.postDataJSON();
        requests.push({ method: request.method(), body });
        if (request.method() === "POST")
          volume = {
            ...body,
            id: "data",
            nodeId: "node",
            state: "ready",
            references: [],
          };
        if (request.method() === "PUT") volume = { ...volume, ...body };
        if (request.method() === "DELETE") volume = undefined;
        status = 202;
        response = { id: "operation", state: "queued", total: 1, completed: 0 };
      }
    }
    await route.fulfill({ status, json: response });
  });
  await page.goto("/resources");
  await page.getByRole("button", { name: /数据节点/ }).click();
  await page.getByRole("tab", { name: "数据卷", exact: true }).click();
  await page.getByRole("button", { name: "创建数据卷" }).click();
  let dialog = page.getByRole("dialog", { name: "创建数据卷" });
  await dialog
    .getByRole("textbox", { name: "名称", exact: true })
    .fill("业务数据");
  await dialog.getByRole("textbox", { name: "容量 · GiB" }).fill("2");
  await dialog.getByRole("button", { name: "创建", exact: true }).click();
  await expect(page.getByText("业务数据", { exact: true })).toBeVisible();
  expect(requests[0].body).toEqual({
    name: "业务数据",
    kind: "vm",
    storagePoolId: "default:node",
    sizeGiB: 2,
  });
  for (const width of [390, 1366]) {
    await page.setViewportSize({ width, height: 900 });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    await page.screenshot({
      path: `../data/volumes-${width}.png`,
      animations: "disabled",
    });
  }
  await page.getByRole("button", { name: "操作 业务数据" }).click();
  await page.getByRole("menuitem", { name: "扩容", exact: true }).click();
  dialog = page.getByRole("dialog", { name: "扩容 业务数据" });
  await dialog.getByRole("textbox", { name: "容量 · GiB" }).fill("4");
  await dialog.getByRole("button", { name: "扩容", exact: true }).click();
  await expect(page.getByText("4 GiB", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "操作 业务数据" }).click();
  await page.getByRole("menuitem", { name: "删除", exact: true }).click();
  await page
    .getByRole("dialog", { name: "删除 业务数据" })
    .getByRole("button", { name: "删除", exact: true })
    .click();
  await expect(page.getByText("暂无数据卷", { exact: true })).toBeVisible();
  expect(requests.slice(1)).toEqual([
    { method: "PUT", body: { sizeGiB: 4 } },
    { method: "DELETE", body: undefined },
  ]);
});

test("仓库目录中的备份通过标准创建接口恢复", async ({ page }) => {
  let restored = false;
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request(),
      path = new URL(request.url()).pathname.replace("/api/v1", "");
    let response: unknown = [],
      status = 200;
    if (path === "/identity")
      response = { id: "admin", name: "admin", administrator: true };
    if (path === "/nodes")
      response = [
        {
          id: "node",
          name: "恢复节点",
          state: "ready",
          endpoint: "https://node.test",
          observedAt: "2026-10-03T08:00:00Z",
          capacity: { cpu: 8, memoryMiB: 16384, diskGiB: 100 },
          reserved: { cpu: 0, memoryMiB: 0, diskGiB: 0 },
          capabilities: ["vm", "container"],
          slots: 4,
        },
      ];
    if (path === "/backup-repositories")
      response = [
        {
          id: "repository",
          name: "历史备份",
          nodeId: "node",
          state: "ready",
          location: "/mnt/backups",
        },
      ];
    if (path === "/backup-repositories/repository/backups")
      response = [
        {
          id: "backup",
          name: "混合网络",
          repositoryId: "repository",
          state: "ready",
          sizeBytes: 2 ** 30,
          createdAt: "2026-10-03T08:00:00Z",
        },
      ];
    if (path === "/environments" && request.method() === "POST") {
      expect(request.postDataJSON()).toEqual({
        name: "恢复环境",
        backupId: "backup",
        run: false,
      });
      restored = true;
      status = 201;
      response = { id: "restored" };
    }
    await route.fulfill({
      status,
      contentType: "application/json",
      body: JSON.stringify(response),
    });
  });
  await page.goto("/resources");
  await page.getByRole("button", { name: /恢复节点/ }).click();
  await page.getByRole("tab", { name: "备份仓库" }).click();
  await page.getByRole("button", { name: "历史备份操作" }).click();
  await page.getByRole("menuitem", { name: "查看备份" }).click();
  await expect(page.getByText("混合网络", { exact: true })).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  await page.screenshot({
    path: "../data/backup-catalog-mobile.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 1366, height: 900 });
  await page.screenshot({
    path: "../data/backup-catalog-desktop.png",
    fullPage: true,
  });
  await page.getByRole("button", { name: "混合网络操作" }).click();
  await page.getByRole("menuitem", { name: "恢复为新环境" }).click();
  const dialog = page.getByRole("dialog", { name: "从备份创建环境" });
  await dialog.getByRole("textbox", { name: "环境名称" }).fill("恢复环境");
  await dialog.getByRole("checkbox", { name: "创建后启动" }).uncheck();
  await dialog.getByRole("button", { name: "创建", exact: true }).click();
  await expect.poll(() => restored).toBe(true);
  await expect(page).toHaveURL(/environments\/restored/);
});

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
          nodeIds: ["node"],
          name: "数据盘",
          driver: "directory",
          directory: "/mnt/data",
        });
        added = true;
        status = 201;
        response = { id: "pool" };
      } else
        response = [
          {
            id: "default:node",
            nodeIds: ["node"],
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
                  nodeIds: ["node"],
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
  await expect(page.getByText("准备中", { exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: /Ceph 共享存储/ }).click();
  await expect(page.getByText("○ 两个就绪的 KVM 节点")).toBeVisible();
  await expect(
    page.getByText("○ 至少一个节点已指定 Ceph 专用盘"),
  ).toBeVisible();
  await page.getByText("配置专用盘", { exact: true }).click();
  await expect(
    page.getByText(/sudo bash netlab-release\/scripts\/install-node.sh/),
  ).toBeVisible();
  await page.getByRole("button", { name: /Ceph 共享存储/ }).click();
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
  await page.getByRole("button", { name: "重试 数据盘", exact: true }).click();
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
