import { expect, test, type Page } from "@playwright/test";

const stamp = "2026-10-23T08:00:00Z";
const resources = { cpu: 4, memoryMiB: 4096, diskGiB: 64 };
const nodes = ["计算节点 A", "计算节点 B"].map((name, index) => ({
  id: `node-${index}`,
  name,
  endpoint: `https://node-${index}.example:9443`,
  state: "ready",
  capacity: { cpu: 32, memoryMiB: 131072, diskGiB: 2048 },
  reserved: { cpu: 12, memoryMiB: 32768, diskGiB: 256 },
  capabilities: ["vm", "container"],
  slots: 8,
  observedAt: stamp,
}));
const templates = [
  {
    id: "windows",
    name: "Windows Server 2022",
    kind: "vm",
    os: "Windows Server",
    format: "qcow2",
    source: "/data/images/server2022.qcow2",
    version: 2,
    state: "ready",
    initialization: "cloudbase-init",
    resources,
    hardware: {
      machine: "pc-q35-10.0",
      firmware: "uefi",
      diskBus: "scsi",
      diskController: "virtio-scsi",
      nicModel: "virtio",
      tpm: true,
    },
  },
  {
    id: "linux",
    name: "Ubuntu 24.04 LTS",
    kind: "vm",
    os: "Linux",
    format: "qcow2",
    source: "/data/images/ubuntu.qcow2",
    version: 1,
    state: "ready",
    resources,
  },
  {
    id: "nginx",
    name: "Nginx 服务",
    kind: "container",
    os: "Linux",
    format: "oci",
    source: "docker.io/library/nginx:stable",
    version: 3,
    state: "ready",
    resources: { cpu: 1, memoryMiB: 512, diskGiB: 4 },
  },
];
const pools = [
  {
    id: "shared",
    name: "共享存储",
    driver: "rbd",
    managed: true,
    default: false,
    nodeIds: nodes.map((node) => node.id),
    state: "ready",
    operationState: "succeeded",
    allocatedGiB: 256,
    capabilities: ["volumes"],
    storage: {
      path: "rbd:netlab",
      filesystem: "ceph",
      capacityBytes: 2 * 2 ** 40,
      availableBytes: 1.5 * 2 ** 40,
      nativeSnapshots: true,
    },
  },
  {
    id: "default:node-0",
    name: "本地存储",
    driver: "directory",
    default: true,
    nodeIds: ["node-0"],
    state: "ready",
    allocatedGiB: 128,
    capabilities: ["volumes"],
    storage: {
      path: "/data/netlab",
      filesystem: "xfs",
      capacityBytes: 2 ** 40,
      availableBytes: 800 * 2 ** 30,
      nativeSnapshots: false,
    },
  },
];
async function fixture(page: Page) {
  await page.route("**/api/v1/**", async (route) => {
    const url = new URL(route.request().url()),
      path = url.pathname.replace("/api/v1", "");
    let response: unknown;
    if (path === "/identity")
      response = { id: "admin", name: "admin", administrator: true };
    else if (path === "/environments")
      response = [
        "企业网络实验室",
        "工业控制联调",
        "Windows 兼容性验证",
        "网络基础课程",
      ]
        .map((name, index) => ({
          id: `env-${index}`,
          name,
          projectId: "default",
          externalReference: `LAB-2026-0${index + 1}`,
          status: ["running", "running", "stopped", "draft"][index],
          assetCount: [20, 12, 8, 0][index],
          networkCount: [3, 2, 2, 0][index],
          revision: 1,
          createdAt: stamp,
          updatedAt: stamp,
        }))
        .filter(
          (item) =>
            item.name.includes(url.searchParams.get("search") ?? "") &&
            (!url.searchParams.get("status") ||
              item.status === url.searchParams.get("status")),
        );
    else if (path === "/templates")
      response = templates.filter(
        (item) =>
          item.name
            .toLowerCase()
            .includes((url.searchParams.get("search") ?? "").toLowerCase()) &&
          (!url.searchParams.get("kind") ||
            item.kind === url.searchParams.get("kind")),
      );
    else if (path === "/templates/windows/cache")
      response = [
        {
          nodeId: "node-0",
          nodeName: "计算节点 A",
          bytes: 16 * 2 ** 30,
          reclaimable: true,
        },
      ];
    else if (path.endsWith("/cache")) response = [];
    else if (path === "/nodes")
      response = nodes.filter((node) =>
        node.name.includes(url.searchParams.get("search") ?? ""),
      );
    else if (path === "/nodes/metrics") {
      const end = new Date(),
        start = new Date(end.getTime() - 60000);
      response = {
        start: start.toISOString(),
        end: end.toISOString(),
        stepSeconds: 10,
        series: nodes.flatMap((node) =>
          Object.entries({
            cpu: 9.5,
            cpu_capacity: 32,
            memory: 24 * 2 ** 30,
            memory_capacity: 128 * 2 ** 30,
            memory_available: 104 * 2 ** 30,
            disk_used: 256 * 2 ** 30,
            disk_capacity: 2 * 2 ** 40,
          }).map(([metric, value]) => ({
            metric,
            nodeId: node.id,
            points: [0, 10, 20, 30, 40, 50, 60].map((offset) => ({
              time: new Date(start.getTime() + offset * 1000).toISOString(),
              value,
            })),
          })),
        ),
      };
    } else if (path === "/storage-pools") response = pools;
    else if (path === "/storage-pools/shared/ceph")
      response = {
        health: "HEALTH_OK",
        messages: [],
        replicas: 1,
        osdsUp: 2,
        osdsTotal: 2,
        disks: nodes.map((node, index) => ({
          name: `osd.${index}`,
          host: node.name,
          state: "up",
          capacityBytes: 2 ** 40,
          usedBytes: (256 + index * 64) * 2 ** 30,
        })),
        daemons: [
          {
            name: "mon.node-0",
            role: "mon",
            host: "计算节点 A",
            state: "running",
          },
        ],
      };
    else if (path.endsWith("/assets")) response = [];
    else if (path === "/volumes")
      response = [
        {
          id: "data",
          name: "业务数据库",
          kind: "vm",
          storagePoolId: "shared",
          sizeGiB: 100,
          state: "ready",
          references: [],
        },
      ];
    else if (path === "/backup-repositories")
      response = [
        {
          id: "repository",
          name: "归档仓库",
          nodeId: "node-0",
          state: "ready",
          location: "/mnt/backups",
        },
      ];
    else if (path === "/backup-repositories/repository/backups")
      response = [
        {
          id: "backup",
          name: "企业网络实验室",
          repositoryId: "repository",
          state: "ready",
          sizeBytes: 64 * 2 ** 30,
          createdAt: stamp,
        },
      ];
    else if (path === "/blueprints" || path === "/operations") response = [];
    else
      return route.fulfill({
        status: 404,
        json: { detail: `未配置的测试接口：${path}` },
      });
    await route.fulfill({ json: response });
  });
}
async function fits(page: Page) {
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
}

