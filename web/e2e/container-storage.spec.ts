import { expect, test } from "@playwright/test";

for (const shared of [false, true]) {
  test(`存储按池展示实际容量，不暴露${shared ? "同盘" : "异盘"}运行层预算`, async ({
    page,
  }) => {
    const runtime = {
      path: "/runtime/overlayfs",
      filesystem: "runtime",
      capacityBytes: 80 * 2 ** 30,
      availableBytes: 60 * 2 ** 30,
    };
    const storage = shared
      ? { ...runtime, path: "/netlab" }
      : {
          path: "/netlab",
          filesystem: "data",
          capacityBytes: 200 * 2 ** 30,
          availableBytes: 180 * 2 ** 30,
        };
    await page.route("**/api/v1/**", async (route) => {
      const path = new URL(route.request().url()).pathname.replace(
        "/api/v1",
        "",
      );
      let response: unknown = [];
      if (path === "/identity")
        response = { id: "admin", name: "admin", administrator: true };
      if (path === "/nodes")
        response = [
          {
            id: "one",
            name: "执行节点",
            endpoint: "https://node.test",
            state: "ready",
            observedAt: new Date().toISOString(),
            capabilities: ["container"],
            cpuAllocationRatio: 4,
            slots: 4,
            capacity: { cpu: 8, memoryMiB: 8192, diskGiB: 200 },
            reserved: { cpu: 1, memoryMiB: 1024, diskGiB: 20 },
          },
        ];
      if (path === "/storage-pools")
        response = [
          {
            id: "default:one",
            nodeIds: ["one"],
            name: "本地存储",
            default: true,
            driver: "directory",
            allocatedGiB: 4,
            capabilities: ["volumes"],
            storage,
            containerStorage: runtime,
            containerAllocatedGiB: 10,
          },
          {
            id: "data",
            nodeIds: ["one"],
            name: "外置数据盘",
            default: false,
            driver: "directory",
            allocatedGiB: 6,
            capabilities: ["volumes"],
            storage: { ...storage, path: "/data" },
          },
        ];
      if (path === "/nodes/metrics")
        response = {
          start: new Date().toISOString(),
          end: new Date().toISOString(),
          stepSeconds: 10,
          series: [],
        };
      await route.fulfill({ json: response });
    });
    await page.goto("/resources/storage");
    await expect(page.getByText("总容量", { exact: true })).toBeVisible();
    await expect(
      page.getByText(`${storage.capacityBytes / 2 ** 30} GiB`, { exact: true }),
    ).toBeVisible();
    await expect(
      page.getByText("/runtime/overlayfs", { exact: true }),
    ).toHaveCount(0);
    await expect(page.getByText(/写入层|预算/)).toHaveCount(0);
    for (const width of [390, 1366, 1920, 2560]) {
      await page.setViewportSize({ width, height: 900 });
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth,
        ),
      ).toBe(true);
    }
    await page
      .getByRole("group", { name: "选择存储池" })
      .getByRole("button", { name: /外置数据盘/ })
      .click();
    await expect(
      page.getByRole("heading", { name: "容器写入层", exact: true }),
    ).toHaveCount(0);
    await expect(page.getByText("总容量", { exact: true })).toBeVisible();
    await page.goto("/resources");
    await page.getByRole("button", { name: /执行节点/ }).click();
    await page.getByRole("tab", { name: "存储池", exact: true }).click();
    await expect(
      page.getByRole("heading", { name: "容器写入层", exact: true }),
    ).toHaveCount(0);
  });
}
