import { expect, test, type Page } from "@playwright/test";

const timestamp = "2026-10-02T08:00:00Z";
const resources = { cpu: 2, memoryMiB: 2048, diskGiB: 20 };
const emptySpec = { assets: [], networks: [] };
const blueprint = {
  id: "blueprint-later",
  projectId: "default",
  name: "后续页模板",
  latestVersionId: "version-101",
  latestVersion: 101,
  assetCount: 1,
  networkCount: 1,
  createdAt: timestamp,
  updatedAt: timestamp,
};
const template = {
  id: "old-vm",
  name: "旧 Windows 模板",
  kind: "vm",
  os: "Windows",
  version: 1,
  source: "https://example.test/windows.qcow2",
  state: "ready",
  resources,
  hardware: {
    firmware: "uefi",
    machine: "q35",
    nicModel: "virtio",
    diskBus: "virtio",
  },
};
const environment = {
  id: "env",
  projectId: "default",
  name: "分页环境",
  revision: 1,
  status: "running",
  createdAt: timestamp,
  updatedAt: timestamp,
  view: {},
  spec: {
    ...emptySpec,
    assets: [
      {
        id: "windows",
        name: "Windows 资产",
        templateId: "old-vm",
        resources,
        interfaces: [],
      },
    ],
  },
};

async function baseFixture(page: Page) {
  await page.route("**/api/v1/**", async (route) => {
    const path = new URL(route.request().url()).pathname.replace("/api/v1", "");
    const response =
      path === "/identity"
        ? { id: "user", name: "operator", administrator: true }
        : path === "/environments/env"
          ? environment
          : path === "/environments/env/state"
            ? {
                id: "env",
                revision: 1,
                status: "running",
                assets: [],
                updatedAt: timestamp,
              }
            : [];
    await route.fulfill({ json: response });
  });
}

function pageOf<T extends { id: string }>(items: T[], url: URL) {
  const cursor = url.searchParams.get("cursor");
  const start = cursor ? items.findIndex((item) => item.id === cursor) + 1 : 0;
  return items.slice(
    start,
    start + Number(url.searchParams.get("limit") ?? 100),
  );
}

test("environment summaries paginate, preserve a failed page, and filter on the server", async ({
  page,
}) => {
  await baseFixture(page);
  const requests: URL[] = [];
  const summaries = Array.from({ length: 103 }, (_, index) => ({
    id: `env-${index}`,
    projectId: "default",
    name: `环境 ${index}`,
    externalReference: `team-${index}`,
    revision: 1,
    status: index === 102 ? "stopped" : "running",
    assetCount: 20,
    networkCount: 3,
    createdAt: timestamp,
    updatedAt: timestamp,
  }));
  let rejectPage = true;
  await page.route("**/api/v1/environments?*", async (route) => {
    const url = new URL(route.request().url());
    requests.push(url);
    if (url.searchParams.has("cursor") && rejectPage) {
      await route.fulfill({ status: 503, json: { detail: "读取下一页失败" } });
      return;
    }
    const search = url.searchParams.get("search") ?? "";
    const status = url.searchParams.get("status");
    await route.fulfill({
      json: pageOf(
        summaries.filter(
          (item) =>
            (!search ||
              item.name.includes(search) ||
              item.externalReference.includes(search)) &&
            (!status || item.status === status),
        ),
        url,
      ),
    });
  });
  await page.goto("/environments");
  await expect(page.locator("tbody tr")).toHaveCount(100);
  await expect(page.locator("tbody tr").first()).toContainText("20 / 3");
  await page.getByRole("button", { name: "加载更多", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("读取下一页失败");
  await expect(page.locator("tbody tr")).toHaveCount(100);
  rejectPage = false;
  await page.getByRole("button", { name: "加载更多", exact: true }).click();
  await expect(page.locator("tbody tr")).toHaveCount(103);
  await page.getByRole("textbox", { name: "搜索环境" }).fill("team-102");
  await expect(page.locator("tbody tr")).toHaveCount(1);
  await page.getByRole("button", { name: "已停止", exact: true }).click();
  await expect
    .poll(() => requests.at(-1)?.searchParams.get("status"))
    .toBe("stopped");
  expect(requests.at(-1)?.searchParams.get("search")).toBe("team-102");
  expect(requests.at(-1)?.searchParams.has("cursor")).toBe(false);
  expect(
    requests.some((url) => url.searchParams.get("cursor") === "env-99"),
  ).toBe(true);
});

test("creation selects blueprints and versions beyond the first page", async ({
  page,
}) => {
  await baseFixture(page);
  const blueprints = Array.from({ length: 100 }, (_, index) => ({
    ...blueprint,
    id: `b-${index}`,
    name: `模板 ${index}`,
  })).concat(blueprint);
  const versions = Array.from({ length: 101 }, (_, index) => ({
    id: `version-${101 - index}`,
    blueprintId: blueprint.id,
    version: 101 - index,
    assetCount: 1,
    networkCount: 1,
    createdAt: timestamp,
  }));
  await page.route("**/api/v1/blueprints?*", (route) =>
    route.fulfill({ json: pageOf(blueprints, new URL(route.request().url())) }),
  );
  await page.route("**/api/v1/blueprints/blueprint-later/versions?*", (route) =>
    route.fulfill({ json: pageOf(versions, new URL(route.request().url())) }),
  );
  let created: unknown;
  await page.route("**/api/v1/environments", async (route) => {
    created = route.request().postDataJSON();
    await route.fulfill({ status: 201, json: environment });
  });
  await page.goto("/environments");
  await page
    .getByRole("button", { name: "新建环境", exact: true })
    .first()
    .click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: "加载更多", exact: true }).click();
  await dialog.getByRole("textbox", { name: "环境模板", exact: true }).click();
  await page.getByRole("option", { name: "后续页模板", exact: true }).click();
  await dialog.getByRole("button", { name: "加载更多", exact: true }).click();
  await dialog.getByRole("textbox", { name: "版本", exact: true }).click();
  await page.getByRole("option", { name: "v1", exact: true }).click();
  await dialog
    .getByRole("textbox", { name: "环境名称" })
    .fill("第一版独立环境");
  await dialog.getByRole("button", { name: "创建并运行", exact: true }).click();
  await expect(page.getByRole("heading", { name: "分页环境" })).toBeVisible();
  expect(created).toMatchObject({
    projectId: "default",
    blueprintVersionId: "version-1",
    run: true,
  });
});

