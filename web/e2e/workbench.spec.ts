import { expect, test, type Page } from "@playwright/test";

test("通信图与抓包贯通，角色仅更新画布，分段失败可见", async ({ page }) => {
  const calls = await fixture(page);
  const capture = "cd3aa501-fda4-4b61-9d68-7337f896ad80";
  const now = new Date().toISOString();
  let active = false;
  let status: "running" | "stopped" = "running";
  const segment = () => ({
    id: capture,
    nodeId: "one",
    nodeName: "节点一",
    environmentId: "env",
    assetIds: ["web"],
    status,
    startedAt: now,
    bytes: 1048576,
    packets: 1000,
    omittedFlows: 0,
  });
  const failed = {
    ...segment(),
    nodeId: "two",
    nodeName: "节点二",
    assetIds: ["windows"],
    status: "failed",
    bytes: 0,
    packets: 0,
    error: "抓包进程未能启动",
  };
  const flows = [
    {
      source: "10.10.0.10",
      destination: "10.10.0.11",
      sourceAssetId: "web",
      destinationAssetId: "windows",
      protocol: "UDP",
      sourcePort: 50000,
      destinationPort: 9000,
      bytes: 1048576,
      bytesPerSecond: 16384,
      packets: 1000,
      firstSeen: now,
      lastSeen: now,
    },
  ];
  await page.route("**/api/v1/environments/env/captures**", (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path.endsWith("/captures")) {
      if (request.method() === "POST") {
        active = true;
        expect(request.postDataJSON().assetIds).toEqual(["web", "windows"]);
      }
      return route.fulfill({
        json: { segments: active ? [segment(), failed] : [], errors: {} },
      });
    }
    if (request.method() === "POST") {
      status = "stopped";
      return route.fulfill({ json: segment() });
    }
    return route.fulfill({ json: { segment: segment(), flows } });
  });
  let sampled = false;
  await page.route("**/api/v1/environments/env/traffic", (route) => {
    sampled = true;
    return route.fulfill({
      json: {
        flows,
        errors: {},
        samplingRate: 512,
        windowSeconds: 60,
        omittedSamples: 0,
        observedAt: now,
      },
    });
  });
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "流量视图" }).click();
  await expect.poll(() => sampled).toBe(true);
  await expect(page.getByText("采样估算 1/512", { exact: true })).toBeVisible();
  await expect(page.getByRole("img", { name: /资产通信图/ })).toBeVisible();
  await page
    .getByRole("button", { name: "开始抓包", exact: true })
    .first()
    .click();
  await page
    .getByRole("dialog", { name: "开始抓包" })
    .getByRole("button", { name: "开始", exact: true })
    .click();
  await expect(page.getByRole("img", { name: /资产通信图/ })).toBeVisible();
  await expect(page.getByText("抓包进程未能启动")).toBeVisible();
  await expect(page.getByText("抓包实测", { exact: true })).toBeVisible();
  await expect(page.getByText("1.0 MiB · 16.0 KiB/s")).toBeVisible();
  await page.getByRole("button", { name: /web-01.*windows-01/ }).click();
  await page.getByRole("button", { name: "查看 web-01", exact: true }).click();
  await page.getByRole("textbox", { name: "角色", exact: true }).fill("客户端");
  await page.getByRole("textbox", { name: "角色", exact: true }).press("Tab");
  await expect
    .poll(() =>
      calls.some(
        (call) =>
          call.path.endsWith("/view") &&
          (call.body as { roles?: Record<string, string> })?.roles?.web ===
            "客户端",
      ),
    )
    .toBe(true);
  expect(calls.some((call) => call.path.endsWith("/changes"))).toBe(false);
  await page.getByRole("button", { name: "停止此分段" }).click();
  await expect(
    page.getByRole("link", { name: "下载抓包文件" }).first(),
  ).toHaveAttribute(
    "href",
    "/api/v1/environments/env/captures/one/" + capture + "/file",
  );
  for (const width of [390, 1366]) {
    await page.setViewportSize({ width, height: 900 });
    await page.screenshot({ path: "../data/traffic-" + width + ".png" });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
  }
});
import type { Identity, Operation } from "../src/api/client";

