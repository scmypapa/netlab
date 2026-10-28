import { expect, test, type Page } from "@playwright/test";
import type { EnvironmentSpec, Schema } from "../src/api/client";

type Packet = Schema<"CapturePacket">;
type Flow = Schema<"CaptureFlow">;
// Browser interaction fixtures; real packet collection is verified on Linux separately.
const instant = Date.parse("2026-10-23T08:00:00Z");
const timestamp = (offset = 0) => new Date(instant + offset).toISOString();
const resources = { cpu: 2, memoryMiB: 2048, diskGiB: 24 };
const assets = [
  ["dc", "DC01", "172.22.1.2", "internal"],
  ["oa", "YINYU-OA01", "172.22.1.18", "internal"],
  ["win7", "YINYU-WIN7", "172.22.1.21", "internal"],
  ["web", "ubuntu-web01", "10.66.0.15", "entry"],
].map(([id, name, address, networkId], index) => ({
  id,
  name,
  templateId: id === "web" ? "linux" : "windows",
  resources,
  interfaces: [
    {
      id: `nic-${id}`,
      networkId,
      address,
      mac: `02:00:00:00:00:${String(index + 2).padStart(2, "0")}`,
      primary: true,
    },
  ],
}));
const spec = {
  assets,
  networks: [
    {
      id: "entry",
      name: "Entry LAN",
      cidr: "10.66.0.0/24",
      gateway: "10.66.0.1",
    },
    {
      id: "internal",
      name: "Internal LAN",
      cidr: "172.22.1.0/24",
      gateway: "172.22.1.1",
    },
  ],
};
assets[3].interfaces.push({
  id: "nic-web-internal",
  networkId: "internal",
  address: "172.22.1.15",
  mac: "02:00:00:00:01:05",
  primary: false,
});
const retention: Schema<"PacketRetention"> = {
  status: "recording",
  limitBytes: 1 << 30,
  usedBytes: 3 * 2 ** 20,
  oldestAt: timestamp(-60000),
  newestAt: timestamp(60000),
};

function frame(number: number, dns = number % 4 === 0): Packet {
  return {
    id: `node-a:ring:${number}`,
    number,
    time: timestamp(number * 10),
    nodeId: "node-a",
    assetId: "win7",
    interfaceId: "nic-win7",
    networkId: "internal",
    interfaceIndex: 0,
    source: "172.22.1.21",
    destination: dns ? "172.22.1.2" : "172.22.1.18",
    sourcePort: dns ? 63574 : 50000,
    destinationPort: dns ? 53 : 80,
    protocol: dns ? "UDP" : "TCP",
    length: 54,
    capturedLength: 54,
    summary: dns
      ? `UDP frame ${number} · 63574 → 53 Len=12`
      : `TCP frame ${number} · 50000 → 80 [ACK] Seq=1 Ack=1 Len=0`,
  };
}
function detail(packet: Packet): Schema<"CapturePacketDetail"> {
  const http = packet.protocol === "HTTP";
  const transport = packet.transportProtocol ?? packet.protocol;
  const payload = http
    ? Buffer.from("GET / HTTP/1.1\r\nHost: lab.example\r\n\r\n")
    : Buffer.alloc(0);
  const result: Schema<"CapturePacketDetail"> = {
    packet,
    hex:
      packet.protocol === "UDP"
        ? "0011223344550200000000040800450000280001400040110000ac160115ac160102f856003500140000000000000000000000000000"
        : "00112233445502000000000408004500" +
          (40 + payload.length).toString(16).padStart(4, "0") +
          "0001400040060000ac160115ac160112c350005000000001000000015010ffff00000000" +
          payload.toString("hex"),
    layers: [
      {
        name: "frame",
        label: `Frame ${packet.number}: ${packet.length} bytes`,
        offset: 0,
        length: packet.length,
        children: [],
      },
      {
        name: "eth",
        label: "Ethernet II",
        offset: 0,
        length: 14,
        children: [
          {
            name: "eth.dst",
            label: "Destination: 00:11:22:33:44:55",
            offset: 0,
            length: 6,
            children: [],
          },
          {
            name: "eth.src",
            label: "Source: 02:00:00:00:00:04",
            offset: 6,
            length: 6,
            children: [],
          },
        ],
      },
      {
        name: "ip",
        label: "Internet Protocol Version 4",
        offset: 14,
        length: 20,
        children: [
          {
            name: "ip.src",
            label: `Source Address: ${packet.source}`,
            offset: 26,
            length: 4,
            children: [],
          },
          {
            name: "ip.dst",
            label: `Destination Address: ${packet.destination}`,
            offset: 30,
            length: 4,
            children: [],
          },
        ],
      },
      {
        name: transport.toLowerCase(),
        label:
          packet.protocol === "UDP"
            ? "User Datagram Protocol"
            : "Transmission Control Protocol",
        offset: 34,
        length: packet.protocol === "UDP" ? 8 : 20,
        children: [
          {
            name: `${transport.toLowerCase()}.srcport`,
            label: `Source Port: ${packet.sourcePort}`,
            offset: 34,
            length: 2,
            children: [],
          },
          {
            name: `${transport.toLowerCase()}.dstport`,
            label: `Destination Port: ${packet.destinationPort}`,
            offset: 36,
            length: 2,
            children: [],
          },
        ],
      },
    ],
  };
  if (http)
    result.layers.push({
      name: "http",
      label: "Hypertext Transfer Protocol",
      offset: 54,
      length: payload.length,
      children: [
        {
          name: "http.request.method",
          label: "Request Method: GET",
          offset: 54,
          length: 3,
          children: [],
        },
      ],
    });
  return result;
}
function flow(
  source: string,
  destination: string,
  extra: Partial<Flow> = {},
): Flow {
  return {
    source,
    destination,
    sourcePort: 50000,
    destinationPort: 80,
    protocol: "TCP",
    bytes: 24000,
    packets: 24,
    bytesPerSecond: 2400,
    firstSeen: timestamp(),
    lastSeen: timestamp(500),
    ...extra,
  };
}
const initialFlows: Flow[] = [
  flow("02:00:00:00:00:04", "ff:ff:ff:ff:ff:ff", {
    networkId: "internal",
    protocol: "ARP",
    sourcePort: 0,
    destinationPort: 0,
    bytes: 60,
    bytesPerSecond: 0,
  }),
  flow("172.22.1.21", "172.22.1.2", {
    sourceAssetId: "win7",
    destinationAssetId: "dc",
    networkId: "internal",
    protocol: "UDP",
    sourcePort: 63574,
    destinationPort: 53,
    bytes: 128,
    bytesPerSecond: 0,
  }),
  flow("172.22.1.2", "172.22.1.21", {
    sourceAssetId: "dc",
    destinationAssetId: "win7",
    networkId: "internal",
    protocol: "UDP",
    sourcePort: 53,
    destinationPort: 63574,
    bytes: 256,
  }),
  flow("172.22.1.21", "172.22.1.18", {
    sourceAssetId: "win7",
    destinationAssetId: "oa",
    networkId: "internal",
  }),
  flow("172.22.1.18", "172.22.1.21", {
    sourceAssetId: "oa",
    destinationAssetId: "win7",
    networkId: "internal",
    sourcePort: 80,
    destinationPort: 50000,
    bytes: 260000,
  }),
  flow("10.66.0.15", "224.0.0.251", {
    sourceAssetId: "web",
    networkId: "entry",
    protocol: "UDP",
    bytes: 160,
    bytesPerSecond: 0,
  }),
  flow("10.66.0.15", "ff02::fb", {
    sourceAssetId: "web",
    networkId: "entry",
    protocol: "UDP",
    bytes: 80,
    bytesPerSecond: 0,
  }),
  flow("172.22.1.18", "172.22.1.255", {
    sourceAssetId: "oa",
    networkId: "internal",
    protocol: "UDP",
    bytes: 120,
    bytesPerSecond: 0,
  }),
  flow("172.22.1.2", "ff:ff:ff:ff:ff:ff", {
    sourceAssetId: "dc",
    networkId: "internal",
    protocol: "ARP",
    bytes: 60,
    bytesPerSecond: 0,
  }),
  flow("172.22.1.1", "172.22.1.18", {
    destinationAssetId: "oa",
    networkId: "internal",
  }),
];
const alert: Schema<"SecurityAlert"> = {
  id: 101,
  environmentId: "env",
  nodeId: "node-a",
  interfaceId: "nic-win7",
  networkId: "internal",
  sourceAssetId: "win7",
  destinationAssetId: "oa",
  source: "172.22.1.21",
  destination: "172.22.1.18",
  sourcePort: 50000,
  destinationPort: 80,
  protocol: "TCP",
  appProtocol: "http",
  generatorId: 1,
  signatureId: 2100001,
  revision: 1,
  severity: 1,
  signature: "ET WEB_SERVER Suspicious request",
  category: "Web Application Attack",
  count: 3,
  firstSeen: timestamp(),
  lastSeen: timestamp(1000),
};