test("环境、模板、节点与存储采用各自工作布局，适配日夜与窄屏", async ({
  page,
}, info) => {
  test.setTimeout(90000);
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await fixture(page);
  for (const theme of ["light", "dark"]) {
    await page.goto("/environments");
    if (theme === "dark")
      await page
        .getByRole("button", { name: "切换夜间主题", exact: true })
        .click();
    for (const width of [1366, 1920, 390]) {
      await page.setViewportSize({ width, height: 900 });
      for (const [section, url] of [
        ["environments", "/environments"],
        ["templates", "/templates"],
        ["nodes", "/resources"],
        ["storage", "/resources/storage"],
      ]) {
        await page.goto(url);
        if (section === "environments")
          await expect(
            page.getByRole("link", { name: /企业网络实验室/ }),
          ).toBeVisible();
        if (section === "templates") {
          await expect(
            page.getByRole("region", { name: "资产模板", exact: true }),
          ).toContainText("pc-q35-10.0");
          await page
            .getByRole("button", { name: "Nginx 服务", exact: true })
            .focus();
          await page.keyboard.press("Enter");
          await expect(
            page.getByRole("region", { name: "资产模板", exact: true }),
          ).toContainText("docker.io/library/nginx:stable");
          await page
            .getByRole("button", { name: "Windows Server 2022", exact: true })
            .click();
        }
        if (section === "nodes")
          await expect(
            page.getByRole("button", { name: /计算节点 A/ }),
          ).toContainText("9.5");
        if (section === "storage")
          await expect(
            page.getByRole("heading", { name: "共享存储", exact: true }),
          ).toBeVisible();
        await fits(page);
        await page.screenshot({
          path: info.outputPath(`${section}-${width}-${theme}.png`),
          animations: "disabled",
        });
      }
    }
  }
  expect(errors).toEqual([]);
});

test("创建、导入、卷与备份操作在所属工作区直接可达", async ({ page }, info) => {
  await fixture(page);
  await page.goto("/environments");
  await page.getByRole("button", { name: "新建环境", exact: true }).click();
  const create = page.getByRole("dialog", { name: "新建环境", exact: true });
  await expect(
    create.getByRole("textbox", { name: "环境模板", exact: true }),
  ).toBeFocused();
  await expect(
    create.getByRole("textbox", { name: "环境模板", exact: true }),
  ).toHaveValue("空白环境");
  await page.screenshot({ path: info.outputPath("create-environment.png") });
  await page.keyboard.press("Escape");
  await page.goto("/templates");
  await page.getByRole("button", { name: "导入模板", exact: true }).click();
  await expect(
    page
      .getByRole("dialog", { name: "导入资产模板", exact: true })
      .getByRole("heading", { name: "镜像来源", exact: true }),
  ).toBeVisible();
  await page.screenshot({ path: info.outputPath("import-template.png") });
  await page.keyboard.press("Escape");
  await page.goto("/resources/storage");
  await page.getByRole("tab", { name: "持久卷", exact: true }).click();
  await page
    .getByRole("row")
    .filter({ hasText: "业务数据库" })
    .getByRole("button", { name: "扩容", exact: true })
    .click();
  await expect(
    page.getByRole("dialog", { name: "扩容 业务数据库", exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  await page.goto("/resources");
  await page.getByRole("button", { name: /计算节点 A/ }).click();
  await page.getByRole("tab", { name: "备份仓库", exact: true }).click();
  await page.getByRole("button", { name: "归档仓库", exact: true }).click();
  await expect(
    page.getByRole("dialog", { name: "归档仓库", exact: true }),
  ).toContainText("企业网络实验室");
  await page.getByRole("button", { name: "恢复", exact: true }).click();
  await expect(
    page.getByRole("dialog", { name: "从备份创建环境", exact: true }),
  ).toBeVisible();
});
