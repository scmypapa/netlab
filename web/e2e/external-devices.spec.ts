import { expect, test } from "@playwright/test";

test("外部设备发现后加入画布、编辑和移除，不创建运行实例", async ({ page }) => {
  const network = {
    id: "field",
    name: "工控网络",
    cidr: "192.0.2.0/24",
    external: { nodeId: "node", interface: "eth1", vlan: 123 },
    allocationPool: "192.0.2.128/26",
  };
  const calls: { path: string; method: string; body: any }[] = [];
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request(),
      path = new URL(request.url()).pathname.replace("/api/v1", ""),
      body = request.postDataJSON();
    calls.push({ path, method: request.method(), body });
    let response: unknown = [];
    if (path === "/identity")
      response = { id: "admin", name: "admin", administrator: true };
    else if (path === "/environments/env")
      response = {
        id: "env",
        projectId: "default",
        name: "现场联调",
        revision: 1,
        status: "running",
        permissions: ["read", "compose", "operate", "manage"],
        spec: { assets: [], networks: [] },
        appliedSpec: { assets: [], networks: [] },
        view: {},
        createdAt: "2026-10-25T00:00:00Z",
        updatedAt: "2026-10-25T00:00:00Z",
      };
    else if (path.endsWith("/state"))
      response = {
        id: "env",
        revision: 1,
        status: "running",
        assets: [],
        updatedAt: "2026-10-25T00:00:00Z",
      };
    else if (path === "/nodes")
      response = [{ id: "node", name: "现场节点", state: "ready" }];
    else if (path === "/nodes/node/interfaces")
      response = [
        {
          name: "eth1",
          kind: "ethernet",
          addresses: [],
          available: true,
          linkUp: true,
        },
      ];
    else if (path.endsWith("/events"))
      return route.fulfill({
        contentType: "text/event-stream",
        body: ": connected\n\n",
      });
    else if (path.endsWith("/external-devices"))
      response = [
        {
          address: "192.0.2.20",
          mac: "02:00:00:00:00:20",
          observedAt: "2026-10-25T00:00:00Z",
        },
      ];
    else if (path.endsWith("/draft")) return route.fulfill({ status: 204 });
    await route.fulfill({ json: response });
  });
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await page.getByRole("button", { name: "添加", exact: true }).click();
  await page.getByRole("menuitem", { name: "外部设备", exact: true }).click();
  const drawer = page.getByRole("dialog", { name: "添加外部设备" });
  await drawer
    .getByRole("button", { name: "接入现场网络", exact: true })
    .click();
  const setup = page.getByRole("dialog", { name: "接入现场网络" });
  await setup
    .getByRole("textbox", { name: "网络名称", exact: true })
    .fill("工控网络");
  await expect(
    setup.getByRole("textbox", { name: "接入节点", exact: true }),
  ).toHaveValue("现场节点");
  await setup.getByRole("textbox", { name: "接入网口", exact: true }).click();
  await page
    .getByRole("option", { name: "以太网口 · eth1", exact: true })
    .click();
  await setup
    .getByRole("textbox", { name: "现场网段", exact: true })
    .fill(network.cidr);
  await setup
    .getByRole("textbox", { name: "虚拟资产预留地址段", exact: true })
    .fill(network.allocationPool);
  await setup.getByRole("button", { name: "网络参数", exact: true }).click();
  await setup.getByRole("textbox", { name: "VLAN", exact: true }).fill("123");
  await setup.getByRole("button", { name: "添加到环境", exact: true }).click();
  await drawer.getByRole("button", { name: "发现设备", exact: true }).click();
  await expect(drawer).toContainText("192.0.2.20");
  expect(
    calls.find((call) => call.path.endsWith("/external-devices")),
  ).toMatchObject({
    method: "POST",
    body: { interface: "eth1", vlan: 123, cidr: "192.0.2.0/24" },
  });
  await drawer.getByRole("checkbox", { name: "选择 192.0.2.20" }).check();
  await drawer
    .getByRole("textbox", { name: "192.0.2.20 的设备名称" })
    .fill("PLC-01");
  for (const width of [390, 1366, 1920, 2560]) {
    await page.setViewportSize({ width, height: 900 });
    expect(
      await drawer.evaluate((element) => element.scrollWidth),
    ).toBeLessThanOrEqual(width);
    await page.screenshot({
      path: test.info().outputPath(`discovery-${width}.png`),
    });
  }
  await drawer.getByRole("button", { name: "加入画布 · 1" }).click();
  await expect(
    page.locator(".react-flow__node").filter({ hasText: "PLC-01" }),
  ).toBeVisible();
  const segment = await page.locator(".react-flow__node-network").boundingBox();
  const physical = await page.locator(".react-flow__node-asset").boundingBox();
  expect(segment).not.toBeNull();
  expect(physical).not.toBeNull();
  expect(physical!.x).toBeGreaterThanOrEqual(segment!.x);
  expect(physical!.y).toBeGreaterThanOrEqual(segment!.y);
  expect(physical!.x + physical!.width).toBeLessThanOrEqual(
    segment!.x + segment!.width,
  );
  expect(physical!.y + physical!.height).toBeLessThanOrEqual(
    segment!.y + segment!.height,
  );
  await page.screenshot({
    path: test.info().outputPath("physical-device-topology.png"),
  });
  await page.getByRole("button", { name: "对象列表", exact: true }).click();
  await page.getByRole("button", { name: "PLC-01", exact: true }).click();
  const inspector = page.getByRole("complementary", { name: "对象详情" });
  await expect(inspector).toContainText("192.0.2.20");
  await expect(
    inspector.getByRole("button", { name: "启动", exact: true }),
  ).toHaveCount(0);
  await inspector.getByRole("button", { name: "编辑", exact: true }).click();
  const edit = page.getByRole("dialog", { name: "编辑外部设备" });
  await expect(edit.getByRole("textbox", { name: /IP/ })).toHaveCount(0);
  await edit.getByRole("textbox", { name: "设备名称" }).fill("PLC-产线一");
  await edit.getByRole("button", { name: "保存设备", exact: true }).click();
  await page.getByRole("button", { name: "PLC-产线一", exact: true }).click();
  await inspector
    .getByRole("button", { name: "对象操作", exact: true })
    .click();
  await page.getByRole("menuitem", { name: "移除设备", exact: true }).click();
  await expect(
    page.locator(".react-flow__node").filter({ hasText: "PLC-产线一" }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "保存草稿", exact: true }).click();
  expect(
    calls.find((call) => call.path.endsWith("/draft"))?.body.spec,
  ).toMatchObject({
    assets: [],
    networks: [{ ...network, id: expect.any(String), devices: [] }],
  });
  expect(calls.some((call) => call.path.endsWith("/actions"))).toBe(false);
});