async function fixture(page: Page, scene: EnvironmentSpec = spec) {
  const requests: { method: string; url: URL }[] = [];
  const packets = Array.from({ length: 36 }, (_, index) => frame(index + 1));
  const state = {
    flows: [...initialFlows],
    packets,
    older: [] as Packet[],
    nextCursor: undefined as string | undefined,
    packetReply: undefined as ((url: URL) => Packet[]) | undefined,
    alerts: [alert],
    olderAlerts: [] as Schema<"SecurityAlert">[],
    retention: { ...retention },
  };
  const byId = new Map(packets.map((packet) => [packet.id!, packet]));
  await page.addInitScript(() => {
    const streams: EventTarget[] = [];
    class Events extends EventTarget {
      onopen: (() => void) | null = null;
      constructor() {
        super();
        streams.push(this);
        setTimeout(() => this.onopen?.(), 0);
      }
      close() {
        streams.splice(streams.indexOf(this), 1);
      }
    }
    Object.assign(window, { EventSource: Events, trafficTestStreams: streams });
  });
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request(),
      url = new URL(request.url());
    const path = url.pathname.replace("/api/v1", "");
    requests.push({ method: request.method(), url });
    let response: unknown;
    if (path === "/identity")
      response = { id: "admin", name: "admin", administrator: true };
    else if (path === "/templates")
      response = ["linux", "windows"].map((id) => ({
        id,
        name: id,
        kind: "vm",
        os: id === "linux" ? "Linux" : "Windows",
        state: "ready",
        version: 1,
        resources,
      }));
    else if (path === "/environments/env")
      response = {
        id: "env",
        projectId: "lab",
        name: "企业网络实验室",
        revision: 1,
        status: "running",
        permissions: ["read", "observe", "compose", "operate", "manage"],
        spec: scene,
        appliedSpec: scene,
        view: { positions: {} },
        createdAt: timestamp(),
        updatedAt: timestamp(),
      };
    else if (path === "/environments/env/state")
      response = {
        id: "env",
        revision: 1,
        status: "running",
        assets: scene.assets.map((asset) => ({
          assetId: asset.id,
          state: "running",
        })),
        updatedAt: timestamp(),
      };
    else if (
      path === "/environments/env/services" ||
      path === "/environments/env/operations"
    )
      response = [];
    else if (path === "/environments/env/view")
      return route.fulfill({ status: 204 });
    else if (path === "/environments/env/traffic")
      response = {
        flows: state.flows,
        errors: {},
        samplingRate: 1,
        windowSeconds: 60,
        omittedSamples: 0,
        retention: state.retention,
      };
    else if (path === "/environments/env/metrics")
      response = {
        start: timestamp(-3600000),
        end: timestamp(60000),
        stepSeconds: 60,
        series: scene.assets.flatMap((asset, index) =>
          ["receive", "transmit"].map((metric) => ({
            metric,
            assetId: asset.id,
            nodeId: "node-a",
            instanceId: asset.id,
            interfaceId: asset.interfaces[0].id,
            points: Array.from({ length: 62 }, (_, point) => ({
              time: timestamp((point - 60) * 60000),
              value:
                1500 +
                index * 300 +
                Math.abs(Math.sin(point / 5 + index)) * 4000,
            })),
          })),
        ),
      };
    else if (path === "/environments/env/security")
      response = {
        start: timestamp(-3600000),
        end: timestamp(60000),
        alerts: url.searchParams.has("before")
          ? state.olderAlerts
          : state.alerts,
        counts: { high: 3, medium: 0, low: 0 },
        sensors: [],
        ...(!url.searchParams.has("before") && state.olderAlerts.length
          ? { nextCursor: 101 }
          : {}),
      };
    else if (path === "/environments/env/captures")
      response = {
        segments: [
          {
            id: "saved",
            nodeId: "node-a",
            nodeName: "Compute A",
            environmentId: "env",
            assetIds: ["win7"],
            status: "stopped",
            startedAt: timestamp(),
            finishedAt: timestamp(1000),
            bytes: 4096,
            packets: 8,
            omittedFlows: 0,
          },
        ],
        errors: {},
      };
    else if (path === "/environments/env/packets/export")
      return route.fulfill({
        contentType: "application/octet-stream",
        body: Buffer.from("0a0d0d0a", "hex"),
      });
    else if (path === "/environments/env/packets") {
      const rows =
        state.packetReply?.(url) ??
        (url.searchParams.has("cursor") ? state.older : state.packets);
      for (const packet of rows) byId.set(packet.id!, packet);
      response = {
        packets: rows,
        retention: state.retention,
        errors: {},
        ...(!url.searchParams.has("cursor") && state.nextCursor
          ? { nextCursor: state.nextCursor }
          : {}),
      };
    } else if (path.startsWith("/environments/env/packets/")) {
      const packet = byId.get(decodeURIComponent(path.split("/").at(-1)!));
      if (!packet)
        return route.fulfill({
          status: 404,
          json: { detail: "报文已过保留期" },
        });
      response = detail(packet);
    } else
      return route.fulfill({
        status: 404,
        json: { detail: `未配置的测试接口：${path}` },
      });
    await route.fulfill({ json: response });
  });
  return { state, requests };
}
async function open(page: Page) {
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "流量视图", exact: true }).click();
  await expect(page.getByRole("grid", { name: "报文列表" })).toBeVisible();
  await expect(
    page.getByRole("grid", { name: "报文列表" }).getByRole("row").nth(1),
  ).toBeVisible();
}