test("系统更新展示正式发布日志并提交指定版本", async ({ page }) => {
  await fixture(page);
  const now = new Date().toISOString();
  let state = {
    currentVersion: "v1.0.0",
    available: true,
    canApply: true,
    checkedAt: now,
    latest: {
      version: "v1.1.0",
      name: "Netlab v1.1.0",
      notes: "## 网络\n\n- 改进跨节点连接\n\n## 运维\n\n- 完善资源曲线",
      publishedAt: now,
      url: "https://github.com/scmypapa/netlab/releases/tag/v1.1.0",
    },
    activity: undefined as
      { version: string; phase: string; updatedAt: string } | undefined,
  };
  const submitted: unknown[] = [];
  await page.route("**/api/v1/system/update**", (route) => {
    if (
      route.request().method() === "POST" &&
      !route.request().url().endsWith("/check")
    ) {
      submitted.push(route.request().postDataJSON());
      state = {
        ...state,
        activity: { version: "v1.1.0", phase: "queued", updatedAt: now },
      };
      return route.fulfill({ status: 202, json: state });
    }
    return route.fulfill({ json: state });
  });
  await page.goto("/environments/env");
  await page.getByRole("button", { name: /operator/ }).click();
  await page.getByRole("menuitem", { name: "系统更新" }).click();
  const panel = page.getByRole("dialog", { name: "系统更新", exact: true });
  await expect(panel.getByText("v1.0.0", { exact: true })).toBeVisible();
  await expect(
    panel.getByRole("heading", { name: "网络", exact: true }),
  ).toBeVisible();
  await expect(
    panel.getByText("改进跨节点连接", { exact: true }),
  ).toBeVisible();
  await panel.getByRole("button", { name: "检查更新" }).click();
  await panel.getByRole("button", { name: "更新到 v1.1.0" }).click();
  await page.getByRole("button", { name: "开始更新", exact: true }).click();
  await expect(panel.getByText("准备更新", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("dialog", { name: "更新到 v1.1.0", exact: true }),
  ).toHaveCount(0);
  expect(submitted).toEqual([{ version: "v1.1.0" }]);
  for (const width of [390, 1366]) {
    await page.setViewportSize({ width, height: 900 });
    await page.screenshot({ path: "../data/update-" + width + ".png" });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
  }
});

test("资产资源曲线按对象查询，时间切换和窄屏布局贯通", async ({ page }) => {
  await fixture(page);
  const requests: URL[] = [];
  await page.route("**/api/v1/environments/env/metrics**", (route) => {
    const url = new URL(route.request().url());
    requests.push(url);
    const end = new Date();
    const start = new Date(
      end.getTime() - Number(url.searchParams.get("range")) * 1000,
    );
    return route.fulfill({
      json: {
        start: start.toISOString(),
        end: end.toISOString(),
        stepSeconds: 10,
        series: [
          "cpu",
          "memory",
          "receive",
          "transmit",
          "disk_read",
          "disk_write",
          "receive_packets",
          "transmit_packets",
          "receive_drops",
          "transmit_drops",
        ].map((metric) => ({
          metric,
          assetId: "windows",
          instanceId: "instance",
          nodeId: "node",
          ...(metric.startsWith("receive") || metric.startsWith("transmit")
            ? { interfaceId: "eth-win" }
            : {}),
          points: [30, 20, 10].map((offset) => ({
            time: new Date(end.getTime() - offset * 1000).toISOString(),
            value: metric.endsWith("drops") ? 0 : 128,
          })),
        })),
      },
    });
  });
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "资产视图", exact: true }).click();
  await page.getByRole("button", { name: "windows-01", exact: true }).click();
  await page.getByRole("button", { name: "对象操作", exact: true }).click();
  await page.getByRole("menuitem", { name: "资源曲线", exact: true }).click();
  await expect(page.getByRole("img", { name: /历史曲线/ })).toHaveCount(4);
  await expect(
    page
      .getByRole("table")
      .filter({ has: page.getByText("接口速率", { exact: true }) }),
  ).toContainText("应用网段 · 网卡1");
  expect(requests[0].searchParams.get("assetId")).toBe("windows");
  await page.getByText("1 小时", { exact: true }).click();
  await expect
    .poll(() => requests.at(-1)?.searchParams.get("range"))
    .toBe("3600");
  for (const width of [390, 1366, 1920, 2560]) {
    await page.setViewportSize({ width, height: 900 });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
  }
  await page.getByRole("button", { name: "切换夜间主题" }).click();
  await expect(page.getByRole("img", { name: /历史曲线/ })).toHaveCount(4);
  await page.getByRole("button", { name: "返回拓扑", exact: true }).click();
  await expect(page.getByLabel("资源观察")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "资产视图", exact: true }),
  ).toBeVisible();
});

