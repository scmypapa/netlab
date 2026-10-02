import { expect, test, type Page } from "@playwright/test";

// API fixtures verify browser interaction. Runtime acceptance uses the real node separately.
async function fixture(page: Page) {
  const calls: { method: string; path: string; body: unknown }[] = [];
  const spec = {
    networks: [
      {
        id: "lan",
        name: "应用网段",
        cidr: "10.10.0.0/24",
        gateway: "10.10.0.1",
      },
    ],
    assets: [
      {
        id: "web",
        name: "web-01",
        templateId: "linux",
        resources: { cpu: 2, memoryMiB: 2048, diskGiB: 20 },
        interfaces: [
          {
            id: "eth-web",
            networkId: "lan",
            address: "10.10.0.10",
            mac: "02:00:00:00:00:10",
            primary: true,
          },
        ],
      },
      {
        id: "windows",
        name: "windows-01",
        templateId: "win",
        resources: { cpu: 4, memoryMiB: 4096, diskGiB: 40 },
        interfaces: [
          {
            id: "eth-win",
            networkId: "lan",
            address: "10.10.0.11",
            mac: "02:00:00:00:00:11",
            primary: true,
          },
        ],
      },
    ],
  };
  const environment = {
    id: "env",
    projectId: "default",
    name: "混合环境",
    revision: 1,
    status: "running",
    spec,
    appliedSpec: spec,
    view: { positions: {} },
    draft: undefined as unknown,
    createdAt: "2026-10-02T08:00:00Z",
    updatedAt: "2026-10-02T08:10:00Z",
  };
  const templates = [
    {
      id: "linux",
      name: "Ubuntu Web",
      kind: "container",
      os: "Linux",
      version: 1,
      source: "docker.io/library/nginx:alpine",
      state: "ready",
      resources: { cpu: 2, memoryMiB: 2048, diskGiB: 20 },
    },
    {
      id: "win",
      name: "Windows Server",
      kind: "vm",
      os: "Windows",
      version: 1,
      source: "https://example.test/windows.qcow2",
      state: "ready",
      resources: { cpu: 4, memoryMiB: 4096, diskGiB: 40 },
      hardware: {
        firmware: "uefi",
        machine: "q35",
        nicModel: "virtio",
        diskBus: "virtio",
      },
    },
  ];
  const blueprints = [
    {
      id: "training",
      projectId: "default",
      name: "混合组网模板",
      latestVersionId: "training-v2",
      latestVersion: 2,
      assetCount: 2,
      networkCount: 1,
      createdAt: environment.createdAt,
      updatedAt: environment.updatedAt,
    },
  ];
  const versions = [1, 2].map((version) => ({
    id: `training-v${version}`,
    blueprintId: "training",
    version,
    assetCount: version,
    networkCount: 1,
    createdAt: environment.createdAt,
    spec: { ...spec, assets: spec.assets.slice(0, version) },
    view: { positions: {} },
    sourceEnvironmentId: "env",
    sourceRevision: 1,
  }));
  let createdEnvironment: typeof environment | undefined;
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname.replace("/api/v1", "");
    const body = request.postDataJSON();
    calls.push({ method: request.method(), path, body });
    let response: unknown;
    let status = 200;
    if (path === "/identity")
      response = { id: "user", name: "operator", administrator: true };
    else if (path === "/environments" && request.method() === "POST") {
      const version = versions.find(
        (item) => item.id === body.blueprintVersionId,
      );
      createdEnvironment = {
        ...environment,
        id: "env-created",
        name: body.name,
        status: body.run ? "deploying" : "draft",
        spec: version?.spec ?? body.spec,
        appliedSpec: version?.spec ?? body.spec,
      };
      response = createdEnvironment;
      status = 201;
    } else if (path === "/environments")
      response = [
        environment,
        ...(createdEnvironment ? [createdEnvironment] : []),
      ];
    else if (path === "/environments/env") response = environment;
    else if (path === "/environments/env-created")
      response = createdEnvironment;
    else if (path === "/environments/env-created/state")
      response = {
        id: "env-created",
        revision: 1,
        status: createdEnvironment!.status,
        assets: [],
        updatedAt: environment.updatedAt,
      };
    else if (path === "/environments/env/state")
      response = {
        id: "env",
        status: "running",
        revision: 1,
        assets: spec.assets.map((asset) => ({
          assetId: asset.id,
          instanceId: asset.id,
          nodeId: "node",
          state: "running",
          observedAt: environment.updatedAt,
        })),
        updatedAt: environment.updatedAt,
      };
    else if (path === "/templates") response = templates;
    else if (path === "/blueprints") response = blueprints;
    else if (
      path === "/blueprints/training/versions" &&
      request.method() === "POST"
    ) {
      const version = {
        ...versions[1],
        id: "training-v3",
        version: 3,
        spec: body.spec,
        assetCount: body.spec.assets.length,
        networkCount: body.spec.networks.length,
      };
      versions.push(version);
      Object.assign(blueprints[0], {
        latestVersionId: version.id,
        latestVersion: 3,
        assetCount: version.assetCount,
        networkCount: version.networkCount,
      });
      response = version;
      status = 201;
    } else if (path === "/blueprints/training/versions")
      response = versions.map(
        ({
          id,
          blueprintId,
          version,
          assetCount,
          networkCount,
          createdAt,
        }) => ({
          id,
          blueprintId,
          version,
          assetCount,
          networkCount,
          createdAt,
        }),
      );
    else if (path.startsWith("/blueprint-versions/"))
      response = versions.find((item) => item.id === path.split("/").at(-1));
    else if (path === "/environments/env/blueprints") {
      const blueprint = {
        ...blueprints[0],
        id: "saved",
        name: body.name,
        latestVersionId: "saved-v1",
        latestVersion: 1,
        assetCount: body.spec.assets.length,
        networkCount: body.spec.networks.length,
      };
      blueprints.push(blueprint);
      response = blueprint;
      status = 201;
    } else if (path === "/nodes")
      response = [
        {
          id: "node",
          name: "计算节点",
          endpoint: "https://node.test:9443",
          capacity: { cpu: 16, memoryMiB: 32768, diskGiB: 500 },
          reserved: { cpu: 6, memoryMiB: 6144, diskGiB: 60 },
          capabilities: ["container", "vm"],
          slots: 4,
          observedAt: environment.updatedAt,
          state: "online",
        },
      ];
    else if (path === "/operations") response = [];
    else if (path.endsWith("/view")) {
      environment.view = body;
      status = 204;
    } else if (path.endsWith("/draft")) {
      environment.draft = request.method() === "DELETE" ? undefined : body;
      status = 204;
    } else if (path.endsWith("/changes") && !body.apply)
      response = {
        revision: 1,
        changes: [
          {
            id: "windows",
            name: "windows-01",
            kind: "asset",
            effect: "remove",
            requiresStop: true,
            dataEffect: "删除系统盘",
          },
        ],
      };
    else if (path.endsWith("/changes") && body.apply) {
      status = 202;
      response = {
        id: "task",
        environmentId: "env",
        kind: "changes",
        state: "queued",
        phase: "queued",
        completed: 0,
        total: 1,
        createdAt: environment.updatedAt,
        updatedAt: environment.updatedAt,
      };
    } else {
      status = 404;
      response = { status, title: "Not found", detail: path };
    }
    await route.fulfill({
      status,
      contentType: "application/json",
      body: status === 204 ? undefined : JSON.stringify(response),
    });
  });
  return calls;
}