test("referenced VM templates, asset selection and operation history read later pages", async ({
  page,
}) => {
  await baseFixture(page);
  const requests: URL[] = [];
  const catalog = Array.from({ length: 100 }, (_, index) => ({
    ...template,
    id: `container-${index}`,
    kind: "container",
    name: `容器模板 ${index}`,
  })).concat({ ...template, id: "new-vm", name: "后续页 VM" });
  await page.route("**/api/v1/templates?*", async (route) => {
    const url = new URL(route.request().url());
    requests.push(url);
    const search = url.searchParams.get("search") ?? "";
    await route.fulfill({
      json: url.searchParams.has("ids")
        ? [template]
        : pageOf(
            catalog.filter((item) => item.name.includes(search)),
            url,
          ),
    });
  });
  const operations = Array.from({ length: 101 }, (_, index) => ({
    id: `op-${index}`,
    environmentId: "env",
    kind: "deploy",
    state: "succeeded",
    phase: "completed",
    completed: 1,
    total: 1,
    createdAt: timestamp,
    updatedAt: timestamp,
  }));
  await page.route("**/api/v1/operations?*", (route) =>
    route.fulfill({ json: pageOf(operations, new URL(route.request().url())) }),
  );
  await page.goto("/environments/env");
  await page.getByRole("button", { name: "对象列表", exact: true }).click();
  const asset = page.getByRole("button", { name: "Windows 资产", exact: true });
  await expect(asset.locator("svg")).toHaveClass(/lucide-monitor/);
  expect(requests.some((url) => url.searchParams.get("ids") === "old-vm")).toBe(
    true,
  );
  await page.locator(".task-tray-toggle").click();
  await expect(page.locator(".task-list > button")).toHaveCount(100);
  await page
    .locator(".task-list")
    .getByRole("button", { name: "加载更多", exact: true })
    .click();
  await expect(page.locator(".task-list > button")).toHaveCount(101);
  await page.locator(".task-tray-toggle").click();
  await page.getByRole("button", { name: "调整环境", exact: true }).click();
  await page.getByRole("button", { name: "添加", exact: true }).click();
  await page.getByRole("menuitem", { name: "资产", exact: true }).click();
  const drawer = page.getByRole("dialog");
  await drawer.getByRole("button", { name: "加载更多", exact: true }).click();
  await drawer.getByRole("textbox", { name: "资产模板", exact: true }).click();
  await page
    .getByRole("option", { name: "后续页 VM · 虚拟机", exact: true })
    .click();
  await expect(
    drawer.getByRole("textbox", { name: "资产模板", exact: true }),
  ).toHaveValue("后续页 VM · 虚拟机");
  await expect(drawer.getByRole("textbox", { name: "资产名称" })).toHaveValue(
    "后续页 VM-1",
  );
});