test("进度事件只刷新任务状态，终态同步关联资源", async ({ page }) => {
  const { requests } = await fixture(page);
  await open(page);
  await page.waitForLoadState("networkidle");
  const count = (suffix: string) =>
    requests.filter(({ url }) => url.pathname.endsWith(suffix)).length;
  const states = count("/state"),
    operations = count("/operations"),
    services = count("/services");
  const emit = (kind: string, repeat = 1, phase = "prepare") =>
    page.evaluate(
      ({ kind, repeat, phase }) => {
        const streams = (
          window as typeof window & { trafficTestStreams: EventTarget[] }
        ).trafficTestStreams;
        for (let i = 0; i < repeat; i++)
          for (const stream of streams)
            stream.dispatchEvent(
              new MessageEvent(kind, { data: JSON.stringify({ phase }) }),
            );
      },
      { kind, repeat, phase },
    );
  await emit("operation.progress", 100);
  await expect.poll(() => count("/state")).toBe(states + 1);
  await expect.poll(() => count("/operations")).toBe(operations + 1);
  await page.waitForLoadState("networkidle");
  expect(count("/services")).toBe(services);
  for (const [index, phase] of ["complete", "rolled-back"].entries()) {
    await emit("operation.progress", 1, phase);
    await expect.poll(() => count("/services")).toBe(services + index + 1);
  }
  for (const [index, kind] of [
    "succeeded",
    "failed",
    "partially_applied",
  ].entries()) {
    await emit(`operation.${kind}`);
    await expect.poll(() => count("/services")).toBe(services + index + 3);
  }
});

