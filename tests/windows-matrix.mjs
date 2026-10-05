import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { execFile } from "node:child_process";
import { copyFile, mkdir, readFile, writeFile, rm } from "node:fs/promises";
import { createReadStream } from "node:fs";
import { promisify } from "node:util";
import { delay } from "./guest-ssh.mjs";
import { nodeCommand, powershell, qga, ready, reboot } from "./windows-qga.mjs";

const execute = promisify(execFile);
const base = process.env.NETLAB_TEST_URL || "http://127.0.0.1:8090";
const media = "D:/.cache/netlab/artifacts/windows-media";
const report = {
  startedAt: new Date().toISOString(),
  cases: [],
  cleanupErrors: [],
};
const cases = [
  {
    name: "Windows Server 2022 BIOS",
    os: "Windows Server 2022",
    iso: "server2022-eval.iso",
    firmware: "bios",
    machine: "pc",
    driver: "2k22",
    index: 2,
  },
  {
    name: "Windows Server 2022 UEFI",
    os: "Windows Server 2022",
    iso: "server2022-eval.iso",
    firmware: "uefi",
    machine: "q35",
    driver: "2k22",
    index: 2,
  },
  {
    name: "Windows 11 UEFI / TPM 2.0",
    os: "Windows 11",
    iso: "windows11-eval.iso",
    firmware: "uefi",
    machine: "q35",
    driver: "w11",
    index: 1,
  },
].filter(
  (item) =>
    !process.env.NETLAB_WINDOWS_CASE ||
    item.name.includes(process.env.NETLAB_WINDOWS_CASE),
);
if (process.env.NETLAB_WINDOWS_RESUME_ENVIRONMENT)
  assert.equal(cases.length, 1, "Select the existing Windows case to resume");
