import { expect, test, type Page } from "@playwright/test";
import { createPrivateKey, createPublicKey } from "node:crypto";
import { readFile } from "node:fs/promises";
import type { Environment, Operation, Schema } from "../src/api/client";

async function fixture(
  page: Page,
  options: {
    assetOnly?: boolean;
    existing?: boolean;
    rejectCreate?: boolean;
  } = {},
) {
  const spec = {
    networks: [
      { id: "lan", name: "应用网段", cidr: "10.10.0.0/24" },
      { id: "lan6", name: "IPv6 网段", cidr: "fd10::/64" },
    ],
    assets: [],
  };
  const environment: Environment = {
    id: "env",
    projectId: "project",
    name: "VPN 环境",
    revision: 3,
    status: "running",
    permissions: options.assetOnly ? ["read"] : ["read", "access"],
    assetPermissions: options.assetOnly ? { web: ["access"] } : {},
    spec,
    appliedSpec: spec,
    view: { positions: {} },
    createdAt: "2026-10-03T08:00:00Z",
    updatedAt: "2026-10-03T08:00:00Z",
  };
  const operation: Operation = {
    id: "vpn-task",
    environmentId: "env",
    kind: "vpn-create",
    state: options.existing ? "succeeded" : "queued",
    phase: options.existing ? "complete" : "queued",
    completed: options.existing ? 1 : 0,
    total: 1,
    retryable: false,
    createdAt: environment.createdAt,
    updatedAt: environment.updatedAt,
  };
  const access: Schema<"VPNAccess"> = {
    id: "peer",
    name: "办公电脑",
    publicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
    mode: "translated",
    routes: [
      { networkId: "lan", cidr: "10.10.0.0/24", accessCidr: "172.28.0.0/24" },
      { networkId: "lan6", cidr: "fd10::/64", accessCidr: "fd70::/64" },
    ],
    addresses: ["10.250.0.2/32", "fd77::2/128"],
    state: options.existing ? "active" : "pending",
    operationId: operation.id,
    createdAt: environment.createdAt,
  };
  let peers = options.existing ? [access] : [];
  let operations = options.existing ? [operation] : [];
  const calls: {
    method: string;
    path: string;
    query: URLSearchParams;
    body: unknown;
  }[] = [];
  await page.addInitScript(() => {
    const streams: EventTarget[] = [];
    Object.assign(window, { testStreams: streams });
    class Events extends EventTarget {
      onopen: ((event: Event) => void) | null = null;
      constructor() {
        super();
        streams.push(this);
        setTimeout(() => this.onopen?.(new Event("open")), 0);
      }
      close() {
        streams.splice(streams.indexOf(this), 1);
      }
    }
    Object.assign(window, { EventSource: Events });
  });
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname.replace("/api/v1", "");
    const body = request.postDataJSON();
    calls.push({
      method: request.method(),
      path,
      query: url.searchParams,
      body,
    });
    let response: unknown = [];
    let status = 200;
    if (path === "/identity")
      response = { id: "user", name: "operator", administrator: false };
    else if (path === "/environments/env") response = environment;
    else if (path === "/environments/env/state")
      response = {
        id: "env",
        revision: environment.revision,
        status: "running",
        operation: operations[0],
        assets: [],
        updatedAt: environment.updatedAt,
      };
    else if (path === "/operations") response = operations;
    else if (
      path === "/environments/env/vpn-access" &&
      request.method() === "POST"
    ) {
      if (options.rejectCreate) {
        response = { detail: "环境已有任务正在执行" };
        status = 409;
      } else {
        Object.assign(access, body);
        peers = [access];
        operations = [operation];
        response = operation;
        status = 202;
      }
    } else if (path === "/environments/env/vpn-access") response = peers;
    else if (
      path === "/environments/env/vpn-access/peer" &&
      request.method() === "DELETE"
    ) {
      access.state = "revoking";
      Object.assign(operation, {
        id: "revoke-task",
        kind: "vpn-revoke",
        state: "queued",
        phase: "queued",
        completed: 0,
      });
      response = operation;
      status = 202;
    } else if (path === "/environments/env/vpn-access/peer/connection")
      response = {
        endpoint: "lab.example.test:51820",
        publicKey: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA=",
        addresses: access.addresses,
        allowedIPs: access.routes.map((item) => item.accessCidr),
        mtu: 1380,
      };
    await route.fulfill({ status, json: response });
  });
  const update = async (
    state: Operation["state"],
    accessState: Schema<"VPNAccess">["state"],
    removed = false,
  ) => {
    Object.assign(operation, {
      state,
      phase:
        state === "succeeded"
          ? "complete"
          : state === "failed"
            ? "failed"
            : "vpn",
      completed: state === "succeeded" ? 1 : 0,
    });
    Object.assign(access, {
      state: accessState,
      error: state === "failed" ? "节点 VPN 配置失败" : undefined,
    });
    if (removed) peers = [];
    if (state === "succeeded") environment.revision++;
    await page.evaluate(
      (type) => {
        for (const stream of (
          window as unknown as { testStreams: EventTarget[] }
        ).testStreams)
          stream.dispatchEvent(new MessageEvent(type));
      },
      state === "running" ? "operation.progress" : `operation.${state}`,
    );
  };
  return { calls, access, operation, update };
}

