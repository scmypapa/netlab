import { expect, test, type Page } from "@playwright/test";
import ELK from "elkjs";
import type { Environment, EnvironmentSpec } from "../src/api/client";
import {
  networkContains,
  networkOptions,
  networksOverlap,
  routerGateways,
} from "../src/features/workbench/networking";
import { connectRouter, topology } from "../src/features/workbench/topology";
import {
  describeEndpoint,
  endpointFilter,
  selectionMatches,
  selectionNetwork,
} from "../src/features/workbench/trafficModel";

function scenario(): EnvironmentSpec {
  return {
    assets: ["a", "b"].map((networkId, index) => ({
      id: `app-${networkId}`,
      name: `应用 ${networkId.toUpperCase()}`,
      templateId: "linux",
      resources: { cpu: 0.5, memoryMiB: 64, diskGiB: 1 },
      interfaces: [
        {
          id: `nic-${networkId}`,
          networkId,
          mac: `02:00:00:00:00:0${index + 1}`,
          address: "10.0.0.10",
          primary: true,
        },
      ],
    })),
    networks: [
      { id: "a", name: "生产网", cidr: "10.0.0.0/24", gateway: "10.0.0.1" },
      { id: "b", name: "隔离网", cidr: "10.0.0.0/24", gateway: "10.0.0.1" },
      {
        id: "transit",
        name: "中转网",
        cidr: "10.255.0.0/30",
        gateway: "10.255.0.1",
      },
    ],
    routers: [
      {
        id: "router-a",
        name: "边界路由器",
        interfaces: [
          { id: "ra-a", networkId: "a", address: "10.0.0.1" },
          { id: "ra-transit", networkId: "transit", address: "10.255.0.1" },
        ],
      },
      {
        id: "router-b",
        name: "现场路由器",
        interfaces: [
          { id: "rb-b", networkId: "b", address: "10.0.0.1" },
          { id: "rb-transit", networkId: "transit", address: "10.255.0.2" },
        ],
      },
    ],
    routes: [
      {
        routerId: "router-a",
        networkId: "transit",
        destination: "0.0.0.0/0",
        nextHop: "10.255.0.2",
      },
    ],
  };
}

async function fixture(page: Page, spec = scenario()) {
  const environment: Environment = {
    id: "env",
    projectId: "lab",
    name: "分区网络",
    revision: 1,
    status: "running",
    permissions: ["read", "compose", "operate", "access"],
    spec,
    appliedSpec: spec,
    view: { positions: {} },
    createdAt: "2026-10-26T08:00:00Z",
    updatedAt: "2026-10-26T08:00:00Z",
  };
  const calls: {
    path: string;
    body: {
      spec?: EnvironmentSpec;
      mode?: string;
      networkIds?: string[];
    } | null;
  }[] = [];
  await page.addInitScript(() => {
    Object.assign(window, {
      EventSource: class extends EventTarget {
        onopen: (() => void) | null = null;
        constructor() {
          super();
          setTimeout(() => this.onopen?.(), 0);
        }
        close() {}
      },
    });
  });
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request(),
      path = new URL(request.url()).pathname.replace("/api/v1", ""),
      body = request.postDataJSON();
    calls.push({ path, body });
    let response: unknown = [],
      status = 200;
    if (path === "/identity")
      response = { id: "user", name: "operator", administrator: true };
    else if (path === "/templates")
      response = [
        {
          id: "linux",
          name: "Alpine",
          kind: "container",
          os: "Linux",
          version: 1,
          source: "alpine:latest",
          state: "ready",
          resources: { cpu: 1, memoryMiB: 64, diskGiB: 1 },
        },
      ];
    else if (path === "/environments/env") response = environment;
    else if (path === "/environments/env/state")
      response = {
        id: "env",
        revision: 1,
        status: "running",
        assets: [],
        updatedAt: environment.updatedAt,
      };
    else if (path.endsWith("/draft")) status = 204;
    else if (path.endsWith("/changes"))
      response = {
        revision: 1,
        changes: [
          {
            id: "routing",
            kind: "network",
            name: "网络配置",
            effect: "update",
            requiresStop: false,
          },
        ],
      };
    else if (path.endsWith("/vpn-access") && request.method() === "POST") {
      response = {
        id: "task",
        environmentId: "env",
        kind: "vpn-create",
        state: "queued",
        phase: "queued",
        completed: 0,
        total: 1,
        createdAt: environment.createdAt,
        updatedAt: environment.updatedAt,
      };
      status = 202;
    }
    await route.fulfill(
      status === 204 ? { status } : { status, json: response },
    );
  });
  await page.goto("/environments/env");
  await expect(page.getByRole("heading", { name: "分区网络" })).toBeVisible();
  return { calls, environment };
}