let cookie;
async function api(path, method = "GET", body) {
  const response = await fetch(base + "/api/v1" + path, {
    method,
    headers: { Cookie: cookie || "", "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(30_000),
  });
  if (path === "/sessions/login")
    cookie = response.headers.get("set-cookie")?.split(";")[0];
  const text = await response.text();
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${text}`);
  return text ? JSON.parse(text) : undefined;
}
async function complete(id) {
  for (const deadline = Date.now() + 300_000; Date.now() < deadline;) {
    const op = await api("/operations/" + id);
    if (["succeeded", "failed", "partially_applied"].includes(op.state)) {
      assert.equal(op.state, "succeeded", `${op.phase}: ${op.error}`);
      return;
    }
    await delay(500);
  }
  throw new Error("operation timed out: " + id);
}
async function upload(definition, files) {
  const boundary = "netlab-" + randomUUID();
  async function* body() {
    yield `--${boundary}\r\nContent-Disposition: form-data; name="template"; filename="template.json"\r\nContent-Type: application/json\r\n\r\n${JSON.stringify(definition)}\r\n`;
    for (const [name, path] of files) {
      yield `--${boundary}\r\nContent-Disposition: form-data; name="files"; filename="${name}"\r\nContent-Type: application/octet-stream\r\n\r\n`;
      yield* createReadStream(path);
      yield "\r\n";
    }
    yield `--${boundary}--\r\n`;
  }
  const response = await fetch(base + "/api/v1/templates", {
    method: "POST",
    headers: {
      Cookie: cookie,
      "Content-Type": "multipart/form-data; boundary=" + boundary,
    },
    body: body(),
    duplex: "half",
    signal: AbortSignal.timeout(600_000),
  });
  const result = await response.json();
  assert.ok(response.ok, "template upload: " + JSON.stringify(result));
  await complete(result.operationId);
  return result;
}

await api(
  "/sessions/login",
  "POST",
  JSON.parse(await readFile("data/dev-login.json", "utf8")),
);
const nodes = await api("/nodes");
const source = nodes.find((node) => node.name === "Local node");
assert.ok(source);
for (const test of cases) {
  console.log(JSON.stringify({ name: test.name, stage: "prepare" }));
  const result = { name: test.name, passed: false },
    started = performance.now();
  const password = randomUUID() + "!",
    setup = `${media}/matrix-${randomUUID()}`;
  let template, environment, actual;
  try {
    const resume = process.env.NETLAB_WINDOWS_RESUME_ENVIRONMENT;
    if (resume) {
      environment = await api("/environments/" + resume);
      template = (
        await api("/templates?ids=" + environment.spec.assets[0].templateId)
      )[0];
      assert.equal(template.os, test.os);
      assert.equal(template.hardware.firmware, test.firmware);
    } else {
      await mkdir(setup);
      const machine = source.vmHardware.machines.find(
        (machine) =>
          machine.aliases.includes(test.machine) &&
          machine.firmware.includes(test.firmware) &&
          (test.firmware === "bios" || (machine.secureBoot && machine.tpm2)),
      );
      assert.ok(machine, "host firmware capability missing");
      let xml = (
        await readFile(
          new URL("./fixtures/windows-unattend.xml", import.meta.url),
          "utf8",
        )
      )
        .replaceAll("@@PASSWORD@@", password)
        .replaceAll("2k25", test.driver)
        .replace(
          "<Value>2</Value></MetaData>",
          `<Value>${test.index}</Value></MetaData>`,
        );
      if (test.firmware === "bios")
        xml = xml
          .replace(
            /<CreatePartitions>[\s\S]*?<\/ModifyPartitions>/,
            '<CreatePartitions><CreatePartition wcm:action="add"><Order>1</Order><Type>Primary</Type><Extend>true</Extend></CreatePartition></CreatePartitions><ModifyPartitions><ModifyPartition wcm:action="add"><Order>1</Order><PartitionID>1</PartitionID><Format>NTFS</Format><Label>Windows</Label><Letter>C</Letter><Active>true</Active></ModifyPartition></ModifyPartitions>',
          )
          .replace(
            "<PartitionID>3</PartitionID></InstallTo>",
            "<PartitionID>1</PartitionID></InstallTo>",
          );
      await writeFile(setup + "/Autounattend.xml", xml);
      await copyFile(
        new URL("./fixtures/windows-matrix.ps1", import.meta.url),
        setup + "/windows.ps1",
      );
      await copyFile(
        media + "/qemu-ga-x86_64.msi",
        setup + "/qemu-ga-x86_64.msi",
      );
      const linuxSetup = setup.replace("D:", "/mnt/d");
      await execute(
        "wsl.exe",
        [
          "-d",
          "Ubuntu",
          "-u",
          "root",
          "--exec",
          "7z",
          "x",
          "-y",
          "-o" + linuxSetup + "/drivers",
          "/mnt/d/.cache/netlab/artifacts/windows-media/virtio-win.iso",
          ...["viostor", "vioscsi", "NetKVM", "vioserial"].map(
            (driver) => `${driver}/${test.driver}/amd64/*`,
          ),
        ],
        { maxBuffer: 1 << 20 },
      );
      await execute("wsl.exe", [
        "-d",
        "Ubuntu",
        "-u",
        "root",
        "--exec",
        "genisoimage",
        "-quiet",
        "-joliet",
        "-rock",
        "-V",
        "NETLAB_SETUP",
        "-o",
        linuxSetup + ".iso",
        linuxSetup,
      ]);
      console.log(JSON.stringify({ name: test.name, stage: "upload" }));
      template = await upload(
        {
          id: randomUUID(),
          name: test.name + " verification installer",
          kind: "vm",
          os: test.os,
          version: 1,
          format: "iso",
          source: "windows.iso",
          resources: { cpu: 2, memoryMiB: 4096, diskGiB: 64 },
          hardware: {
            machine: machine.name,
            firmware: test.firmware,
            secureBoot: test.firmware === "uefi",
            tpm: test.firmware === "uefi",
            guestAgent: true,
            cpuModel: "host-passthrough",
            diskBus: "virtio",
            nicModel: "virtio",
          },
          media: [{ id: "setup", source: "setup.iso" }],
        },
        [
          ["windows.iso", media + "/" + test.iso],
          ["setup.iso", setup + ".iso"],
        ],
      );
      console.log(JSON.stringify({ name: test.name, stage: "create" }));
      const asset = {
        id: randomUUID(),
        name: test.name,
        templateId: template.id,
        resources: { cpu: 2, memoryMiB: 4096, diskGiB: 64 },
        storagePoolId: "default:" + source.id,
        interfaces: [],
      };
      environment = await api("/environments", "POST", {
        name: test.name + " verification",
        run: true,
        spec: { networks: [], assets: [asset] },
        clientRequestId: randomUUID(),
      });
      await complete(environment.operationId);
    }
    actual = (await api(`/environments/${environment.id}/state`)).assets[0];
    result.environmentId = environment.id;
    if (!resume) {
      await delay(2000);
      await nodeCommand(actual, [
        "virsh",
        "send-key",
        actual.instanceId,
        "--codeset",
        "linux",
        "28",
      ]);
    }
    console.log(
      JSON.stringify({
        name: test.name,
        stage: "guest setup",
        instanceId: actual.instanceId,
      }),
    );
    await ready(
      async () =>
        assert.equal(
          await powershell(
            actual,
            "Get-Content C:\\ProgramData\\NetlabTests\\matrix-setup.done",
          ),
          "installed",
        ),
      1800_000,
    );
    result.guest = await qga(actual, { execute: "guest-get-osinfo" });
    assert.match(result.guest.version, new RegExp(test.os));
    result.hardware = JSON.parse(
      await powershell(
        actual,
        "$firmware=(Get-ComputerInfo -Property BiosFirmwareType).BiosFirmwareType.ToString().ToLower(); if($firmware -eq 'legacy'){$firmware='bios'}; @{firmware=$firmware; tpm=(Get-Tpm).TpmPresent; secureBoot=if($firmware -eq 'uefi'){Confirm-SecureBootUEFI}else{$false}} | ConvertTo-Json -Compress",
      ),
    );
    assert.equal(result.hardware.firmware, test.firmware);
    assert.equal(result.hardware.tpm, test.firmware === "uefi");
    assert.equal(result.hardware.secureBoot, test.firmware === "uefi");
    await powershell(
      actual,
      "Set-Content C:\\ProgramData\\NetlabTests\\reboot-proof.txt 'persisted'",
    );
    result.reboot = await reboot(actual);
    await ready(async () =>
      assert.equal(
        await powershell(
          actual,
          "Get-Content C:\\ProgramData\\NetlabTests\\reboot-proof.txt",
        ),
        "persisted",
      ),
    );
    result.passed = true;
  } catch (error) {
    result.error = error.message.replaceAll(password, "[redacted]");
    console.log(
      JSON.stringify({ name: test.name, stage: "failed", error: result.error }),
    );
    process.exitCode = 1;
    if (actual)
      try {
        const screenshot = `/var/lib/netlab-dev/test-tmp/matrix-${actual.instanceId}.png`;
        await nodeCommand(actual, [
          "virsh",
          "screenshot",
          actual.instanceId,
          screenshot,
        ]);
        result.screenshot = `data/matrix-${actual.instanceId}.png`;
        await writeFile(
          result.screenshot,
          Buffer.from(
            await nodeCommand(actual, ["base64", "-w0", screenshot]),
            "base64",
          ),
        );
        await nodeCommand(actual, ["rm", screenshot]);
      } catch (diagnostic) {
        result.diagnosticError = diagnostic.message;
      }
  } finally {
    if (environment && result.passed)
      try {
        await complete(
          (
            await api(`/environments/${environment.id}/actions`, "POST", {
              action: "destroy",
              clientRequestId: randomUUID(),
            })
          ).id,
        );
      } catch (error) {
        report.cleanupErrors.push(error.message);
      }
    if (template && result.passed)
      try {
        await complete((await api(`/templates/${template.id}`, "DELETE")).id);
      } catch (error) {
        report.cleanupErrors.push(error.message);
      }
    await rm(setup, { recursive: true, force: true });
    await rm(setup + ".iso", { force: true });
    result.durationMs = Math.round(performance.now() - started);
    report.cases.push(result);
    report.finishedAt = new Date().toISOString();
    report.passed =
      report.cases.every((item) => item.passed) && !report.cleanupErrors.length;
    await writeFile(
      "data/windows-matrix-result.json",
      JSON.stringify(report, null, 2),
    );
    console.log(JSON.stringify(result));
  }
  if (!result.passed) break;
}