// API fixtures verify browser interaction. Runtime acceptance uses the real node separately.
async function fixture(
  page: Page,
  options: {
    permissions?: string[];
    assetPermissions?: Record<string, string[]>;
    identity?: Identity;
    operation?: Operation;
    status?: string;
  } = {},
) {
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
    projectId: "lab-project",
    name: "混合环境",
    revision: 1,
    status: options.status ?? "running",
    permissions: options.permissions ?? [
      "read",
      "operate",
      "compose",
      "manage",
      "session",
      "file",
      "observe",
      "network",
      "access",
    ],
    assetPermissions: options.assetPermissions,
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
      initialization: "cloudbase-init",
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
  if (options.identity && !options.identity.administrator)
    for (const template of templates) template.source = "";
  const blueprints = [
    {
      id: "training",
      projectId: "lab-project",
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
    if (path.endsWith("/events"))
      return route.fulfill({
        contentType: "text/event-stream",
        body: ": connected\n\n",
      });
    let response: unknown;
    let status = 200;
    if (path === "/identity")
      response = options.identity ?? {
        id: "user",
        name: "operator",
        administrator: true,
      };
    else if (path === "/environments" && request.method() === "POST") {
      const version = versions.find(
        (item) => item.id === body.blueprintVersionId,
      );
      createdEnvironment = {
        ...environment,
        id: "env-created",
        projectId: body.projectId ?? "default",
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
      ].map(({ spec, appliedSpec, view, draft, ...item }) => ({
        ...item,
        assetCount: (appliedSpec ?? spec).assets.length,
        networkCount: (appliedSpec ?? spec).networks.length,
      }));
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
        status: environment.status,
        revision: 1,
        operation: options.operation,
        assets: spec.assets.map((asset) => ({
          assetId: asset.id,
          instanceId: asset.id,
          nodeId: "node",
          state: environment.status,
          observedAt: environment.updatedAt,
        })),
        updatedAt: environment.updatedAt,
      };
    else if (path.endsWith("/services")) response = [];
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
          state: "ready",
        },
      ];
    else if (path === "/nodes/node/interfaces")
      response = [
        {
          name: "eth0",
          kind: "ethernet",
          mac: "02:00:00:00:00:01",
          mtu: 1500,
          addresses: ["192.0.2.10/24"],
          available: false,
        },
        {
          name: "br-lan",
          kind: "linux-bridge",
          mac: "02:00:00:00:00:02",
          mtu: 1500,
          addresses: ["192.0.2.254/24"],
          available: true,
        },
      ];
    else if (path.endsWith("/retry")) {
      options.operation = {
        ...options.operation!,
        state: "queued",
        retryable: false,
      };
      response = options.operation;
      status = 202;
    } else if (path === "/operations")
      response = options.operation ? [options.operation] : [];
    else if (path === "/environments/env/actions") {
      response = {
        id: "environment-action",
        environmentId: "env",
        kind: body.action,
        state: "queued",
        phase: "queued",
        completed: 0,
        total: 2,
        retryable: false,
        createdAt: environment.updatedAt,
        updatedAt: environment.updatedAt,
      };
      status = 202;
    } else if (
      path.startsWith("/environments/env/assets/") &&
      path.endsWith("/actions")
    ) {
      status = 202;
      response = {
        id: "asset-action",
        environmentId: "env",
        kind: body.action,
        state: "queued",
        phase: "queued",
        completed: 0,
        total: 1,
        retryable: false,
        createdAt: environment.updatedAt,
        updatedAt: environment.updatedAt,
      };
    } else if (path.endsWith("/view")) {
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

test("恢复点：只读查看、捕获、恢复与删除请求、真实错误呈现", async ({
  page,
}) => {
  const options = { permissions: ["read"] };
  await fixture(page, options);
  let created = false,
    rejectCapture = true,
    restored = false;
  const point = {
    id: "point",
    environmentId: "env",
    name: "调整前",
    revision: 1,
    state: "ready",
    assetCount: 2,
    memoryAssetCount: 1,
    consistency: "crash",
    sizeBytes: 64 * 2 ** 20,
    createdAt: "2026-10-03T08:00:00Z",
  };
  await page.route(
    "**/api/v1/environments/env/recovery-points**",
    async (route) => {
      const request = route.request();
      let status = 200,
        response: unknown = created ? [point] : [];
      if (request.url().endsWith("/restore")) {
        expect(request.postDataJSON()).toEqual({ expectedRevision: 1 });
        restored = true;
        status = 202;
        response = { id: "restore" };
      } else if (request.method() === "POST") {
        expect(request.postDataJSON()).toEqual({
          name: "调整前",
          expectedRevision: 1,
          includeMemory: true,
        });
        if (rejectCapture) {
          status = 409;
          response = { detail: "请等待当前任务完成" };
        } else {
          status = 201;
          created = true;
          response = point;
        }
      } else if (request.method() === "DELETE") {
        expect(new URL(request.url()).pathname).toBe(
          "/api/v1/environments/env/recovery-points/point",
        );
        status = 202;
        created = false;
        response = { id: "delete" };
      }
      await route.fulfill({
        status,
        contentType: "application/json",
        body: JSON.stringify(response),
      });
    },
  );
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "环境操作" }).click();
  await page.getByRole("menuitem", { name: "恢复点", exact: true }).click();
  await expect(page.getByText("暂无恢复点", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "创建恢复点" })).toHaveCount(0);
  options.permissions.push("manage");
  await page.reload();
  await page.getByRole("button", { name: "环境操作" }).click();
  await page.getByRole("menuitem", { name: "恢复点", exact: true }).click();
  await page.getByRole("button", { name: "创建恢复点", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "创建恢复点" });
  await dialog.getByRole("textbox", { name: "名称" }).fill("调整前");
  await dialog.getByRole("checkbox", { name: "保存虚拟机内存" }).check();
  await dialog.getByRole("button", { name: "开始捕获" }).click();
  await expect(dialog.getByText("请等待当前任务完成")).toBeVisible();
  rejectCapture = false;
  await dialog.getByRole("button", { name: "开始捕获" }).click();
  await expect(page.getByRole("heading", { name: "调整前" })).toBeVisible();
  await expect(page.getByText("含 1 台虚拟机内存")).toBeVisible();
  await expect(page.getByText("崩溃一致", { exact: true })).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  await page.screenshot({ path: "../data/recovery-drawer-mobile.png" });
  await page.getByRole("button", { name: "调整前操作" }).click();
  await page.getByRole("menuitem", { name: "恢复", exact: true }).click();
  const restore = page.getByRole("dialog", { name: "恢复环境", exact: true });
  await expect(restore).toContainText("当前资产配置与数据将被替换");
  await restore.getByRole("button", { name: "恢复", exact: true }).click();
  await expect(restore).toHaveCount(0);
  expect(restored).toBe(true);
  await page.getByRole("button", { name: "调整前操作" }).click();
  await page.getByRole("menuitem", { name: "删除", exact: true }).click();
  await page
    .getByRole("dialog", { name: "删除恢复点" })
    .getByRole("button", { name: "删除", exact: true })
    .click();
  await expect(page.getByText("暂无恢复点", { exact: true })).toBeVisible();
});