test("draft persists across exit; applying removal uses preview revision", async ({
  page,
}) => {
  const calls = await fixture(page);
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await page.getByRole("button", { name: "对象列表", exact: true }).click();
  await page.getByRole("button", { name: "windows-01", exact: true }).click();
  await page.getByRole("button", { name: "对象操作", exact: true }).click();
  await page.getByRole("menuitem", { name: "移除资产", exact: true }).click();
  await page.getByRole("button", { name: "保存草稿", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "继续调整", exact: true }),
  ).toBeVisible();
  expect(
    calls.find((call) => call.path.endsWith("/draft"))?.body,
  ).toMatchObject({ baseRevision: 1, spec: { assets: [{ id: "web" }] } });
  await page.getByRole("button", { name: "继续调整", exact: true }).click();
  await page.getByRole("button", { name: "预览变更", exact: true }).click();
  await expect(page.getByText("需要停机")).toBeVisible();
  await page.getByRole("button", { name: "应用变更", exact: true }).click();
  expect(
    calls
      .filter((call) => call.path.endsWith("/changes"))
      .map((call) => call.body),
  ).toEqual([
    expect.objectContaining({ apply: false, expectedRevision: 1 }),
    expect.objectContaining({ apply: true, expectedRevision: 1 }),
  ]);
});