test("routing topology uses native router nodes and stable interface links", async () => {
  const spec = scenario(),
    graph = await topology(spec, [], new ELK());
  expect(
    graph.nodes.filter((node) => node.type === "router").map((node) => node.id),
  ).toEqual(["router-a", "router-b"]);
  expect(
    graph.edges
      .filter((edge) => edge.target === "transit")
      .map((edge) => edge.id),
  ).toEqual(["ra-transit", "rb-transit"]);
  expect(
    graph.nodes.filter((node) => node.data.secondary === "10.0.0.0/24"),
  ).toHaveLength(2);
  expect(
    new Set(
      graph.nodes.map((node) =>
        JSON.stringify(
          node.parentId
            ? { parent: node.parentId, ...node.position }
            : node.position,
        ),
      ),
    ).size,
  ).toBe(graph.nodes.length);
  const connected = connectRouter(spec, "router-a", "b");
  expect(connected.routers?.[0].interfaces).toHaveLength(3);
  expect(connectRouter(connected, "router-a", "b")).toEqual(connected);
  expect(connected.routers?.[0].interfaces[2]).toMatchObject({
    networkId: "b",
    address: "",
  });
});

test("gateway removal follows native interface identity without changing an external gateway", () => {
  const spec = scenario(),
    remaining = [spec.routers![1]];
  expect(routerGateways(spec.networks, spec.routers!, remaining)).toMatchObject(
    [
      { id: "a", gateway: undefined },
      { id: "b", gateway: "10.0.0.1" },
      { id: "transit", gateway: "10.255.0.2" },
    ],
  );
  const external = {
    ...spec.networks[0],
    external: { nodeId: "node", interface: "eth0" },
  };
  expect(routerGateways([external], spec.routers!, []).at(0)?.gateway).toBe(
    "10.0.0.1",
  );
  expect(
    routerGateways(
      [{ ...spec.networks[0], gateway: "10.0.0.254" }],
      spec.routers!,
      [],
    ).at(0)?.gateway,
  ).toBe("10.0.0.254");
});

test("traffic endpoint identities and packet filters stay scoped to their network", () => {
  const spec = scenario(),
    flow = {
      source: "10.0.0.1",
      destination: "10.0.0.99",
      sourceAssetId: "",
      destinationAssetId: "",
      networkId: "a",
    };
  const a = describeEndpoint(spec, flow, "source"),
    b = describeEndpoint(spec, { ...flow, networkId: "b" }, "source");
  expect(a.name).toBe("边界路由器");
  expect(b.name).toBe("现场路由器");
  expect(a.id).not.toBe(b.id);
  const selection = { asset: a.id };
  expect(selectionNetwork(spec, selection)).toBe("a");
  expect(endpointFilter(spec, [a.id])).toBe("(ip.addr == 10.0.0.1)");
  expect(selectionMatches(spec, selection, { ...flow, networkId: "b" })).toBe(
    false,
  );
  const unknown = describeEndpoint(
    spec,
    { ...flow, networkId: undefined },
    "destination",
  );
  expect(selectionNetwork(spec, { asset: unknown.id })).toBeUndefined();
});

test("network overlap covers IPv4 and IPv6 without merging address families", () => {
  const network = (cidr: string) => ({ id: cidr, name: cidr, cidr });
  expect(
    networksOverlap(network("10.0.0.0/24"), network("10.0.0.128/25")),
  ).toBe(true);
  expect(networksOverlap(network("10.0.0.0/24"), network("10.0.1.0/24"))).toBe(
    false,
  );
  expect(
    networksOverlap(network("fd10::/48"), network("fd10:0:0:1::/64")),
  ).toBe(true);
  expect(
    networksOverlap(network("fd10::/64"), network("fd10:0:0:1::/64")),
  ).toBe(false);
  expect(networksOverlap(network("10.0.0.0/24"), network("fd10::/64"))).toBe(
    false,
  );
  expect(networkContains("::ffff:192.0.2.0/120", "::ffff:c000:201")).toBe(true);
});

