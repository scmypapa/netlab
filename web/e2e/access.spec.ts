import { expect, test, type Page } from "@playwright/test";

const permissions = [
  "read",
  "operate",
  "session",
  "file",
  "observe",
  "network",
  "access",
  "compose",
  "manage",
];
const roles = [
  { key: "viewer", name: "查看者", permissions: ["read"] },
  { key: "operator", name: "操作员", permissions: permissions.slice(0, 7) },
  { key: "manager", name: "管理员", permissions },
];
const time = "2026-10-02T08:00:00Z";
const admin = {
  id: "admin",
  name: "admin",
  kind: "user",
  administrator: true,
  disabled: false,
  createdAt: time,
};
const player = {
  id: "player",
  name: "测试用户",
  kind: "user",
  administrator: false,
  disabled: false,
  createdAt: time,
};
const environment = {
  id: "env",
  projectId: "default",
  name: "共享测试环境",
  revision: 1,
  status: "running",
  permissions,
  assetPermissions: { one: permissions },
  view: {},
  spec: {
    networks: [],
    assets: [
      {
        id: "one",
        name: "Web 服务",
        templateId: "template",
        resources: { cpu: 1, memoryMiB: 512, diskGiB: 1 },
        interfaces: [],
      },
    ],
  },
  createdAt: time,
  updatedAt: time,
};

async function fixture(page: Page) {
  const principals = [admin, player];
  const calls: { method: string; path: string; body: unknown }[] = [];
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname.replace("/api/v1", "");
    const body = request.postDataJSON();
    calls.push({ method: request.method(), path, body });
    let response: unknown = [];
    let status = 200;
    if (path === "/identity")
      response = {
        id: "admin",
        name: "admin",
        administrator: true,
        roles,
        grants: [],
      };
    else if (path === "/principals" && request.method() === "POST") {
      const created = { ...player, id: "new-user", name: body.name };
      principals.push(created);
      response = created;
      status = 201;
    } else if (path === "/principals")
      response = principals.filter(
        (principal) =>
          principal.kind === (url.searchParams.get("kind") ?? "user"),
      );
    else if (path.startsWith("/principals/") && request.method() === "PUT") {
      Object.assign(
        principals.find(
          (principal) => principal.id === path.split("/").at(-1),
        )!,
        { name: body.name, disabled: body.disabled },
      );
      status = 204;
    } else if (path === "/service-tokens") {
      const principal = {
        ...player,
        id: "service",
        name: body.name,
        kind: "token",
      };
      principals.push(principal);
      response = { principal, token: "fixture-once-token" };
      status = 201;
    } else if (path === "/service-tokens/service") {
      principals.find((principal) => principal.id === "service")!.disabled =
        true;
      status = 204;
    } else if (path === "/environments")
      response = [{ ...environment, assetCount: 1, networkCount: 0 }];
    else if (path === "/environments/env") response = environment;
    else if (path === "/environments/env/state")
      response = {
        id: "env",
        revision: 1,
        status: "running",
        assets: [],
        updatedAt: time,
      };
    else if (path === "/environments/env/grants") {
      if (request.method() === "PUT") status = 204;
      else
        response = {
          ownerId: "admin",
          grants: [],
          inherited: [],
          subjects: principals,
        };
    }
    await route.fulfill({
      status,
      contentType: "application/json",
      body: status === 204 ? undefined : JSON.stringify(response),
    });
  });
  return calls;
}