test("隐藏分析面板停止查询，展开后恢复实时刷新", async ({ page }) => {
  await page.setViewportSize({ width: 1366, height: 900 });
  await page.clock.install();
  const { requests, state } = await fixture(page);
  await open(page);
  const count = (suffix: string) =>
    requests.filter(({ url }) => url.pathname.endsWith(suffix)).length;
  await page.clock.fastForward(11_000);
  expect(count("/metrics")).toBe(0);
  expect(count("/packets")).toBeGreaterThan(1);
  expect(count("/traffic")).toBeGreaterThan(1);

  await page.getByRole("button", { name: /^告警 \d/ }).click();
  const packets = count("/packets");
  state.retention.status = "idle";
  await page.clock.fastForward(11_000);
  expect(count("/packets")).toBe(packets);
  await expect(page.getByText("无运行接口", { exact: true })).toBeVisible();

  await page.getByRole("button", { name: "流量趋势", exact: true }).click();
  await expect.poll(() => count("/metrics")).toBeGreaterThan(0);
  expect(
    requests
      .find(({ url }) => url.pathname.endsWith("/metrics"))!
      .url.searchParams.get("metrics"),
  ).toBe("receive,transmit");
  await page.getByRole("button", { name: "流量趋势", exact: true }).click();
  const history = count("/metrics");
  await page.clock.fastForward(11_000);
  expect(count("/metrics")).toBe(history);

  await page.getByRole("button", { name: "报文", exact: true }).click();
  await expect.poll(() => count("/packets")).toBeGreaterThan(packets);
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    page.getByRole("button", { name: "通信图", exact: true }),
  ).toBeVisible();
  const flows = count("/traffic");
  await page.clock.fastForward(11_000);
  expect(count("/traffic")).toBe(flows);
  await page.getByRole("button", { name: "通信图", exact: true }).click();
  await expect.poll(() => count("/traffic")).toBeGreaterThan(flows);
});

test("收起时间轴会取消在途历史请求", async ({ page }) => {
  await page.setViewportSize({ width: 1366, height: 900 });
  await fixture(page);
  await open(page);
  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/environments/env/metrics?**", async (route) => {
    await held;
    await route.fulfill({
      json: {
        start: timestamp(),
        end: timestamp(1000),
        stepSeconds: 10,
        series: [],
      },
    });
  });
  const requested = page.waitForRequest((request) =>
    request.url().includes("/env/metrics?"),
  );
  await page.getByRole("button", { name: "流量趋势", exact: true }).click();
  const request = await requested;
  const cancelled = page.waitForEvent("requestfailed", {
    predicate: (failed) => failed === request,
  });
  try {
    await page.getByRole("button", { name: "流量趋势", exact: true }).click();
    await cancelled;
    expect(request.failure()?.errorText).toContain("ERR_ABORTED");
  } finally {
    release();
  }
});
const connection = (page: Page, left = "DC01", right = "YINYU-WIN7") =>
  page
    .getByRole("region", { name: "资产通信图" })
    .getByRole("group", { name: new RegExp(`${left} ↔ ${right}`) });
async function clickConnection(page: Page) {
  await expect(page.locator("[data-smart-edge-placeholder]")).toHaveCount(0);
  const point = await connection(page)
    .locator(".react-flow__edge-interaction")
    .evaluate((element) => {
      const path = element as SVGPathElement;
      const length = path.getTotalLength(),
        matrix = path.getScreenCTM()!;
      const edge = element.closest(".react-flow__edge")!;
      for (let distance = length / 2; distance < length * 1.5; distance += 3) {
        const point = path.getPointAtLength(distance % length);
        const screen = new DOMPoint(point.x, point.y).matrixTransform(matrix);
        if (
          document
            .elementFromPoint(screen.x, screen.y)
            ?.closest(".react-flow__edge") === edge
        )
          return { x: screen.x, y: screen.y };
      }
      const middle = path.getPointAtLength(length / 2),
        screen = new DOMPoint(middle.x, middle.y).matrixTransform(matrix);
      throw new Error(
        "连接没有可点击的独立线段：" +
          JSON.stringify({
            path: path.getAttribute("d"),
            screen: { x: screen.x, y: screen.y },
            hit: document
              .elementFromPoint(screen.x, screen.y)
              ?.outerHTML.slice(0, 250),
            viewport: edge
              .closest(".react-flow")
              ?.getBoundingClientRect()
              .toJSON(),
            svgZ: getComputedStyle(edge.parentElement!).zIndex,
          }),
      );
    });
  await page.mouse.click(point.x, point.y);
}
const packetCalls = (
  requests: Awaited<ReturnType<typeof fixture>>["requests"],
) => requests.filter(({ url }) => url.pathname.endsWith("/packets"));

