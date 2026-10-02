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
      const body = request.postDataJSON();
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

test("OVA and OVF import omit hardware and display the parsed template", async ({
  page,
}) => {
  const imported = await fixture(page);
  const drawer = page.getByRole("dialog");
  await drawer.getByRole("button", { name: "虚拟机", exact: true }).click();
  await drawer.getByText("整机镜像", { exact: true }).click();
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