test("恢复点克隆通过创建接口打开独立环境", async ({ page }) => {
  await fixture(page, {
    identity: {
      id: "administrator",
      name: "admin",
      administrator: true,
      grants: [],
    },
  });
  await page.route("**/api/v1/environments/env/recovery-points**", (route) =>
    route.fulfill({
      json: [
        {
          id: "point",
          environmentId: "env",
          name: "完整环境",
          revision: 1,
          state: "ready",
          assetCount: 2,
          sizeBytes: 64 * 2 ** 20,
          createdAt: "2026-10-03T08:00:00Z",
        },
      ],
    }),
  );
  let submitted: unknown;
  await page.route("**/api/v1/environments", (route) => {
    submitted = route.request().postDataJSON();
    return route.fulfill({ status: 201, json: { id: "clone" } });
  });
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "环境操作" }).click();
  await page.getByRole("menuitem", { name: "恢复点", exact: true }).click();
  await page.getByRole("button", { name: "完整环境操作" }).click();
  await page.getByRole("menuitem", { name: "克隆为新环境" }).click();
  const dialog = page.getByRole("dialog", {
    name: "克隆为新环境",
    exact: true,
  });
  await expect(
    dialog.getByRole("checkbox", { name: "创建后启动" }),
  ).not.toBeChecked();
  await dialog.getByRole("textbox", { name: "环境名称" }).fill("环境副本");
  await dialog.getByRole("checkbox", { name: "创建后启动" }).check();
  await dialog.getByRole("button", { name: "创建环境", exact: true }).click();
  await expect(page).toHaveURL(/\/environments\/clone$/);
  expect(submitted).toEqual({
    name: "环境副本",
    projectId: "lab-project",
    recoveryPointId: "point",
    run: true,
  });
});

test("备份复用恢复工作区，提交、恢复和克隆请求贯通", async ({ page }) => {
  await fixture(page, {
    identity: {
      id: "administrator",
      name: "admin",
      administrator: true,
      grants: [],
    },
  });
  const point = {
    id: "point",
    environmentId: "env",
    name: "调整前",
    revision: 1,
    state: "ready",
    assetCount: 2,
    sizeBytes: 1024,
    createdAt: "2026-10-03T08:00:00Z",
  };
  const backup = {
    id: "backup",
    environmentId: "env",
    repositoryId: "repository",
    name: "独立备份",
    state: "ready",
    sizeBytes: 1024,
    createdAt: point.createdAt,
  };
  let saved = false,
    restored = false,
    cloned: unknown;
  await page.route("**/api/v1/environments/env/recovery-points**", (route) =>
    route.fulfill({ json: [point] }),
  );
  await page.route("**/api/v1/backup-repositories", (route) =>
    route.fulfill({
      json: [
        { id: "repository", name: "存储仓库", nodeId: "node", state: "ready" },
      ],
    }),
  );
  await page.route("**/api/v1/environments/env/backups**", async (route) => {
    const request = route.request();
    if (request.url().endsWith("/restore")) {
      expect(request.postDataJSON()).toEqual({ expectedRevision: 1 });
      restored = true;
      await route.fulfill({ status: 202, json: { id: "restore" } });
    } else if (request.method() === "POST") {
      expect(request.postDataJSON()).toEqual({
        name: "独立备份",
        repositoryId: "repository",
        recoveryPointId: "point",
      });
      saved = true;
      await route.fulfill({ status: 201, json: backup });
    } else await route.fulfill({ json: saved ? [backup] : [] });
  });
  await page.route("**/api/v1/environments", (route) => {
    cloned = route.request().postDataJSON();
    return route.fulfill({ status: 201, json: { id: "clone" } });
  });
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "环境操作" }).click();
  await page.getByRole("menuitem", { name: "恢复点", exact: true }).click();
  await page.getByRole("button", { name: "调整前操作" }).click();
  await page.getByRole("menuitem", { name: "保存为备份" }).click();
  const save = page.getByRole("dialog", { name: "保存为备份", exact: true });
  await save.getByRole("textbox", { name: "名称" }).fill("独立备份");
  await save.getByRole("textbox", { name: "备份仓库" }).click();
  await page.getByRole("option", { name: "存储仓库", exact: true }).click();
  await save.getByRole("button", { name: "备份", exact: true }).click();
  await expect(page.getByRole("heading", { name: "独立备份" })).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  await page.getByRole("button", { name: "独立备份操作" }).click();
  await page.getByRole("menuitem", { name: "恢复", exact: true }).click();
  await page
    .getByRole("dialog", { name: "恢复环境" })
    .getByRole("button", { name: "恢复", exact: true })
    .click();
  await expect.poll(() => restored).toBe(true);
  await page.getByRole("button", { name: "独立备份操作" }).click();
  await page.getByRole("menuitem", { name: "克隆为新环境" }).click();
  const clone = page.getByRole("dialog", { name: "克隆为新环境", exact: true });
  await clone.getByRole("textbox", { name: "环境名称" }).fill("备份副本");
  await clone.getByRole("button", { name: "创建环境", exact: true }).click();
  await expect(page).toHaveURL(/\/environments\/clone$/);
  expect(cloned).toEqual({
    name: "备份副本",
    projectId: "lab-project",
    backupId: "backup",
    run: false,
  });
});

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