async function create(page: Page) {
  await page.getByRole("button", { name: "VPN", exact: true }).click();
  await page.getByRole("button", { name: "新建连接" }).click();
  await page.getByLabel("连接名称").fill("办公电脑");
  await page.getByText("独立访问地址", { exact: true }).click();
  await page.getByRole("button", { name: "创建连接", exact: true }).click();
  await expect(page.getByText("等待配置", { exact: true })).toBeVisible();
}

test("VPN uses environment-level access, not an asset grant", async ({
  page,
}) => {
  const { calls } = await fixture(page, { assetOnly: true });
  await page.goto("/environments/env");
  await expect(page.getByRole("heading", { name: "VPN 环境" })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "VPN", exact: true }),
  ).toHaveCount(0);
  expect(calls.some((item) => item.path.includes("vpn-access"))).toBe(false);
});

test("VPN download waits for applied task and contains a matching private key and dual-stack routes", async ({
  page,
}) => {
  const { calls, update } = await fixture(page);
  await page.setViewportSize({ width: 1366, height: 900 });
  await page.goto("/environments/env");
  await create(page);
  const body = calls.find((item) => item.method === "POST")!
    .body as Schema<"CreateVPNAccess">;
  expect(Object.keys(body).sort()).toEqual([
    "clientRequestId",
    "expectedRevision",
    "mode",
    "name",
    "networkIds",
    "publicKey",
  ]);
  expect(body).toMatchObject({
    name: "办公电脑",
    mode: "translated",
    networkIds: ["lan", "lan6"],
    expectedRevision: 3,
  });
  expect(Buffer.from(body.publicKey, "base64")).toHaveLength(32);
  await expect(
    page.getByRole("button", { name: "下载 WireGuard 配置" }),
  ).toHaveCount(0);
  await update("running", "active");
  await expect(page.getByText("可用", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "下载 WireGuard 配置" }),
  ).toHaveCount(0);
  await update("succeeded", "active");
  await expect(
    page.getByRole("button", { name: "下载 WireGuard 配置" }),
  ).toBeVisible();
  await page.getByText("应用网段 · IPv6 网段", { exact: true }).click();
  await expect(
    page.getByText("10.10.0.0/24 → 172.28.0.0/24", { exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: "D:/.cache/netlab/artifacts/vpn-web-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "D:/.cache/netlab/artifacts/vpn-web-mobile.png",
  });
  await page.setViewportSize({ width: 1366, height: 900 });
  const downloaded = page.waitForEvent("download");
  await page.getByRole("button", { name: "下载 WireGuard 配置" }).click();
  const download = await downloaded;
  expect(download.suggestedFilename()).toBe("WireGuard.conf");
  const config = await readFile((await download.path())!, "utf8");
  expect(config).toContain("Address = 10.250.0.2/32, fd77::2/128");
  expect(config).toContain("AllowedIPs = 172.28.0.0/24, fd70::/64");
  expect(config).toContain("Endpoint = lab.example.test:51820");
  expect(config).toContain("MTU = 1380");
  const privateKey = config.match(/PrivateKey = (.+)/)![1];
  const pair = createPrivateKey({
    key: Buffer.concat([
      Buffer.from("302e020100300506032b656e04220420", "hex"),
      Buffer.from(privateKey, "base64"),
    ]),
    format: "der",
    type: "pkcs8",
  });
  const publicKey = createPublicKey(pair).export({ format: "jwk" });
  expect(Buffer.from(publicKey.x!, "base64url").toString("base64")).toBe(
    body.publicKey,
  );
  expect(JSON.stringify(calls)).not.toContain(privateKey);
  expect(
    await page.evaluate(() => JSON.stringify({ ...localStorage })),
  ).not.toContain(privateKey);
  await expect(
    page.getByRole("button", { name: "下载 WireGuard 配置" }),
  ).toHaveCount(0);
  await page.reload();
  await page.getByRole("button", { name: "VPN", exact: true }).click();
  await expect(page.getByText("可用", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "下载 WireGuard 配置" }),
  ).toHaveCount(0);
});

