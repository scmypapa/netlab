import { expect, test, type Page } from "@playwright/test";

const time = "2026-10-02T08:00:00Z";
const machines = [
  {
    name: "pc-q35-10.0",
    aliases: ["q35"],
    maxVcpus: 512,
    firmware: ["bios", "uefi"],
    secureBoot: true,
    tpm2: true,
    diskBuses: ["sata", "scsi"],
    firmwareFiles: [],
  },
  {
    name: "pc-i440fx-9.2",
    aliases: ["pc"],
    maxVcpus: 255,
    firmware: ["bios"],
    secureBoot: false,
    tpm2: false,
    diskBuses: ["ide", "scsi"],
    firmwareFiles: [],
  },
];

async function fixture(page: Page) {
  const imported: Record<string, unknown>[] = [];
  const templates: Record<string, unknown>[] = [];
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname.replace("/api/v1", "");
    let response: unknown = [];
    let status = 200;
    if (path === "/identity")
      response = { id: "admin", name: "admin", administrator: true };
    else if (path === "/nodes")
      response = [
        {
          id: "ready",
          name: "Ready",
          endpoint: "https://node.test",
          state: "ready",
          capacity: { cpu: 64, memoryMiB: 131072, diskGiB: 1024 },
          reserved: { cpu: 0, memoryMiB: 0, diskGiB: 0 },
          capabilities: ["vm"],
          slots: 8,
          observedAt: time,
          vmHardware: {
            machines,
            cpuModes: ["host-model"],
            cpuModels: [],
            nicModels: ["e1000e", "vmxnet3"],
            diskControllers: ["lsilogic", "virtio-scsi"],
          },
        },
        {
          id: "offline",
          name: "Offline",
          state: "offline",
          vmHardware: {
            machines: [{ ...machines[0], name: "unavailable-machine" }],
            nicModels: ["rtl8139"],
            diskControllers: ["buslogic"],
          },
        },
      ];
    else if (path === "/templates" && request.method() === "POST") {
      const contentType = request.headers()["content-type"];
      const body = contentType?.startsWith("multipart/form-data")
        ? JSON.parse(
            request
              .postDataBuffer()!
              .toString()
              .match(/name="template"[\s\S]*?\r\n\r\n([\s\S]*?)\r\n--/)![1],
          )
        : request.postDataJSON();
      imported.push(body);
      const template =
        body.format === "ova" || body.format === "ovf"
          ? {
              ...body,
              resources: { cpu: 4, memoryMiB: 4096, diskGiB: 42 },
              hardware: {
                machine: "pc-i440fx-9.2",
                firmware: "bios",
                diskBus: "scsi",
                diskController: "lsilogic",
                nicModel: "vmxnet3",
              },
              disks: [
                { id: "boot", bus: "scsi", sizeGiB: 32, bootOrder: 1 },
                { id: "data", bus: "scsi", sizeGiB: 10, bootOrder: 2 },
              ],
              nicModels: ["vmxnet3"],
              state: "ready",
            }
          : { ...body, state: "ready" };
      templates.push(template);
      response = template;
      status = 201;
    } else if (path === "/templates") response = templates;
    await route.fulfill({
      status,
      contentType: "application/json",
      body: JSON.stringify(response),
    });
  });
  await page.goto("/templates");
  await page
    .getByRole("button", { name: "导入模板", exact: true })
    .first()
    .click();
  return imported;
}