test("account management creates, edits and disables an ordinary user", async ({
  page,
}) => {
  const calls = await fixture(page);
  await page.goto("/accounts");
  await page.getByRole("button", { name: "新建用户", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("textbox", { name: "账号名称" }).fill("新用户");
  await dialog.getByLabel("密码").fill("fixture-password");
  await dialog.getByRole("button", { name: "保存", exact: true }).click();
  await expect(page.locator("tbody")).toContainText("新用户");
  expect(calls.find((call) => call.method === "POST")?.body).toEqual({
    name: "新用户",
    password: "fixture-password",
  });
  await page.getByRole("button", { name: "管理新用户", exact: true }).click();
  await page.getByRole("menuitem", { name: "编辑账号" }).click();
  await dialog.getByRole("textbox", { name: "账号名称" }).fill("编辑用户");
  await dialog.getByLabel("重置密码", { exact: true }).fill("updated-password");
  await dialog.getByRole("button", { name: "保存", exact: true }).click();
  await expect(page.locator("tbody")).toContainText("编辑用户");
  await page.getByRole("button", { name: "管理编辑用户", exact: true }).click();
  await page.getByRole("menuitem", { name: "停用", exact: true }).click();
  await expect(
    page.locator("tbody tr").filter({ hasText: "编辑用户" }),
  ).toContainText("已停用");
  expect(
    calls.filter((call) => call.method === "PUT").map((call) => call.body),
  ).toEqual([
    { name: "编辑用户", disabled: false, password: "updated-password" },
    { name: "编辑用户", disabled: true },
  ]);
});

test("service token uses a scoped role with file permission removed, then revokes", async ({
  page,
}) => {
  const calls = await fixture(page);
  await page.goto("/accounts");
  await page.getByRole("button", { name: "服务 Token", exact: true }).click();
  await page.getByRole("button", { name: "签发 Token", exact: true }).click();
  const drawer = page.getByRole("dialog");
  await drawer
    .getByRole("textbox", { name: "Token 名称", exact: true })
    .fill("自动化服务");
  await drawer.getByRole("textbox", { name: "授权环境", exact: true }).click();
  await page.getByRole("option", { name: "共享测试环境", exact: true }).click();
  await drawer.getByRole("textbox", { name: "角色", exact: true }).click();
  await page.getByRole("option", { name: "操作员", exact: true }).click();
  await drawer.getByRole("button", { name: "细项权限", exact: true }).click();
  await drawer
    .getByRole("checkbox", { name: "文件传输", exact: true })
    .uncheck();
  await drawer.getByRole("button", { name: "签发", exact: true }).click();
  await expect(drawer.locator("code")).toHaveText("fixture-once-token");
  const issued = calls.find((call) => call.path === "/service-tokens")?.body;
  expect(issued).toEqual({
    name: "自动化服务",
    grants: [
      {
        scopeKind: "environment",
        scopeId: "env",
        permissions: roles[1].permissions.filter(
          (permission) => permission !== "file",
        ),
      },
    ],
  });
  await drawer.getByRole("button", { name: "完成", exact: true }).click();
  await expect(page.getByText("fixture-once-token")).not.toBeVisible();
  await page
    .getByRole("button", { name: "管理自动化服务", exact: true })
    .click();
  await page.getByRole("menuitem", { name: "撤销 Token", exact: true }).click();
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "撤销", exact: true })
    .click();
  await expect(
    page.locator("tbody tr").filter({ hasText: "自动化服务" }),
  ).toContainText("已撤销");
  expect(
    calls.some(
      (call) =>
        call.method === "DELETE" && call.path === "/service-tokens/service",
    ),
  ).toBe(true);
});

test("environment sharing applies a role to selected assets without file access", async ({
  page,
}) => {
  const calls = await fixture(page);
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "共享环境", exact: true }).click();
  const drawer = page.getByRole("dialog");
  await drawer.getByRole("textbox", { name: "添加成员", exact: true }).click();
  await page.getByRole("option", { name: "测试用户", exact: true }).click();
  await drawer.getByRole("button", { name: "添加成员", exact: true }).click();
  await drawer.getByRole("textbox", { name: "角色", exact: true }).click();
  await page.getByRole("option", { name: "操作员", exact: true }).click();
  await drawer.getByRole("button", { name: "细项权限", exact: true }).click();
  await drawer
    .getByRole("checkbox", { name: "文件传输", exact: true })
    .uncheck();
  await drawer.getByRole("textbox", { name: "授权资产", exact: true }).click();
  await page.getByRole("option", { name: "Web 服务", exact: true }).click();
  await page.setViewportSize({ width: 390, height: 900 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await drawer.getByRole("button", { name: "保存授权", exact: true }).click();
  expect(
    calls.find((call) => call.path.endsWith("/grants") && call.method === "PUT")
      ?.body,
  ).toEqual([
    {
      principalId: "player",
      permissions: roles[1].permissions.filter(
        (permission) => permission !== "file",
      ),
      assetIds: ["one"],
    },
  ]);
  await expect(drawer).not.toBeVisible();
});