test("duplicate network labels use routing context and a sequence only when needed", () => {
  const spec = scenario();
  expect(networkOptions(spec).map((item) => item.label)).toEqual([
    "生产网 · 10.0.0.0/24",
    "隔离网 · 10.0.0.0/24",
    "中转网 · 10.255.0.0/30",
  ]);
  spec.networks[0].name = spec.networks[1].name = "LAN";
  let options = networkOptions(spec);
  expect(options[0]).toEqual({
    value: "a",
    title: "LAN · 边界路由器",
    label: "LAN · 边界路由器 · 10.0.0.0/24",
  });
  expect(options[1].label).toBe("LAN · 现场路由器 · 10.0.0.0/24");
  spec.routers = [];
  options = networkOptions(spec);
  expect(options[0].label).toBe("1. LAN · 纯二层 · 10.0.0.0/24");
  expect(options[1].label).toBe("2. LAN · 纯二层 · 10.0.0.0/24");
  expect(options[0].value).toBe("a");
});

test("router editor preserves interface identity and submits routes through the environment preview", async ({
  page,
}, testInfo) => {
  const { calls } = await fixture(page);
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await page.locator('[data-id="router-a"]').click();
  const inspector = page.getByRole("complementary", { name: "对象详情" });
  await inspector.getByRole("button", { name: "编辑", exact: true }).click();
  const drawer = page.getByRole("dialog", { name: "编辑路由器" });
  await drawer.getByLabel("路由器名称").fill("出口路由器");
  await drawer.getByLabel("接口 1 地址", { exact: true }).fill("10.0.0.254");
  await drawer.getByLabel("下一跳").fill("10.255.0.2");
  await page.screenshot({ path: testInfo.outputPath("router-editor.png") });
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await drawer.evaluate(
      (element) => element.scrollWidth > element.clientWidth,
    ),
  ).toBe(false);
  await page.screenshot({
    path: testInfo.outputPath("router-editor-narrow.png"),
  });
  await drawer.getByRole("button", { name: "更新路由器", exact: true }).click();
  await page.getByRole("button", { name: "预览变更", exact: true }).click();
  await expect(
    page.getByRole("dialog", { name: "应用环境变更" }),
  ).toBeVisible();
  const saved = calls.find((call) => call.path.endsWith("/changes"))!.body!
    .spec!;
  expect(saved.routers?.[0]).toMatchObject({
    id: "router-a",
    name: "出口路由器",
    interfaces: [{ id: "ra-a", address: "10.0.0.254" }, { id: "ra-transit" }],
  });
  expect(saved.networks[0].gateway).toBe("10.0.0.254");
  expect(saved.routes).toEqual([
    {
      routerId: "router-a",
      networkId: "transit",
      destination: "0.0.0.0/0",
      nextHop: "10.255.0.2",
    },
  ]);
  expect(saved.routers?.[1]).toEqual(scenario().routers![1]);
});

test("router removal cleans routes and only that router's default gateway", async ({
  page,
}) => {
  const { calls } = await fixture(page);
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await page.locator('[data-id="router-a"]').click();
  const inspector = page.getByRole("complementary", { name: "对象详情" });
  await inspector.getByRole("button", { name: "对象操作" }).click();
  await page.getByRole("menuitem", { name: "移除路由器" }).click();
  await page.getByRole("button", { name: "预览变更", exact: true }).click();
  const saved = calls.find((call) => call.path.endsWith("/changes"))!.body!
    .spec!;
  expect(saved.routers?.map((router) => router.id)).toEqual(["router-b"]);
  expect(saved.routes).toEqual([]);
  expect(saved.networks[0].gateway).toBeUndefined();
  expect(saved.networks[1].gateway).toBe("10.0.0.1");
  expect(saved.networks[2].gateway).toBe("10.255.0.2");
});