test("bare VM import uses native node hardware and initialization", async ({
  page,
}) => {
  const imported = await fixture(page);
  const drawer = page.getByRole("dialog");
  await drawer.getByRole("button", { name: "虚拟机", exact: true }).click();
  await drawer.getByText("链接或节点路径", { exact: true }).click();
  await drawer.getByRole("textbox", { name: "模板名称" }).fill("Linux VM");
  await drawer
    .getByRole("textbox", { name: "文件地址" })
    .fill("https://storage.test/linux.qcow2");
  await expect(drawer.getByRole("textbox", { name: "机器类型" })).toHaveValue(
    "pc-q35-10.0",
  );
  await drawer.getByRole("textbox", { name: "固件", exact: true }).click();
  await page.getByRole("option", { name: "UEFI", exact: true }).click();
  await drawer.getByRole("textbox", { name: "磁盘总线" }).click();
  await page.getByRole("option", { name: "SCSI", exact: true }).click();
  await expect(drawer.getByRole("textbox", { name: "磁盘控制器" })).toHaveValue(
    "lsilogic",
  );
  await drawer.getByRole("textbox", { name: "网卡型号" }).click();
  await expect(
    page.getByRole("option", { name: "virtio", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("option", { name: "vmxnet3", exact: true }).click();
  await drawer
    .getByRole("switch", { name: "Secure Boot", exact: true })
    .check();
  await drawer.getByRole("switch", { name: "TPM 2.0", exact: true }).check();
  await drawer.getByRole("button", { name: "来宾初始化", exact: true }).click();
  await drawer.getByRole("textbox", { name: "初始化方式" }).click();
  await page.getByRole("option", { name: "cloud-init", exact: true }).click();
  await page.setViewportSize({ width: 390, height: 900 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await drawer.getByRole("button", { name: "导入模板", exact: true }).click();
  await expect(drawer).not.toBeVisible();
  expect(imported[0]).toMatchObject({
    kind: "vm",
    format: "qcow2",
    initialization: "cloud-init",
    resources: { cpu: 2, memoryMiB: 2048, diskGiB: 20 },
    hardware: {
      machine: "pc-q35-10.0",
      firmware: "uefi",
      diskBus: "scsi",
      diskController: "lsilogic",
      nicModel: "vmxnet3",
      secureBoot: true,
      tpm: true,
    },
  });
});

test("OVF uploads the descriptor and associated disks with a selected main file", async ({
  page,
}) => {
  const imported = await fixture(page),
    drawer = page.getByRole("dialog");
  await drawer.getByRole("button", { name: "虚拟机", exact: true }).click();
  await drawer.getByText("整机镜像", { exact: true }).click();
  await drawer.getByRole("textbox", { name: "文件格式" }).click();
  await page.getByRole("option", { name: "OVF", exact: true }).click();
  await drawer.getByRole("textbox", { name: "模板名称" }).fill("OVF upload");
  await drawer
    .locator('input[type="file"]')
    .first()
    .setInputFiles([
      {
        name: "disk.vmdk",
        mimeType: "application/octet-stream",
        buffer: Buffer.from("disk fixture"),
      },
      {
        name: "machine.ovf",
        mimeType: "application/xml",
        buffer: Buffer.from("<Envelope/>"),
      },
    ]);
  await expect(drawer.getByRole("textbox", { name: "主镜像文件" })).toHaveValue(
    "machine.ovf",
  );
  await page.setViewportSize({ width: 390, height: 900 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await drawer.getByRole("button", { name: "导入模板", exact: true }).click();
  await expect(drawer).not.toBeVisible();
  expect(imported[0]).toMatchObject({ format: "ovf", source: "machine.ovf" });
});

test("ISO installation selects Windows 11 hardware and uploads its driver disc", async ({
  page,
}) => {
  const imported = await fixture(page),
    drawer = page.getByRole("dialog");
  await drawer.getByRole("button", { name: "虚拟机", exact: true }).click();
  await drawer
    .getByRole("textbox", { name: "模板名称" })
    .fill("Windows 11 installation");
  await drawer.getByRole("textbox", { name: "操作系统" }).click();
  await page.getByRole("option", { name: "Windows 11", exact: true }).click();
  await drawer.getByText("安装系统", { exact: true }).click();
  await expect(
    drawer.getByRole("textbox", { name: "固件", exact: true }),
  ).toHaveValue("UEFI");
  await expect(
    drawer.getByRole("switch", { name: "Secure Boot", exact: true }),
  ).toBeChecked();
  await expect(
    drawer.getByRole("switch", { name: "TPM 2.0", exact: true }),
  ).toBeChecked();
  await drawer
    .locator('input[type="file"]')
    .first()
    .setInputFiles({
      name: "windows.iso",
      mimeType: "application/octet-stream",
      buffer: Buffer.from("installer fixture"),
    });
  await drawer
    .locator('input[type="file"]')
    .last()
    .setInputFiles({
      name: "virtio.iso",
      mimeType: "application/octet-stream",
      buffer: Buffer.from("driver fixture"),
    });
  await page.setViewportSize({ width: 390, height: 900 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await drawer.getByRole("button", { name: "导入模板", exact: true }).click();
  await expect(drawer).not.toBeVisible();
  expect(imported[0]).toMatchObject({
    format: "iso",
    initialization: "none",
    resources: { cpu: 2, memoryMiB: 4096, diskGiB: 64 },
    media: [{ source: "virtio.iso" }],
  });
});

test("registry authentication is submitted only with a registry import", async ({
  page,
}) => {
  const imported = await fixture(page),
    drawer = page.getByRole("dialog");
  await drawer
    .getByRole("textbox", { name: "模板名称" })
    .fill("Private registry");
  await drawer
    .getByRole("textbox", { name: "镜像地址" })
    .fill("registry.example.test/dev/nginx:1");
  await drawer.getByRole("button", { name: "仓库认证" }).click();
  await drawer.getByRole("textbox", { name: "仓库用户名" }).fill("test");
  await drawer
    .getByLabel("仓库密码或访问令牌", { exact: true })
    .fill("test-password");
  await drawer.getByRole("switch", { name: "HTTP 仓库" }).check();
  await drawer.getByRole("button", { name: "导入模板", exact: true }).click();
  await expect(drawer).not.toBeVisible();
  expect(imported[0]).toMatchObject({
    registry: { username: "test", password: "test-password", plainHttp: true },
  });
});

test("OVA and OVF import omit hardware and display the parsed template", async ({
  page,
}) => {
  const imported = await fixture(page);
  const drawer = page.getByRole("dialog");
  await drawer.getByRole("button", { name: "虚拟机", exact: true }).click();
  await drawer.getByText("整机镜像", { exact: true }).click();
  await drawer.getByText("链接或节点路径", { exact: true }).click();
  await drawer
    .getByRole("textbox", { name: "模板名称" })
    .fill("Windows appliance");
  await drawer.getByRole("textbox", { name: "文件格式" }).click();
  await page.getByRole("option", { name: "OVF", exact: true }).click();
  await drawer
    .getByRole("textbox", { name: "文件地址" })
    .fill("https://storage.test/windows.ovf");
  await expect(drawer.getByRole("textbox", { name: "机器类型" })).toHaveCount(
    0,
  );
  await expect(drawer.getByLabel("CPU · 核", { exact: true })).toHaveCount(0);
  await drawer.getByRole("button", { name: "导入模板", exact: true }).click();
  await expect(drawer).not.toBeVisible();
  expect(imported[0]).toMatchObject({
    kind: "vm",
    format: "ovf",
    initialization: "none",
  });
  expect(imported[0]).not.toHaveProperty("hardware");
  await page.getByRole("button", { name: /Windows appliance/ }).click();
  await expect(drawer).toContainText("pc-i440fx-9.2");
  await expect(drawer).toContainText("BIOS");
  await expect(drawer).toContainText("lsilogic");
  await expect(drawer).toContainText("42 GiB");
  await expect(drawer).toContainText("启动盘");
});

test("container image package uses the existing import API without VM fields", async ({
  page,
}) => {
  const imported = await fixture(page);
  const drawer = page.getByRole("dialog");
  await drawer.getByText("镜像包", { exact: true }).click();
  await drawer.getByText("链接或节点路径", { exact: true }).click();
  await drawer.getByRole("textbox", { name: "模板名称" }).fill("Modbus device");
  await drawer
    .getByRole("textbox", { name: "文件地址" })
    .fill("https://storage.test/modbus.tar");
  await expect(
    drawer.getByRole("button", { name: "来宾初始化", exact: true }),
  ).toHaveCount(0);
  await drawer.getByRole("button", { name: "导入模板", exact: true }).click();
  await expect(drawer).not.toBeVisible();
  expect(imported[0]).toMatchObject({
    kind: "container",
    format: "docker",
    source: "https://storage.test/modbus.tar",
  });
  expect(imported[0]).not.toHaveProperty("hardware");
  expect(imported[0]).toHaveProperty("initialization", "none");
});

test("ordinary template details hide the source row and retain virtual hardware", async ({
  page,
}) => {
  await fixture(page);
  await page.route("**/api/v1/identity", (route) =>
    route.fulfill({
      json: { id: "viewer", name: "viewer", administrator: false },
    }),
  );
  await page.route("**/api/v1/templates?*", (route) =>
    route.fulfill({
      json: [
        {
          id: "windows",
          name: "Windows template",
          kind: "vm",
          os: "Windows",
          source: "",
          version: 1,
          state: "ready",
          format: "qcow2",
          initialization: "cloudbase-init",
          resources: { cpu: 4, memoryMiB: 4096, diskGiB: 40 },
          hardware: {
            machine: "pc-q35-10.0",
            firmware: "uefi",
            diskBus: "sata",
            nicModel: "e1000e",
          },
        },
      ],
    }),
  );
  await page.goto("/templates");
  await expect(
    page.getByRole("button", { name: "导入模板", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: /Windows template/ }).click();
  const drawer = page.getByRole("dialog", { name: "资产模板" });
  await expect(drawer).toContainText("pc-q35-10.0");
  await expect(drawer).toContainText("UEFI");
  await expect(drawer).toContainText("Cloudbase-Init");
  await expect(drawer.locator("dt").filter({ hasText: "地址" })).toHaveCount(0);
});

test("template deletion keeps reference errors visible and closes after acceptance", async ({
  page,
}) => {
  await fixture(page);
  await page.setViewportSize({ width: 390, height: 844 });
  let allowed = false;
  let items = [
    {
      id: "delete-me",
      name: "Lab container",
      kind: "container",
      os: "Linux",
      version: 1,
      source: "nginx:stable",
      state: "ready",
      resources: { cpu: 1, memoryMiB: 512, diskGiB: 1 },
    },
  ];
  await page.route("**/api/v1/templates?*", (route) =>
    route.fulfill({ json: items }),
  );
  await page.route("**/api/v1/templates/delete-me", (route) => {
    if (!allowed)
      return route.fulfill({
        status: 409,
        json: {
          status: 409,
          title: "Conflict",
          detail: "模板仍被引用：环境：Training",
        },
      });
    items = [];
    return route.fulfill({
      status: 202,
      json: { id: "remove", kind: "delete-template", state: "queued" },
    });
  });
  await page.goto("/templates");
  await page.getByRole("button", { name: /Lab container/ }).click();
  await page.getByRole("button", { name: "删除模板", exact: true }).click();
  const confirmation = page.getByRole("dialog", {
    name: "删除 Lab container？",
  });
  await confirmation.getByRole("button", { name: "删除", exact: true }).click();
  await expect(confirmation).toContainText("环境：Training");
  allowed = true;
  await confirmation.getByRole("button", { name: "删除", exact: true }).click();
  await expect(confirmation).not.toBeVisible();
  await expect(
    page.getByRole("dialog", { name: "资产模板" }),
  ).not.toBeVisible();
  await expect(page.getByRole("button", { name: /Lab container/ })).toHaveCount(
    0,
  );
});

test("failed deletion resumes the original operation from the template", async ({
  page,
}) => {
  await fixture(page);
  let retried = false;
  await page.route("**/api/v1/templates?*", (route) =>
    route.fulfill({
      json: [
        {
          id: "delete-me",
          name: "Failed deletion",
          kind: "container",
          os: "Linux",
          version: 1,
          source: "nginx:stable",
          state: "deleting",
          error: "Worker unavailable",
          operationId: "remove",
          resources: { cpu: 1, memoryMiB: 512, diskGiB: 1 },
        },
      ],
    }),
  );
  await page.route("**/api/v1/operations/remove/retry", (route) => {
    retried = true;
    return route.fulfill({
      status: 202,
      json: { id: "remove", kind: "delete-template", state: "queued" },
    });
  });
  await page.goto("/templates");
  await page.getByRole("button", { name: /Failed deletion/ }).click();
  const drawer = page.getByRole("dialog", { name: "资产模板" });
  await expect(drawer).toContainText("Worker unavailable");
  await expect(
    drawer.getByRole("button", { name: "删除模板", exact: true }),
  ).toHaveCount(0);
  await drawer.getByRole("button", { name: "重试删除" }).click();
  await expect(drawer).not.toBeVisible();
  expect(retried).toBe(true);
});
