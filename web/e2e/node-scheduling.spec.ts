import { expect, test } from "@playwright/test";
import type { Node } from "../src/api/client";

test("宿主内存设置使用服务端额度，保存后刷新节点详情", async ({ page }) => {
  let node: Node = {
    id: "one",
    name: "计算节点",
    endpoint: "https://node.test",
    state: "ready",
    capacity: { cpu: 8, memoryMiB: 4096, diskGiB: 100 },
    cpuAllocationRatio: 4,
    memoryReserveMiB: 0,
    allocatableMemoryMiB: 3072,
    reserved: { cpu: 1, memoryMiB: 1024, diskGiB: 5 },
    capabilities: ["container"],
    slots: 4,
    observedAt: "2026-10-28T08:00:00Z",
  };
  const requests: unknown[] = [];
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname.replace("/api/v1", "");
    let response: unknown = [];
    if (path === "/identity")
      response = { id: "admin", name: "admin", administrator: true };
    if (path === "/nodes") response = [node];
    if (path === "/nodes/metrics")
      response = {
        start: "2026-10-28T08:00:00Z",
        end: "2026-10-28T08:01:00Z",
        stepSeconds: 10,
        series: [],
      };
    if (path === "/nodes/one/scheduling") {
      const body = route.request().postDataJSON();
      requests.push(body);
      node = {
        ...node,
        ...body,
        allocatableMemoryMiB: body.memoryReserveMiB === 512 ? 3584 : 3072,
      };
      return route.fulfill({ status: 204 });
    }
    await route.fulfill({ json: response });
  });
  await page.goto("/resources");
  await page.getByRole("button", { name: /计算节点.*可用/ }).click();
  const panel = page.getByRole("region", { name: "计算资源分配", exact: true });
  const available = panel.locator("dl > div").filter({ hasText: "可分配内存" });
  const host = panel.locator("dl > div").filter({ hasText: "宿主预留内存" });
  await expect(available).toContainText("3 GiB");
  await expect(host).toContainText("1 GiB");
  await expect(
    panel.getByRole("button", { name: "应用", exact: true }),
  ).toBeDisabled();
  await panel.getByText("自定义", { exact: true }).click();
  await panel
    .getByRole("textbox", { name: "宿主预留内存 MiB", exact: true })
    .fill("512");
  await panel.getByRole("button", { name: "应用", exact: true }).click();
  await expect(available).toContainText("3.5 GiB");
  await expect(host).toContainText("0.5 GiB");
  expect(requests).toEqual([{ cpuAllocationRatio: 4, memoryReserveMiB: 512 }]);
  for (const width of [1366, 390]) {
    await page.setViewportSize({ width, height: 900 });
    await expect
      .poll(() =>
        page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),
      )
      .toBe(true);
    await page.screenshot({
      path: `test-results/node-scheduling-${width}.png`,
    });
  }
  await panel.getByText("自动", { exact: true }).click();
  await panel.getByRole("button", { name: "应用", exact: true }).click();
  await expect(available).toContainText("3 GiB");
  await expect(host).toContainText("1 GiB");
  await expect(
    panel.getByRole("radio", { name: "自动", exact: true }),
  ).toBeChecked();
  expect(requests).toEqual([
    { cpuAllocationRatio: 4, memoryReserveMiB: 512 },
    { cpuAllocationRatio: 4, memoryReserveMiB: 0 },
  ]);
});