test("asset creation carries template resources and optional guest configuration", async ({
  page,
}) => {
  const calls = await fixture(page);
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await page.getByRole("button", { name: "添加", exact: true }).click();
  await page.getByRole("menuitem", { name: "资产", exact: true }).click();
  const drawer = page.getByRole("dialog");
  await expect(
    drawer.getByRole("button", { name: "来宾设置", exact: true }),
  ).toHaveCount(0);
  await drawer.getByRole("textbox", { name: "资产模板", exact: true }).click();
  await page
    .getByRole("option", { name: "Windows Server · 虚拟机", exact: true })
    .click();
  await expect(drawer.getByLabel("CPU · 核", { exact: true })).toHaveValue("4");
  await expect(drawer.getByLabel("内存 · GiB", { exact: true })).toHaveValue(
    "4",
  );
  await drawer.getByRole("button", { name: "来宾设置", exact: true }).click();
  await drawer
    .getByRole("textbox", { name: "主机名", exact: true })
    .fill("windows-new");
  await drawer
    .getByRole("textbox", { name: "登录用户", exact: true })
    .fill("operator");
  await drawer
    .getByRole("textbox", { name: "SSH 公钥", exact: true })
    .fill("ssh-ed25519 fixture-public-key");
  await drawer.getByRole("button", { name: "添加到环境", exact: true }).click();
  await page.getByRole("button", { name: "保存草稿", exact: true }).click();
  const draft = calls.find((call) => call.path.endsWith("/draft"))?.body as {
    spec: { assets: Record<string, unknown>[] };
  };
  expect(
    draft.spec.assets.find((asset) => asset.name === "Windows Server-2"),
  ).toMatchObject({
    resources: { cpu: 4, memoryMiB: 4096, diskGiB: 40 },
    guest: {
      hostname: "windows-new",
      username: "operator",
      sshAuthorizedKeys: ["ssh-ed25519 fixture-public-key"],
    },
  });
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
  const attempts: unknown[] = [];
  await page.route("**/api/v1/environments*", async (route) => {
    if (route.request().method() === "POST") {
      attempts.push(route.request().postDataJSON());
      if (attempts.length === 1) {
        await route.fulfill({
          status: 502,
          contentType: "application/json",
          body: JSON.stringify({ detail: "连接中断，请重试" }),
        });
        return;
      }
    }
    await route.fallback();
  });
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
    page.getByText("连接中断，请重试", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "创建并运行", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "独立 Web 环境", exact: true }),
  ).toBeVisible();
  const create = calls.find(
    (call) => call.path === "/environments" && call.method === "POST",
  );
  expect(create?.body).toEqual({
    name: "独立 Web 环境",
    projectId: "lab-project",
    blueprintVersionId: "training-v1",
    run: true,
    clientRequestId: expect.any(String),
  });
  expect(attempts).toEqual([create?.body, create?.body]);
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
    clientRequestId: expect.any(String),
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
    projectId: "lab-project",
    clientRequestId: expect.any(String),
    blueprintVersionId: "training-v2",
    run: false,
  });
});