test("overlapping VPN networks automatically use independent access addresses", async ({
  page,
}) => {
  const { calls } = await fixture(page);
  await page.getByRole("button", { name: "VPN", exact: true }).click();
  await page.getByRole("button", { name: "新建连接" }).click();
  await expect(
    page.getByRole("dialog", { name: "VPN", exact: true }),
  ).not.toContainText("中转网");
  await page.getByLabel("连接名称").fill("测试接入");
  await expect(page.getByRole("radio", { name: "独立访问地址" })).toBeChecked();
  await expect(
    page.getByRole("radio", { name: "原地址", exact: true }),
  ).toBeDisabled();
  await page.getByRole("button", { name: "创建连接", exact: true }).click();
  await expect
    .poll(
      () =>
        calls.find((call) => call.path.endsWith("/vpn-access") && call.body)
          ?.body?.mode,
    )
    .toBe("translated");
  expect(
    calls.find((call) => call.path.endsWith("/vpn-access") && call.body)?.body
      ?.networkIds,
  ).toEqual(["a", "b"]);
});

test("VPN can authorize a registered device on a routerless layer-two network", async ({
  page,
}) => {
  const spec: EnvironmentSpec = {
    assets: [],
    routers: [],
    networks: [
      {
        id: "plant",
        name: "现场设备网",
        cidr: "192.168.8.0/24",
        devices: [
          {
            id: "plc",
            name: "PLC",
            address: "192.168.8.10",
            mac: "02:00:00:00:00:01",
          },
        ],
      },
    ],
  };
  const { calls } = await fixture(page, spec);
  await page.getByRole("button", { name: "VPN", exact: true }).click();
  await page.getByRole("button", { name: "新建连接" }).click();
  await page.getByLabel("连接名称").fill("现场接入");
  await expect(
    page.getByRole("dialog", { name: "VPN", exact: true }),
  ).toContainText("现场设备网 · 192.168.8.0/24");
  await page.getByRole("button", { name: "创建连接", exact: true }).click();
  await expect
    .poll(
      () =>
        calls.find((call) => call.path.endsWith("/vpn-access") && call.body)
          ?.body?.networkIds,
    )
    .toEqual(["plant"]);
});

test("new ordinary networks attach to the only router with an allocated interface identity", async ({
  page,
}) => {
  const spec = scenario();
  spec.routers = spec.routers!.slice(0, 1);
  const { calls } = await fixture(page, spec);
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await page.getByRole("button", { name: "添加", exact: true }).click();
  await page.getByRole("menuitem", { name: "网段", exact: true }).click();
  const drawer = page.getByRole("dialog", { name: "添加网段" });
  await drawer.getByLabel("网段名称").fill("业务网");
  await drawer.getByRole("button", { name: "添加到环境", exact: true }).click();
  await page.getByRole("button", { name: "预览变更", exact: true }).click();
  await expect(
    page.getByRole("dialog", { name: "应用环境变更" }),
  ).toBeVisible();
  const saved = calls.find((call) => call.path.endsWith("/changes"))!.body!
    .spec!;
  const network = saved.networks.find((network) => network.name === "业务网")!;
  const iface = saved.routers![0].interfaces.find(
    (iface) => iface.networkId === network.id,
  )!;
  expect(iface.id).toBeTruthy();
  expect(iface.address).toBe("");
  expect(saved.routers![0].interfaces.slice(0, 2)).toEqual(
    spec.routers![0].interfaces,
  );
});

test("new networks do not connect ambiguous routing domains implicitly", async ({
  page,
}) => {
  const { calls } = await fixture(page);
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await page.getByRole("button", { name: "添加", exact: true }).click();
  await page.getByRole("menuitem", { name: "网段", exact: true }).click();
  const drawer = page.getByRole("dialog", { name: "添加网段" });
  await drawer.getByLabel("网段名称").fill("纯二层网");
  await drawer.getByLabel("网络地址 / 前缀").fill("10.0.0.0/24");
  await drawer.getByRole("button", { name: "添加到环境", exact: true }).click();
  await page.getByRole("button", { name: "预览变更", exact: true }).click();
  await expect(
    page.getByRole("dialog", { name: "应用环境变更" }),
  ).toBeVisible();
  const saved = calls.find((call) => call.path.endsWith("/changes"))!.body!
    .spec!;
  expect(saved.routers).toEqual(scenario().routers);
  expect(
    saved.networks.find((network) => network.name === "纯二层网")?.gateway,
  ).toBeUndefined();
});