test("点连接直接分析报文，协议与字节双向联动，键盘可选择连接和帧", async ({
  page,
}) => {
  const { state, requests } = await fixture(page);
  state.packetReply = (url) =>
    url.searchParams.get("assetId") === "dc"
      ? state.packets
          .filter((packet) => packet.protocol === "UDP")
          .map((packet) => ({
            ...packet,
            assetId: "dc",
            interfaceId: "nic-dc",
          }))
      : state.packets;
  await open(page);
  await clickConnection(page);
  await expect
    .poll(() => packetCalls(requests).at(-1)?.url.searchParams.get("assetId"))
    .toBe("dc");
  const edgeQuery = packetCalls(requests).at(-1)!.url.searchParams;
  expect(edgeQuery.get("networkId")).toBe("internal");
  expect(edgeQuery.get("filter")).toBe(
    "(ip.addr == 172.22.1.21 || eth.addr == 02:00:00:00:00:04)",
  );
  const table = page.getByRole("grid", { name: "报文列表" });
  await expect(table.getByRole("row")).toHaveCount(10);
  expect(requests.some(({ url }) => /\/packets\/.+/.test(url.pathname))).toBe(
    false,
  );
  await expect(
    page.getByRole("button", { name: "开始抓包", exact: true }),
  ).toHaveCount(0);
  expect(requests.filter(({ method }) => method === "POST")).toHaveLength(0);
  const row = table.getByRole("row").filter({ hasText: "UDP frame 4 ·" });
  await row.click();
  await row.press("ArrowDown");
  await expect(
    table.getByRole("row").filter({ hasText: "UDP frame 8 ·" }),
  ).toHaveAttribute("aria-selected", "true");
  await expect(
    page
      .getByLabel("报文协议层")
      .getByRole("button", { name: "Frame 8: 54 bytes", exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", {
      name: "Destination: 00:11:22:33:44:55",
      exact: true,
    })
    .click();
  await expect(
    page.getByLabel("报文字节").locator('button[data-selected="true"]'),
  ).toHaveCount(12);
  await page.getByRole("button", { name: "字节 26，ac", exact: true }).click();
  await expect(
    page.getByRole("button", {
      name: "Source Address: 172.22.1.21",
      exact: true,
    }),
  ).toHaveAttribute("data-selected", "true");
  await page
    .getByRole("button", { name: "字节 26，ac", exact: true })
    .press("ArrowRight");
  await expect(
    page.getByRole("button", { name: "字节 27，16", exact: true }),
  ).toBeFocused();
  await page.getByRole("button", { name: "Ethernet II", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Ethernet II", exact: true }),
  ).toHaveAttribute("aria-expanded", "false");
  await page
    .getByRole("button", { name: "Ethernet II", exact: true })
    .press("ArrowRight");
  await expect(
    page.getByRole("button", { name: "Ethernet II", exact: true }),
  ).toHaveAttribute("aria-expanded", "true");
  await connection(page).focus();
  await page.keyboard.press("Escape");
  await connection(page).focus();
  await page.keyboard.press("Enter");
  await expect(connection(page)).toHaveClass(/selected/);
  await page.keyboard.press("Escape");
  await expect(connection(page)).not.toHaveClass(/selected/);
  await page
    .getByRole("region", { name: "资产通信图" })
    .locator('.react-flow__node[data-id="web"]')
    .press("Enter");
  await expect
    .poll(() => packetCalls(requests).at(-1)?.url.searchParams.get("assetId"))
    .toBe("web");
  const assetQuery = packetCalls(requests).at(-1)!.url.searchParams;
  expect(assetQuery.get("networkId")).toBeNull();
  expect(assetQuery.get("filter")).toBeNull();
});

test("风险在同一工作台筛选、分页并直接定位关联报文", async ({ page }) => {
  const { state, requests } = await fixture(page);
  const payload = Buffer.from("GET / HTTP/1.1\r\nHost: lab.example\r\n\r\n");
  state.packets[1] = {
    ...frame(2, false),
    protocol: "HTTP",
    transportProtocol: "TCP",
    length: 54 + payload.length,
    capturedLength: 54 + payload.length,
    summary: "GET / HTTP/1.1 · Host: lab.example",
  };
  state.olderAlerts = [
    { ...alert, id: 99, signature: "ET WEB_SERVER Earlier request" },
  ];
  state.packetReply = (url) =>
    url.searchParams.get("filter")?.includes("tcp.port")
      ? state.packets.filter(
          (packet) => (packet.transportProtocol ?? packet.protocol) === "TCP",
        )
      : state.packets;
  await open(page);
  await expect(
    page
      .getByRole("row")
      .filter({ hasText: "GET / HTTP/1.1 ·" })
      .getByLabel("关联高风险"),
  ).toBeVisible();
  await page.getByRole("button", { name: "高风险", exact: true }).click();
  await expect(page.getByRole("table", { name: "告警记录" })).toBeVisible();
  await expect(page.getByRole("region", { name: "资产通信图" })).toBeVisible();
  await page.getByRole("button", { name: "更早告警", exact: true }).click();
  await expect(
    page.getByRole("row").filter({ hasText: "ET WEB_SERVER Earlier request" }),
  ).toBeVisible();
  await page
    .getByRole("row")
    .filter({ hasText: "ET WEB_SERVER Suspicious request" })
    .press("Enter");
  await expect(page.getByRole("grid", { name: "报文列表" })).toBeVisible();
  await expect
    .poll(() => packetCalls(requests).at(-1)?.url.searchParams.get("filter"))
    .toContain("tcp.port == 50000");
  const query = packetCalls(requests).at(-1)!.url.searchParams;
  expect(query.get("filter")).toContain("tcp.port == 80");
  expect(query.get("assetId")).toBeNull();
  expect(query.get("interfaceId")).toBe("nic-win7");
  expect(query.get("networkId")).toBe("internal");
  expect(query.get("start")).toBe(alert.firstSeen);
  await expect(page.getByLabel("报文协议层")).toContainText(alert.signature);
  await expect(
    page.getByRole("button", { name: "通信分析", exact: true }),
  ).toHaveCount(0);
});

test("读取更早报文暂停实时头页，继续实时清分页，修改过滤器清旧帧", async ({
  page,
}) => {
  const { state, requests } = await fixture(page);
  state.packets = Array.from({ length: 100 }, (_, index) => frame(index + 21));
  state.older = Array.from({ length: 20 }, (_, index) => frame(index + 1));
  state.nextCursor = "before-21";
  await page.clock.install();
  await open(page);
  await page.getByRole("button", { name: "更早报文", exact: true }).click();
  await expect(
    page.getByRole("grid", { name: "报文列表" }).getByRole("row"),
  ).toHaveCount(121);
  await expect(
    page.getByRole("button", { name: "继续实时", exact: true }),
  ).toBeVisible();
  const before = packetCalls(requests).length;
  state.packets = Array.from({ length: 100 }, (_, index) => frame(index + 121));
  state.nextCursor = undefined;
  await page.clock.fastForward(10000);
  expect(packetCalls(requests)).toHaveLength(before);
  await expect(
    page.getByRole("row").filter({ hasText: "TCP frame 21 ·" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "继续实时", exact: true }).click();
  await page.clock.fastForward(3500);
  await expect(
    page.getByRole("row").filter({ hasText: "TCP frame 121 ·" }),
  ).toBeVisible();
  await expect(
    page.getByRole("grid", { name: "报文列表" }).getByRole("row"),
  ).toHaveCount(101);
  const selected = page.getByRole("row").filter({ hasText: "TCP frame 121 ·" });
  await selected.click();
  await expect(page.getByLabel("报文协议层")).toContainText("Frame 121:");
  state.packetReply = () => [frame(240, true)];
  await page
    .getByRole("textbox", { name: "报文过滤器" })
    .fill("udp.port == 53");
  await page.clock.fastForward(500);
  await expect(page.getByLabel("报文协议层")).toContainText("选择报文");
  await expect(page.getByLabel("报文协议层")).not.toContainText("Frame 121:");
  await page.getByRole("row").filter({ hasText: "UDP frame 240 ·" }).click();
  await expect(page.getByLabel("报文协议层")).toContainText("Frame 240:");
  expect(
    packetCalls(requests).at(-1)!.url.searchParams.get("filter"),
  ).toContain("udp.port == 53");
});

test("流量刷新保留视角，只呈现环境对象，广播组播归入交换机", async ({
  page,
}) => {
  const { state } = await fixture(page);
  await page.clock.install();
  await open(page);
  const graph = page.getByRole("region", { name: "资产通信图" });
  await expect(
    graph.getByRole("group", { name: "Entry LAN", exact: true }),
  ).toHaveCount(1);
  await expect(
    graph.getByRole("group", { name: "Internal LAN", exact: true }),
  ).toHaveCount(1);
  await expect(
    graph.getByRole("group", { name: "ff02::fb", exact: true }),
  ).toHaveCount(0);
  await expect(
    graph.getByRole("group", { name: "02:00:00:00:00:04", exact: true }),
  ).toHaveCount(0);
  await page
    .locator("[data-traffic-workspace]")
    .getByRole("button", { name: "放大通信图" })
    .click();
  const box = (await graph.boundingBox())!;
  await page.mouse.move(box.x + 35, box.y + box.height - 45);
  await page.mouse.down();
  await page.mouse.move(box.x + 78, box.y + box.height - 70, { steps: 4 });
  await page.mouse.up();
  const viewport = await graph
    .locator(".react-flow__viewport")
    .getAttribute("style");
  const positions = await graph
    .locator(".react-flow__node")
    .evaluateAll((nodes) =>
      Object.fromEntries(
        nodes.map((node) => [
          node.getAttribute("data-id"),
          (node as HTMLElement).style.transform,
        ]),
      ),
    );
  expect(new Set(Object.values(positions)).size).toBe(
    Object.keys(positions).length,
  );
  const rectangles = await graph
    .locator(".react-flow__node")
    .evaluateAll((nodes) =>
      nodes.map((node) => ({
        id: node.getAttribute("data-id"),
        ...node.getBoundingClientRect().toJSON(),
      })),
    );
  const overlaps: string[] = [];
  for (let i = 0; i < rectangles.length; i++)
    for (let j = i + 1; j < rectangles.length; j++) {
      const a = rectangles[i],
        b = rectangles[j];
      if (
        assets.some(
          (asset) =>
            (asset.id === a.id && asset.interfaces[0].networkId === b.id) ||
            (asset.id === b.id && asset.interfaces[0].networkId === a.id),
        )
      )
        continue;
      if (
        a.left < b.right &&
        a.right > b.left &&
        a.top < b.bottom &&
        a.bottom > b.top
      )
        overlaps.push(`${a.id}/${b.id}`);
    }
  expect(overlaps).toEqual([]);
  state.flows = [
    ...state.flows,
    flow("172.22.1.18", "203.0.113.88", {
      sourceAssetId: "oa",
      networkId: "internal",
    }),
  ];
  await page.clock.fastForward(5500);
  await expect(
    graph.getByRole("group", { name: "203.0.113.88", exact: true }),
  ).toHaveCount(0);
  await expect(graph.locator(".react-flow__viewport")).toHaveAttribute(
    "style",
    viewport!,
  );
  const after = await graph
    .locator(".react-flow__node")
    .evaluateAll((nodes) =>
      Object.fromEntries(
        nodes.map((node) => [
          node.getAttribute("data-id"),
          (node as HTMLElement).style.transform,
        ]),
      ),
    );
  for (const id of Object.keys(positions))
    expect(after[id]).toBe(positions[id]);
  expect(new Set(Object.values(after)).size).toBe(Object.keys(after).length);
  await page
    .locator("[data-traffic-workspace]")
    .getByRole("button", { name: "适应画布", exact: true })
    .click();
  const request = page.waitForRequest((request) =>
    new URL(request.url()).pathname.endsWith("/packets"),
  );
  await graph
    .getByRole("group", { name: "Entry LAN", exact: true })
    .locator("header")
    .click();
  await page.clock.fastForward(500);
  const query = new URL((await request).url()).searchParams;
  expect(query.get("networkId")).toBe("entry");
  expect(query.get("filter")).toBeNull();
});

test("全屏覆盖整个分析台且菜单、协议选择和对话框仍可用", async ({
  page,
}, info) => {
  await fixture(page);
  await open(page);
  await page.getByRole("button", { name: "全屏分析", exact: true }).click();
  await expect
    .poll(() =>
      page.evaluate(() =>
        document.fullscreenElement?.hasAttribute("data-traffic-workspace"),
      ),
    )
    .toBe(true);
  await page.getByRole("textbox", { name: "协议筛选", exact: true }).click();
  await expect(
    page.getByRole("option", { name: "UDP", exact: true }),
  ).toBeVisible();
  await page.getByRole("option", { name: "UDP", exact: true }).click();
  await page.getByRole("button", { name: "分析选项", exact: true }).click();
  await page.getByRole("menuitem", { name: "已保存抓包", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "已保存抓包", exact: true });
  await expect(dialog).toBeVisible();
  expect(
    await dialog.evaluate((element) =>
      document.fullscreenElement?.contains(element),
    ),
  ).toBe(true);
  await expect(
    dialog.getByRole("link", { name: "下载抓包", exact: true }),
  ).toBeVisible();
  await page.screenshot({ path: info.outputPath("traffic-fullscreen.png") });
  await dialog.locator(".mantine-Modal-close").click();
  await page.getByRole("button", { name: "退出全屏", exact: true }).click();
  await expect
    .poll(() => page.evaluate(() => document.fullscreenElement))
    .toBeNull();
});

test("四窗格、时间轴和分隔条适配桌面、窄屏与日夜主题", async ({
  page,
}, info) => {
  test.setTimeout(60000);
  await fixture(page);
  await open(page);
  await page
    .getByRole("grid", { name: "报文列表" })
    .getByRole("row")
    .nth(2)
    .click();
  await expect(page.getByLabel("报文协议层")).toContainText("Frame 2:");
  const column = page.getByRole("separator", {
    name: "调整分析栏宽",
    exact: true,
  });
  await column.focus();
  await column.press("ArrowRight");
  await expect(column).toHaveAttribute("aria-valuenow", "44");
  await page
    .getByRole("separator", { name: "调整报文详情高度", exact: true })
    .press("ArrowUp");
  for (const theme of ["light", "dark"]) {
    if (theme === "dark")
      await page
        .getByRole("button", { name: "切换夜间主题", exact: true })
        .click();
    for (const width of [1366, 1920, 2560, 390]) {
      await page.setViewportSize({
        width,
        height: width === 1366 ? 768 : width === 390 ? 844 : 1080,
      });
      await open(page);
      const timeline = page.getByRole("region", { name: "环境流量时间轴" });
      if (width === 1366 || width === 390)
        await expect(timeline).toHaveCount(0);
      else await expect(timeline).toBeVisible();
      if (width === 390)
        await page.getByRole("button", { name: "通信图", exact: true }).click();
      const graph = page.getByRole("region", { name: "资产通信图" });
      await expect(graph).toBeVisible();
      await expect(
        graph.locator('.react-flow__node:has([data-kind="asset"])'),
      ).toHaveCount(assets.length);
      await expect
        .poll(() =>
          graph.locator(".react-flow").evaluate((canvas) => {
            const frame = canvas.getBoundingClientRect();
            return Array.from(
              canvas.querySelectorAll(
                '.react-flow__node:has([data-kind="asset"])',
              ),
            )
              .filter((node) => {
                const rect = node.getBoundingClientRect();
                return (
                  rect.left < frame.left - 1 ||
                  rect.right > frame.right + 1 ||
                  rect.top < frame.top - 1 ||
                  rect.bottom > frame.bottom + 1
                );
              })
              .map((node) => node.getAttribute("data-id"));
          }),
        )
        .toEqual([]);
      const labelConflicts = await graph
        .locator(".react-flow")
        .evaluate((canvas) => {
          const labels = Array.from(
            canvas.querySelectorAll(".react-flow__node strong"),
          ).map((label) => ({
            name: label.getAttribute("title"),
            rect: label.getBoundingClientRect(),
          }));
          const tools = canvas
            .closest("[data-traffic-workspace]")!
            .querySelector('[class*="graphTools"]')!
            .getBoundingClientRect();
          const intersects = (a: DOMRect, b: DOMRect) =>
            a.left < b.right &&
            a.right > b.left &&
            a.top < b.bottom &&
            a.bottom > b.top;
          const conflicts: string[] = [];
          for (let i = 0; i < labels.length; i++) {
            if (intersects(labels[i].rect, tools))
              conflicts.push(`${labels[i].name}/tools`);
            for (let j = i + 1; j < labels.length; j++)
              if (intersects(labels[i].rect, labels[j].rect))
                conflicts.push(`${labels[i].name}/${labels[j].name}`);
          }
          return conflicts;
        });
      expect(labelConflicts).toEqual([]);
      if (width === 390)
        await page
          .getByRole("button", { name: "报文", exact: true })
          .first()
          .click();
      await page
        .getByRole("grid", { name: "报文列表" })
        .getByRole("row")
        .nth(2)
        .click();
      await expect(page.getByLabel("报文协议层")).toContainText("Frame 2:");
      await expect(page.getByRole("grid", { name: "报文列表" })).toBeVisible();
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= innerWidth,
        ),
      ).toBe(true);
      const panes = await page.locator('[class*="panes"]').boundingBox();
      const packetList = await page
        .getByRole("grid", { name: "报文列表" })
        .locator("..")
        .boundingBox();
      const protocols = await page.getByLabel("报文协议层").boundingBox();
      expect(protocols!.height).toBeGreaterThanOrEqual(100);
      expect(packetList!.height).toBeGreaterThanOrEqual(160);
      expect(packetList!.y).toBeLessThan(protocols!.y);
      expect(panes!.y + panes!.height).toBeLessThanOrEqual(
        (await page.viewportSize())!.height,
      );
      await page.screenshot({
        path: info.outputPath(`traffic-${width}-${theme}.png`),
      });
      if (width === 1366 || width === 390) {
        await page
          .getByRole("button", { name: "流量趋势", exact: true })
          .click();
        await expect(timeline).toBeVisible();
        await expect(
          page.getByRole("textbox", { name: "趋势范围", exact: true }),
        ).toBeVisible();
        await page
          .getByRole("button", { name: "流量趋势", exact: true })
          .click();
        await expect(timeline).toHaveCount(0);
        await expect(page.getByLabel("报文协议层")).toContainText("Frame 2:");
      }
      if (width === 390) {
        const divider = page.getByRole("separator", {
          name: "调整报文详情高度",
          exact: true,
        });
        await divider.press("ArrowUp");
        await expect
          .poll(
            async () =>
              (await page.getByLabel("报文协议层").boundingBox())!.height,
          )
          .toBeGreaterThan(protocols!.height);
        await divider.press("ArrowDown");
        await page.getByRole("button", { name: "通信图", exact: true }).click();
        await expect(
          page.getByRole("region", { name: "资产通信图" }),
        ).toBeVisible();
        await page.screenshot({
          path: info.outputPath(`traffic-${width}-${theme}-graph.png`),
        });
        await page
          .getByRole("button", { name: "报文", exact: true })
          .first()
          .click();
      } else {
        await expect(page.getByLabel("报文字节")).toBeVisible();
      }
    }
  }
});
test("无流量资产不显示，密集连线绕开设备与名称，保留真实环境对象", async ({
  page,
}, info) => {
  test.setTimeout(60000);
  const scene: EnvironmentSpec = {
    networks: [
      { id: "dense", name: "海上风电场生产控制网络", cidr: "10.20.0.0/24" },
    ],
    routers: [],
    assets: Array.from({ length: 33 }, (_, i) => ({
      id: "device-" + i,
      name: i % 3 ? "风电控制设备 " + i : "工业协议转换与通信控制设备 " + i,
      templateId: "linux",
      resources,
      interfaces: [
        {
          id: "nic-" + i,
          networkId: "dense",
          address: "10.20.0." + (i + 1),
          mac: "02:00:00:00:00:" + (i + 1).toString(16).padStart(2, "0"),
          primary: true,
        },
      ],
    })),
  };
  const { state } = await fixture(page, scene);
  state.alerts = [];
  state.packets = [];
  state.flows = scene.assets.slice(1, 32).flatMap((asset, i) => [
    flow(scene.assets[0].interfaces[0].address, asset.interfaces[0].address, {
      sourceAssetId: "device-0",
      destinationAssetId: asset.id,
      networkId: "dense",
    }),
    ...(i > 1
      ? [
          flow(
            scene.assets[i - 1].interfaces[0].address,
            asset.interfaces[0].address,
            {
              sourceAssetId: scene.assets[i - 1].id,
              destinationAssetId: asset.id,
              networkId: "dense",
            },
          ),
        ]
      : []),
  ]);
  await page.setViewportSize({ width: 1920, height: 1080 });
  await page.goto("/environments/env?view=traffic");
  const graph = page.getByRole("region", { name: "资产通信图" });
  await expect(graph.locator(".react-flow__node-trafficAsset")).toHaveCount(32);
  await expect(
    graph.locator('.react-flow__node[data-id="device-32"]'),
  ).toHaveCount(0);
  await expect(graph.locator(".react-flow__edge")).toHaveCount(
    state.flows.length,
  );
  await expect(graph.locator("[data-smart-edge-placeholder]")).toHaveCount(0);
  await expect
    .poll(
      () =>
        graph.evaluate((element) => {
          const nodes = Array.from(
            element.querySelectorAll(".react-flow__node-trafficAsset"),
          ).map((node) => ({
            id: node.getAttribute("data-id"),
            rect: node.getBoundingClientRect(),
          }));
          const labels = Array.from(
            element.querySelectorAll(".react-flow__node strong"),
          ).map((node) => node.getBoundingClientRect());
          const problems: string[] = [];
          for (const edge of element.querySelectorAll(".react-flow__edge")) {
            const [source, target] = JSON.parse(edge.getAttribute("data-id")!);
            const path = edge.querySelector<SVGPathElement>(
              ".react-flow__edge-path",
            )!;
            const length = path.getTotalLength(),
              matrix = path.getScreenCTM()!;
            const obstacles = [
              ...nodes
                .filter((node) => node.id !== source && node.id !== target)
                .map((node) => node.rect),
              ...labels,
            ];
            for (let distance = 2; distance < length; distance += 3) {
              const point = path.getPointAtLength(distance);
              const screen = new DOMPoint(point.x, point.y).matrixTransform(
                matrix,
              );
              if (
                obstacles.some(
                  (rect) =>
                    screen.x > rect.left + 1 &&
                    screen.x < rect.right - 1 &&
                    screen.y > rect.top + 1 &&
                    screen.y < rect.bottom - 1,
                )
              ) {
                const hit = obstacles.find(
                  (rect) =>
                    screen.x > rect.left + 1 &&
                    screen.x < rect.right - 1 &&
                    screen.y > rect.top + 1 &&
                    screen.y < rect.bottom - 1,
                )!;
                const obstacle =
                  nodes.find((node) => node.rect === hit)?.id ?? "label";
                problems.push(source + "/" + target + " crosses " + obstacle);
                break;
              }
            }
          }
          return problems;
        }),
      { timeout: 15000 },
    )
    .toEqual([]);
  const clipped = await graph
    .locator(".react-flow__node strong")
    .evaluateAll((labels) =>
      labels
        .filter(
          (label) =>
            (label as HTMLElement).scrollWidth >
            (label as HTMLElement).clientWidth + 1,
        )
        .map((label) => label.textContent),
    );
  expect(clipped).toEqual([]);
  await page.screenshot({ path: info.outputPath("traffic-dense.png") });
});