test("asset rebuild requires scoped manage and confirms its system disk effect", async ({
  page,
}) => {
  const calls = await fixture(page, {
    permissions: ["read", "operate"],
    assetPermissions: { windows: ["manage"] },
  });
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "对象列表", exact: true }).click();
  await page.getByRole("button", { name: "web-01", exact: true }).click();
  await page.getByRole("button", { name: "对象操作", exact: true }).click();
  await expect(
    page.getByRole("menuitem", { name: "重建资产", exact: true }),
  ).toHaveCount(0);
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "windows-01", exact: true }).click();
  await page.getByRole("button", { name: "对象操作", exact: true }).click();
  await page.getByRole("menuitem", { name: "重建资产", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "重建 windows-01" });
  await expect(dialog).toContainText("系统盘上的改动将清除");
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  expect(calls.some((call) => call.path.endsWith("/actions"))).toBe(false);
  await page.getByRole("button", { name: "对象操作", exact: true }).click();
  await page.getByRole("menuitem", { name: "重建资产", exact: true }).click();
  await dialog.getByRole("button", { name: "重建资产", exact: true }).click();
  await expect
    .poll(() => calls.filter((call) => call.path.endsWith("/actions")))
    .toEqual([
      {
        method: "POST",
        path: "/environments/env/assets/windows/actions",
        body: {
          action: "rebuild",
          expectedRevision: 1,
          clientRequestId: expect.any(String),
        },
      },
    ]);
});

for (const retryable of [true, false]) {
  test(`failed task retry follows server eligibility ${retryable}`, async ({
    page,
  }) => {
    const calls = await fixture(page, {
      operation: {
        id: "failed-task",
        environmentId: "env",
        kind: "change",
        state: "failed",
        phase: "prepare",
        completed: 0,
        total: 1,
        retryable,
        error: "节点连接中断",
        createdAt: "2026-10-02T08:00:00Z",
        updatedAt: "2026-10-02T08:10:00Z",
      },
    });
    await page.goto("/environments/env");
    await page.getByRole("button", { name: /^任务/ }).click();
    const retry = page.getByRole("button", { name: "重试", exact: true });
    if (!retryable) {
      await expect(retry).toHaveCount(0);
      return;
    }
    await retry.click();
    await expect
      .poll(() => calls.filter((call) => call.path.endsWith("/retry")))
      .toEqual([
        { method: "POST", path: "/operations/failed-task/retry", body: null },
      ]);
    await expect(retry).toHaveCount(0);
  });
}

for (const [projectId, permission, canCreate] of [
  ["lab-project", "read", false],
  ["other-project", "compose", false],
  ["lab-project", "compose", true],
] as const) {
  test(`blueprint creation follows project compose ${projectId} ${permission}`, async ({
    page,
  }) => {
    await fixture(page, {
      identity: {
        id: "user",
        name: "operator",
        administrator: false,
        grants: [
          {
            scopeKind: "project",
            scopeId: projectId,
            permissions: [permission],
          },
        ],
      },
    });
    await page.goto("/templates?tab=environments");
    await page
      .getByRole("button", { name: "混合组网模板", exact: true })
      .click();
    const drawer = page.getByRole("dialog", { name: "混合组网模板" });
    await expect(drawer).toContainText("2 个资产");
    await expect(
      drawer.getByRole("button", { name: "创建环境", exact: true }),
    ).toHaveCount(canCreate ? 1 : 0);
  });
}