test("existing VPN copies public connection data and revokes through an operation", async ({
  page,
  context,
}) => {
  const { calls, update } = await fixture(page, { existing: true });
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "VPN", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "下载 WireGuard 配置" }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: "办公电脑 操作" }).click();
  await page.getByRole("menuitem", { name: "复制连接参数" }).click();
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toContain("Endpoint = lab.example.test:51820");
  expect(
    await page.evaluate(() => navigator.clipboard.readText()),
  ).not.toContain("PrivateKey");
  await page.getByRole("button", { name: "办公电脑 操作" }).click();
  await page.getByRole("menuitem", { name: "撤销连接" }).click();
  await expect(page.getByText("撤销中", { exact: true })).toBeVisible();
  const revoke = calls.find((item) => item.method === "DELETE")!;
  expect(revoke.path).toBe("/environments/env/vpn-access/peer");
  expect(revoke.query.get("expectedRevision")).toBe("3");
  expect(revoke.query.get("clientRequestId")).toBeTruthy();
  await update("succeeded", "revoking", true);
  await expect(page.getByText("暂无 VPN 连接")).toBeVisible();
});

test("failed VPN remains failed without a downloadable configuration", async ({
  page,
}) => {
  const { update } = await fixture(page);
  await page.goto("/environments/env");
  await create(page);
  await update("failed", "failed");
  await expect(
    page.getByText("节点 VPN 配置失败", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "下载 WireGuard 配置" }),
  ).toHaveCount(0);
});

test("VPN creation errors retain the form and drawer fits a narrow screen", async ({
  page,
}) => {
  const { calls } = await fixture(page, { rejectCreate: true });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "VPN", exact: true }).click();
  await page.getByRole("button", { name: "新建连接" }).click();
  await page.getByLabel("连接名称").fill("办公电脑");
  await page.getByRole("button", { name: "创建连接", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("环境已有任务正在执行");
  await expect(page.getByLabel("连接名称")).toHaveValue("办公电脑");
  await expect(
    page.getByRole("button", { name: "下载 WireGuard 配置" }),
  ).toHaveCount(0);
  expect(
    calls.filter((item) => item.path.includes("/connection")),
  ).toHaveLength(0);
  const overflowing = await page
    .getByRole("dialog", { name: "VPN", exact: true })
    .evaluate((element) => element.scrollWidth > element.clientWidth);
  expect(overflowing).toBe(false);
});