test("canvas positioning only writes view", async ({ page }) => {
  const calls = await fixture(page);
  await page.goto("/environments/env");
  const node = page.locator(".react-flow__node").filter({ hasText: "web-01" });
  await expect(node).toBeVisible();
  const bounds = (await node.boundingBox())!;
  await page.mouse.move(bounds.x + 35, bounds.y + 35);
  await page.mouse.down();
  await page.mouse.move(bounds.x + 80, bounds.y + 55, { steps: 8 });
  await page.mouse.up();
  await expect
    .poll(() => calls.some((call) => call.path.endsWith("/view")))
    .toBe(true);
  expect(
    calls
      .filter((call) => ["PUT", "POST", "DELETE"].includes(call.method))
      .map((call) => call.path),
  ).toEqual(["/environments/env/view"]);
});

test("working area fits supported widths and both themes", async ({ page }) => {
  await fixture(page);
  for (const width of [390, 1366, 1920, 2560]) {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/environments/env");
    await expect(page.getByRole("heading", { name: "混合环境" })).toBeVisible();
    await page.getByRole("button", { name: "对象列表", exact: true }).click();
    await page.getByRole("button", { name: "windows-01", exact: true }).click();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    await page.screenshot({ path: `test-results/workbench-${width}.png` });
  }
  await page.getByRole("button", { name: "切换夜间主题" }).click();
  await expect(page.locator("html")).toHaveAttribute(
    "data-mantine-color-scheme",
    "dark",
  );
  await page.screenshot({ path: "test-results/workbench-dark.png" });
});

test("saving an edited environment creates a template or appends a version without applying the draft", async ({
  page,
}) => {
  const calls = await fixture(page);
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await page.getByRole("button", { name: "对象列表", exact: true }).click();
  await page.getByRole("button", { name: "windows-01", exact: true }).click();
  await page.getByRole("button", { name: "对象操作", exact: true }).click();
  await page.getByRole("menuitem", { name: "移除资产", exact: true }).click();
  await page.getByRole("button", { name: "草稿操作", exact: true }).click();
  await page
    .getByRole("menuitem", { name: "保存为环境模板", exact: true })
    .click();
  await page.getByRole("textbox", { name: "模板名称" }).fill("Web 环境模板");
  await page.getByRole("button", { name: "保存模板", exact: true }).click();
  await expect(page.getByText("v1 已保存", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "完成", exact: true }).click();
  await expect(page.getByText("调整中", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "草稿操作", exact: true }).click();
  await page
    .getByRole("menuitem", { name: "保存为环境模板", exact: true })
    .click();
  await page
    .getByRole("button", { name: "现有模板新版本", exact: true })
    .click();
  await page.getByRole("textbox", { name: "环境模板" }).click();
  await page
    .getByRole("option", { name: "混合组网模板 · v2", exact: true })
    .click();
  await page.getByRole("button", { name: "保存模板", exact: true }).click();
  await expect(page.getByText("v3 已保存", { exact: true })).toBeVisible();
  const writes = calls.filter((call) => call.method === "POST");
  expect(writes).toEqual([
    {
      method: "POST",
      path: "/environments/env/blueprints",
      body: expect.objectContaining({
        name: "Web 环境模板",
        expectedRevision: 1,
        spec: expect.objectContaining({
          assets: [expect.objectContaining({ id: "web" })],
        }),
      }),
    },
    {
      method: "POST",
      path: "/blueprints/training/versions",
      body: expect.objectContaining({
        environmentId: "env",
        expectedRevision: 1,
        spec: expect.objectContaining({
          assets: [expect.objectContaining({ id: "web" })],
        }),
      }),
    },
  ]);
});