test("suspended environment and asset both offer a normal stop", async ({
  page,
}) => {
  const calls = await fixture(page, { status: "suspended" });
  await page.goto("/environments/env");
  await expect(
    page.getByRole("button", { name: "继续运行", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "环境操作", exact: true }).click();
  await page.getByRole("menuitem", { name: "停止环境", exact: true }).click();
  await expect
    .poll(() =>
      calls.filter((call) => call.path === "/environments/env/actions"),
    )
    .toEqual([
      {
        method: "POST",
        path: "/environments/env/actions",
        body: {
          action: "stop",
          expectedRevision: 1,
          clientRequestId: expect.any(String),
        },
      },
    ]);
  await page.getByRole("button", { name: "对象列表", exact: true }).click();
  await page.getByRole("button", { name: "windows-01", exact: true }).click();
  await page.getByRole("button", { name: "对象操作", exact: true }).click();
  await expect(
    page.getByRole("menuitem", { name: "继续运行", exact: true }),
  ).toBeEnabled();
  await page.getByRole("menuitem", { name: "关机", exact: true }).click();
  await expect
    .poll(
      () =>
        calls.find(
          (call) => call.path === "/environments/env/assets/windows/actions",
        )?.body,
    )
    .toEqual({
      action: "stop",
      expectedRevision: 1,
      clientRequestId: expect.any(String),
    });
});

test("stopped assets cannot pause", async ({ page }) => {
  await fixture(page, { status: "stopped" });
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "对象列表", exact: true }).click();
  await page.getByRole("button", { name: "windows-01", exact: true }).click();
  await page.getByRole("button", { name: "对象操作", exact: true }).click();
  await expect(
    page.getByRole("menuitem", { name: "暂停", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByRole("menuitem", { name: "启动", exact: true }),
  ).toBeEnabled();
});

test("asset-scoped session opens its console without environment management", async ({
  page,
}) => {
  await fixture(page, {
    permissions: [],
    assetPermissions: { windows: ["read", "session"] },
    identity: { id: "scoped", name: "scoped", administrator: false },
  });
  const connections: string[] = [];
  await page.routeWebSocket(
    /\/api\/v1\/environments\/env\/assets\/windows\/console\?kind=vnc$/,
    (socket) => {
      connections.push(socket.url());
      socket.close({ code: 1000, reason: "fixture completed" });
    },
  );
  await page.goto("/environments/env");
  await expect(
    page.getByRole("button", { name: "共享环境", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "环境操作", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "对象列表", exact: true }).click();
  await page.getByRole("button", { name: "web-01", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "终端", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "windows-01", exact: true }).click();
  await expect(page.locator(".template-properties")).toContainText("UEFI");
  await page.getByRole("button", { name: "控制台", exact: true }).click();
  await expect
    .poll(() => [...new Set(connections)])
    .toEqual([
      `${new URL(page.url()).origin.replace(/^http/, "ws")}/api/v1/environments/env/assets/windows/console?kind=vnc`,
    ]);
});

test("ordinary asset editing retains VM specifications while source is hidden", async ({
  page,
}) => {
  const calls = await fixture(page, {
    permissions: ["read", "compose"],
    identity: { id: "composer", name: "composer", administrator: false },
  });
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await page.getByRole("button", { name: "对象列表", exact: true }).click();
  await page.getByRole("button", { name: "windows-01", exact: true }).click();
  await page.getByRole("button", { name: "编辑", exact: true }).click();
  const editor = page.getByRole("dialog", { name: "编辑资产" });
  await expect(
    editor.getByRole("textbox", { name: "资产模板", exact: true }),
  ).toHaveValue("Windows Server · 虚拟机");
  await expect(editor.getByLabel("CPU · 核", { exact: true })).toHaveValue("4");
  await editor.getByRole("button", { name: "来宾设置", exact: true }).click();
  await editor
    .getByRole("textbox", { name: "主机名", exact: true })
    .fill("desktop-01");
  await editor.getByRole("button", { name: "更新资产", exact: true }).click();
  await page.getByRole("button", { name: "保存草稿", exact: true }).click();
  await expect
    .poll(() => calls.find((call) => call.path.endsWith("/draft"))?.body)
    .toMatchObject({
      spec: {
        assets: [
          { id: "web" },
          {
            id: "windows",
            templateId: "win",
            resources: { cpu: 4, memoryMiB: 4096, diskGiB: 40 },
            guest: { hostname: "desktop-01" },
          },
        ],
      },
    });
});

test("container logs distinguish stderr and switch the requested stream", async ({
  page,
}) => {
  await fixture(page);
  const queries: string[] = [];
  await page.route(
    "**/api/v1/environments/env/assets/web/logs?**",
    async (route) => {
      const stream = new URL(route.request().url()).searchParams.get("stream")!;
      queries.push(stream);
      const chunks = [
        { stream: "stdout", data: "request complete\n" },
        { stream: "stderr", data: "worker failed\n" },
      ].filter((chunk) => stream === "all" || chunk.stream === stream);
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: chunks
          .map((chunk) => `event: output\ndata: ${JSON.stringify(chunk)}\n\n`)
          .join(""),
      });
    },
  );
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "对象列表", exact: true }).click();
  await page.getByRole("button", { name: "web-01", exact: true }).click();
  await page.getByRole("button", { name: "对象操作", exact: true }).click();
  await page.getByRole("menuitem", { name: "进程日志", exact: true }).click();
  const drawer = page.getByRole("dialog", { name: "web-01 · 进程日志" });
  await expect(drawer.getByLabel("容器日志")).toContainText("request complete");
  await expect(drawer.getByLabel("容器日志")).toContainText("worker failed");
  await drawer.getByText("错误", { exact: true }).click();
  await expect.poll(() => [...new Set(queries)]).toEqual(["all", "stderr"]);
  await expect(drawer.getByLabel("容器日志")).not.toContainText(
    "request complete",
  );
  await expect(drawer.getByLabel("容器日志")).toContainText("worker failed");
});

test("container restart policy is edited with the ordinary asset draft", async ({
  page,
}) => {
  const calls = await fixture(page);
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await page.getByRole("button", { name: "对象列表", exact: true }).click();
  await page.getByRole("button", { name: "web-01", exact: true }).click();
  await page.getByRole("button", { name: "编辑", exact: true }).click();
  const editor = page.getByRole("dialog", { name: "编辑资产" });
  await editor
    .getByRole("textbox", { name: "进程退出后", exact: true })
    .click();
  await page
    .getByRole("option", { name: "异常退出时重启", exact: true })
    .click();
  await editor.getByRole("button", { name: "更新资产", exact: true }).click();
  await page.getByRole("button", { name: "保存草稿", exact: true }).click();
  await expect
    .poll(() => calls.find((call) => call.path.endsWith("/draft"))?.body)
    .toMatchObject({
      spec: {
        assets: [{ id: "web", restartPolicy: "on-failure" }, { id: "windows" }],
      },
    });
});

