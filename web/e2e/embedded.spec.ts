import { expect, test } from "@playwright/test";

test("同源嵌入复用工作区、权限与主题，所有请求沿上下文前缀", async ({
  page,
}) => {
  const prefix = "/api/teamlab/netlab/game/8";
  const seen: string[] = [];
  await page.route(`**${prefix}/environments/env`, async (route) => {
    const response = await route.fetch({ url: "http://127.0.0.1:5174/" });
    const html = (await response.text())
      .replace(
        'href="/" data-netlab-base',
        `href="${prefix}/" data-netlab-base`,
      )
      .replace(
        "<head>",
        `<head><script type="application/json" id="netlab-host">${JSON.stringify({ basePath: prefix, theme: "dark", environmentOnly: true })}</script>`,
      );
    await route.fulfill({ response, body: html });
  });
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname;
    seen.push(path);
    expect(path.startsWith(prefix)).toBe(true);
    const endpoint = path.slice((prefix + "/api/v1").length);
    const environment = {
      id: "env",
      name: "风电演练",
      status: "draft",
      revision: 1,
      projectId: "project",
      spec: { assets: [], networks: [] },
      permissions: ["read", "observe", "manage"],
    };
    if (endpoint === "/identity")
      return route.fulfill({
        json: {
          id: "service",
          name: "参赛队伍",
          administrator: false,
          grants: [],
        },
      });
    if (endpoint === "/environments/env")
      return route.fulfill({ json: environment });
    if (endpoint.endsWith("/events"))
      return route.fulfill({
        contentType: "text/event-stream",
        body: "event: runtime.changed\ndata: {}\n\n",
      });
    if (endpoint.endsWith("/state"))
      return route.fulfill({ json: { status: "draft", assets: [] } });
    if (endpoint.endsWith("/storage"))
      return route.fulfill({
        json: {
          bytes: 0,
          baseBytes: 0,
          locations: [],
          measuredAt: new Date().toISOString(),
        },
      });
    return route.fulfill({ json: [] });
  });
  await page.goto(prefix + "/environments/env");
  await expect(page.getByRole("heading", { name: "风电演练" })).toBeVisible();
  await expect(page.getByRole("button", { name: "共享环境" })).toHaveCount(0);
  await expect(page.getByRole("link", { name: "返回环境列表" })).toHaveCount(0);
  await expect(page.getByRole("navigation", { name: "主导航" })).toHaveCount(0);
  await expect(page.locator("html")).toHaveAttribute(
    "data-mantine-color-scheme",
    "dark",
  );
  await page.evaluate(() =>
    window.postMessage(
      { type: "netlab:theme", theme: "light" },
      window.location.origin,
    ),
  );
  await expect(page.locator("html")).toHaveAttribute(
    "data-mantine-color-scheme",
    "light",
  );
  await expect
    .poll(() => seen.some((path) => path.endsWith("/events")))
    .toBe(true);
  for (const width of [390, 1366, 1920, 2560]) {
    await page.setViewportSize({ width, height: 900 });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
  }
  await page.screenshot({
    path: test.info().outputPath("embedded-workbench.png"),
    animations: "disabled",
  });
});