test("native router creation edits interface and route in the same environment draft", async ({
  page,
}) => {
  const { calls } = await fixture(page);
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await page.getByRole("button", { name: "添加", exact: true }).click();
  await page.getByRole("menuitem", { name: "路由器", exact: true }).click();
  const drawer = page.getByRole("dialog", { name: "添加路由器" });
  await drawer.getByLabel("路由器名称").fill("业务路由器");
  await drawer.getByLabel("连接网段").click();
  await page
    .getByRole("option", { name: "生产网 · 10.0.0.0/24", exact: true })
    .click();
  await drawer.getByLabel("接口 1 地址", { exact: true }).fill("10.0.0.254");
  await drawer.locator("summary").click();
  await drawer.getByRole("button", { name: "添加路由", exact: true }).click();
  await drawer.getByLabel("目标网段").fill("172.18.0.0/24");
  await drawer.getByLabel("下一跳").fill("10.0.0.253");
  await drawer.getByRole("button", { name: "添加到环境", exact: true }).click();
  await page.getByRole("button", { name: "预览变更", exact: true }).click();
  await expect(
    page.getByRole("dialog", { name: "应用环境变更" }),
  ).toBeVisible();
  const saved = calls.find((call) => call.path.endsWith("/changes"))!.body!
    .spec!;
  const router = saved.routers!.find((router) => router.name === "业务路由器")!;
  expect(router.interfaces[0]).toMatchObject({
    networkId: "a",
    address: "10.0.0.254",
  });
  expect(saved.routes).toEqual([
    ...scenario().routes!,
    {
      routerId: router.id,
      networkId: "a",
      destination: "172.18.0.0/24",
      nextHop: "10.0.0.253",
    },
  ]);
  expect(saved.networks[0].gateway).toBe("10.0.0.1");
});

test("identically named networks stay distinguishable in router, route and VPN selectors", async ({
  page,
}) => {
  const spec = scenario();
  spec.networks[0].name = spec.networks[1].name = "LAN";
  await fixture(page, spec);
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await page.locator('[data-id="router-a"]').click();
  await page
    .getByRole("complementary", { name: "对象详情" })
    .getByRole("button", { name: "编辑", exact: true })
    .click();
  const drawer = page.getByRole("dialog", { name: "编辑路由器" });
  await drawer.getByLabel("连接网段").click();
  await expect(
    page.getByRole("option", {
      name: "LAN · 现场路由器 · 10.0.0.0/24",
      exact: true,
    }),
  ).toBeVisible();
  await drawer.getByLabel("路由器名称").click();
  await drawer.getByLabel("出口网段").click();
  await expect(
    page.getByRole("option", {
      name: "LAN · 边界路由器 · 10.0.0.0/24",
      exact: true,
    }),
  ).toBeVisible();
  await drawer.getByLabel("路由器名称").click();
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "添加", exact: true }).click();
  await page.getByRole("menuitem", { name: "资产", exact: true }).click();
  const asset = page.getByRole("dialog", { name: "添加资产", exact: true });
  await asset.getByLabel("资产模板").click();
  await page
    .getByRole("option", { name: "Alpine · 容器", exact: true })
    .click();
  await asset.getByLabel("连接网段").click();
  await expect(
    page.getByRole("option", {
      name: "LAN · 现场路由器 · 10.0.0.0/24",
      exact: true,
    }),
  ).toBeVisible();
  await asset.getByLabel("资产名称").click();
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: "VPN", exact: true }).click();
  await page.getByRole("button", { name: "新建连接" }).click();
  const vpn = page.getByRole("dialog", { name: "VPN", exact: true });
  await expect(vpn).toContainText("LAN · 边界路由器 · 10.0.0.0/24");
  await expect(vpn).toContainText("LAN · 现场路由器 · 10.0.0.0/24");
});