test("service endpoints follow the selected asset and acceptance does not imply an applied mapping", async ({
  page,
}) => {
  await fixture(page);
  await page.route("**/api/v1/environments/env/services", (route) =>
    route.fulfill({
      json: [
        {
          id: "http-entry",
          assetId: "web",
          interfaceId: "eth-web",
          protocol: "tcp",
          targetPort: 80,
          address: "192.0.2.10",
          port: 32000,
          updatedAt: "2026-10-02T08:10:00Z",
        },
      ],
    }),
  );
  const submitted: unknown[] = [];
  await page.route(
    "**/api/v1/environments/env/assets/web/services",
    (route) => {
      submitted.push(route.request().postDataJSON());
      return route.fulfill({
        status: 202,
        json: {
          id: "service-op",
          kind: "change",
          state: "queued",
          phase: "queued",
          completed: 0,
          total: 1,
        },
      });
    },
  );
  const revoked: URL[] = [];
  await page.route(
    "**/api/v1/environments/env/services/http-entry?**",
    (route) => {
      revoked.push(new URL(route.request().url()));
      expect(route.request().method()).toBe("DELETE");
      return route.fulfill({
        status: 202,
        json: {
          id: "revoke-op",
          kind: "change",
          state: "queued",
          phase: "queued",
          completed: 0,
          total: 1,
        },
      });
    },
  );
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "对象列表", exact: true }).click();
  await page.getByRole("button", { name: "web-01", exact: true }).click();
  await page.getByRole("button", { name: "管理服务入口", exact: true }).click();
  const drawer = page.getByRole("dialog", { name: "web-01 · 服务" });
  await expect(drawer).toContainText("192.0.2.10:32000");
  await drawer.getByRole("button", { name: "开放服务", exact: true }).click();
  await drawer
    .getByRole("textbox", { name: "目标端口", exact: true })
    .fill("8080");
  await drawer.getByRole("button", { name: "开放服务", exact: true }).click();
  await expect
    .poll(() => submitted)
    .toEqual([
      expect.objectContaining({
        interfaceId: "eth-web",
        protocol: "tcp",
        targetPort: 8080,
        expectedRevision: 1,
      }),
    ]);
  expect(submitted[0]).not.toHaveProperty("listenPort");
  await expect(drawer).not.toContainText("TCP 8080");
  await drawer
    .getByRole("button", { name: "服务 tcp/80 操作", exact: true })
    .click();
  await page.getByRole("menuitem", { name: "撤销入口", exact: true }).click();
  await expect.poll(() => revoked.length).toBe(1);
  expect(revoked[0].searchParams.get("expectedRevision")).toBe("1");
});

test("asset read permission does not expose service management", async ({
  page,
}) => {
  await fixture(page, { permissions: ["read"] });
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "对象列表", exact: true }).click();
  await page.getByRole("button", { name: "web-01", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "管理服务入口", exact: true }),
  ).toHaveCount(0);
});

test("external LAN editor reads live interfaces and preserves the LAN gateway", async ({
  page,
}) => {
  const calls = await fixture(page);
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await page.getByRole("button", { name: "对象列表", exact: true }).click();
  await page.getByRole("button", { name: "应用网段", exact: true }).click();
  await page.getByRole("button", { name: "编辑", exact: true }).click();
  const editor = page.getByRole("dialog", { name: "编辑网段" });
  await editor.getByText("已有 LAN", { exact: true }).click();
  await editor.getByRole("textbox", { name: "接入节点", exact: true }).click();
  await page.getByRole("option", { name: "计算节点", exact: true }).click();
  await editor.getByRole("textbox", { name: "外部接口", exact: true }).click();
  await expect(page.getByRole("option", { name: /eth0/ })).toHaveCount(0);
  await page
    .getByRole("option", { name: "br-lan · 192.0.2.254/24", exact: true })
    .click();
  await editor.getByRole("textbox", { name: "VLAN", exact: true }).fill("100");
  await editor
    .getByRole("textbox", { name: "网络地址 / 前缀", exact: true })
    .fill("192.0.2.0/24");
  await editor
    .getByRole("textbox", { name: "Netlab 可分配地址段", exact: true })
    .fill("192.0.2.128/26");
  await editor
    .getByRole("textbox", { name: "LAN 网关", exact: true })
    .fill("192.0.2.1");
  await page.setViewportSize({ width: 1366, height: 900 });
  await page.screenshot({ path: "test-results/external-lan-1366.png" });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({ path: "test-results/external-lan-390.png" });
  expect(
    await editor.evaluate((element) => element.scrollWidth),
  ).toBeLessThanOrEqual(390);
  await editor.getByRole("button", { name: "更新网段", exact: true }).click();
  await page.getByRole("button", { name: "保存草稿", exact: true }).click();
  expect(
    calls.find((call) => call.path.endsWith("/draft"))?.body,
  ).toMatchObject({
    spec: {
      networks: [
        {
          id: "lan",
          cidr: "192.0.2.0/24",
          gateway: "192.0.2.1",
          allocationPool: "192.0.2.128/26",
          external: { nodeId: "node", interface: "br-lan", vlan: 100 },
        },
      ],
    },
  });
});