test("template details create and run the selected immutable version", async ({
  page,
}) => {
  const calls = await fixture(page);
  await page.goto("/templates?tab=environments");
  await page.getByRole("button", { name: "混合组网模板", exact: true }).click();
  await page.getByRole("textbox", { name: "版本", exact: true }).click();
  await page.getByRole("option", { name: /^v1 ·/ }).click();
  await expect(page.locator(".blueprint-objects")).toContainText("web-01");
  await expect(page.locator(".blueprint-objects")).not.toContainText(
    "windows-01",
  );
  for (const width of [390, 1366]) {
    await page.setViewportSize({ width, height: 900 });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    await page.screenshot({ path: `test-results/blueprint-${width}.png` });
  }
  await page.getByRole("button", { name: "创建环境", exact: true }).click();
  await page.getByRole("textbox", { name: "环境名称" }).fill("独立 Web 环境");
  await page.getByRole("button", { name: "创建并运行", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "独立 Web 环境", exact: true }),
  ).toBeVisible();
  const create = calls.find(
    (call) => call.path === "/environments" && call.method === "POST",
  );
  expect(create?.body).toEqual({
    name: "独立 Web 环境",
    blueprintVersionId: "training-v1",
    run: true,
    clientRequestId: expect.any(String),
  });
  expect(
    calls.some((call) => call.path === "/blueprint-versions/training-v1"),
  ).toBe(true);
  expect(calls.some((call) => call.path.endsWith("/actions"))).toBe(false);
});

test("the shared creation dialog also creates an empty environment", async ({
  page,
}) => {
  const calls = await fixture(page);
  await page.goto("/environments");
  await page.getByRole("button", { name: "新建环境", exact: true }).click();
  await page.getByRole("textbox", { name: "环境名称" }).fill("空白实验环境");
  await page.getByRole("button", { name: "创建环境", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "空白实验环境", exact: true }),
  ).toBeVisible();
  expect(calls.find((call) => call.method === "POST")?.body).toEqual({
    name: "空白实验环境",
    spec: { assets: [], networks: [] },
  });
});

test("the environment list selects a template without starting it", async ({
  page,
}) => {
  const calls = await fixture(page);
  await page.setViewportSize({ width: 390, height: 900 });
  await page.goto("/environments");
  await page.getByRole("button", { name: "新建环境", exact: true }).click();
  await page.getByRole("textbox", { name: "环境模板", exact: true }).click();
  await page.getByRole("option", { name: "混合组网模板", exact: true }).click();
  await page.getByLabel("创建后运行", { exact: true }).uncheck();
  await page.getByRole("textbox", { name: "环境名称" }).fill("待运行模板环境");
  await page.screenshot({ path: "test-results/blueprint-create-390.png" });
  await page.getByRole("button", { name: "创建环境", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "待运行模板环境", exact: true }),
  ).toBeVisible();
  expect(calls.find((call) => call.method === "POST")?.body).toEqual({
    name: "待运行模板环境",
    blueprintVersionId: "training-v2",
    run: false,
  });
});
