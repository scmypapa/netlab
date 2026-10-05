import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import { delay } from "./guest-ssh.mjs";
import { nodeCommand, powershell, qga, ready, reboot } from "./windows-qga.mjs";

const base = process.env.NETLAB_TEST_URL || "http://127.0.0.1:8090";
const report = {
  startedAt: new Date().toISOString(),
  steps: [],
  cleanupErrors: [],
};
const domain = "netlab.test",
  password = randomUUID() + "!";
let cookie, environment, point, dc, member, windows, container;
const clean = (error) => error.message.replaceAll(password, "[redacted]");

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
    const operation = await api("/operations/" + id);
    if (
      ["succeeded", "failed", "partially_applied"].includes(operation.state)
    ) {
      assert.equal(
        operation.state,
        "succeeded",
        `${operation.phase}: ${operation.error}`,
      );
      return;
    }
    await delay(500);
  }
  throw new Error("operation timed out: " + id);
}
async function step(name, action) {
  const item = { name, passed: false },
    start = performance.now();
  try {
    await action();
    item.passed = true;
  } catch (error) {
    item.error = clean(error);
    throw error;
  } finally {
    item.durationMs = Math.round(performance.now() - start);
    report.steps.push(item);
    console.log(JSON.stringify(item));
    await writeFile(
      "data/windows-ad-result.json",
      JSON.stringify(report, null, 2),
    );
  }
}
async function boot(actual) {
  const hostname = environment.spec.assets.find(
    (asset) => asset.id === actual.assetId,
  ).guest.hostname;
  await ready(async () => {
    assert.equal(
      (await powershell(actual, "hostname")).toLowerCase(),
      hostname,
    );
    assert.equal(
      (await qga(actual, { execute: "guest-get-osinfo" })).id,
      "mswindows",
    );
    assert.equal(
      await powershell(
        actual,
        "(Get-ItemProperty HKLM:\\SYSTEM\\Setup).SystemSetupInProgress",
      ),
      "0",
    );
    assert.equal(
      await powershell(
        actual,
        "(Get-Content -LiteralPath 'C:\\Program Files\\Cloudbase Solutions\\Cloudbase-Init\\log\\cloudbase-init.log' -Tail 200 | Select-String -SimpleMatch 'Plugins execution done').Count -gt 0",
      ),
      "True",
    );
  }, 600_000);
}
async function verifyDomain() {
  await ready(async () =>
    assert.equal(
      await powershell(
        dc,
        "(Get-Service NTDS,DNS,Netlogon | Where-Object Status -ne Running).Count",
      ),
      "0",
    ),
  );
  await ready(async () =>
    assert.equal(
      await powershell(
        member,
        "(Get-CimInstance Win32_ComputerSystem).PartOfDomain -and (Test-ComputerSecureChannel)",
      ),
      "True",
    ),
  );
  await verifyDns();
}
async function verifyDns() {
  report.srvRecords = JSON.parse(
    await powershell(
      member,
      `ConvertTo-Json -InputObject @(Resolve-DnsName -Type SRV _ldap._tcp.dc._msdcs.${domain} | Where-Object Type -eq SRV | Select-Object NameTarget,Port) -Compress`,
    ),
  );
  assert.ok(
    report.srvRecords.some(
      (record) =>
        record.Port === 389 &&
        record.NameTarget.toLowerCase().startsWith("netlab-dc."),
    ),
  );
  assert.equal(
    await powershell(
      member,
      '(Get-DnsClientServerAddress -AddressFamily IPv4 | Where-Object InterfaceAlias -notlike *Loopback*).ServerAddresses -join ","',
    ),
    environment.spec.assets.find((asset) => asset.name === "Domain controller")
      .interfaces[0].address,
  );
}

try {
  await api(
    "/sessions/login",
    "POST",
    JSON.parse(await readFile("data/dev-login.json", "utf8")),
  );
  await step("通过平台 API 创建双 Windows 环境，DNS 指向域控资产", async () => {
    const saved = JSON.parse(
      await readFile("data/windows-guest-state.json", "utf8"),
    );
    const templates = await api("/templates?limit=100");
    windows = templates.find(
      (template) =>
        template.id === saved.template.id && template.state === "ready",
    );
    container = templates.find(
      (template) =>
        template.name === "API container" && template.state === "ready",
    );
    assert.ok(windows && container, "prepared templates missing");
    const network = {
      id: randomUUID(),
      name: "AD LAN",
      cidr: "10.96.0.0/24",
      dnsAssetId: randomUUID(),
    };
    const assets = ["Domain controller", "Domain member"].map((name, i) => ({
      id: i === 0 ? network.dnsAssetId : randomUUID(),
      name,
      templateId: windows.id,
      resources: windows.resources,
      guest: {
        hostname: i === 0 ? "netlab-dc" : "netlab-member",
        username: "netlab",
      },
      interfaces: [
        {
          id: randomUUID(),
          networkId: network.id,
          address: "",
          mac: "",
          primary: true,
        },
      ],
    }));
    environment = process.env.NETLAB_AD_RESUME_ENVIRONMENT
      ? await api(`/environments/${process.env.NETLAB_AD_RESUME_ENVIRONMENT}`)
      : await api("/environments", "POST", {
          name: "AD native verification",
          run: true,
          spec: { networks: [network], assets },
          clientRequestId: randomUUID(),
        });
    report.environmentId = environment.id;
    await complete(environment.operationId);
    environment = await api(`/environments/${environment.id}`);
    const actual = (await api(`/environments/${environment.id}/state`)).assets;
    dc = actual.find(
      (asset) =>
        asset.assetId ===
        environment.spec.assets.find(
          (item) => item.name === "Domain controller",
        ).id,
    );
    member = actual.find(
      (asset) =>
        asset.assetId ===
        environment.spec.assets.find((item) => item.name === "Domain member")
          .id,
    );
    await Promise.all([boot(dc), boot(member)]);
    report.nodes = [dc.nodeId, member.nodeId];
  });
  await step("安装 AD DS 与 DNS，创建隔离域并验证成员加入", async () => {
    const system = JSON.parse(
      await powershell(
        dc,
        "Get-CimInstance Win32_ComputerSystem | Select-Object Domain,DomainRole | ConvertTo-Json -Compress",
      ),
    );
    if (system.Domain !== domain)
      await powershell(
        dc,
        `Set-LocalUser -Name Administrator -Password (ConvertTo-SecureString '${password}' -AsPlainText -Force); Install-WindowsFeature AD-Domain-Services -IncludeManagementTools | Out-Null; Install-ADDSForest -DomainName '${domain}' -DomainNetbiosName NETLAB -SafeModeAdministratorPassword (ConvertTo-SecureString '${password}' -AsPlainText -Force) -InstallDns -NoRebootOnCompletion -Force | Out-Null`,
        600_000,
      );
    if (system.DomainRole < 4) await reboot(dc);
    await ready(async () =>
      assert.equal(
        await powershell(
          dc,
          "(Get-Service NTDS,DNS,Netlogon | Where-Object Status -ne Running).Count",
        ),
        "0",
      ),
    );
    await powershell(
      dc,
      `Set-ADAccountPassword -Identity Administrator -Reset -NewPassword (ConvertTo-SecureString '${password}' -AsPlainText -Force)`,
    );
    await ready(verifyDns);
    if (
      (await powershell(
        member,
        "(Get-CimInstance Win32_ComputerSystem).Domain",
      )) !== domain
    ) {
      await powershell(
        member,
        `$credential=New-Object System.Management.Automation.PSCredential('Administrator@${domain}',(ConvertTo-SecureString '${password}' -AsPlainText -Force)); Add-Computer -DomainName '${domain}' -Credential $credential -Force`,
      );
      await reboot(member);
    }
    await verifyDomain();
  });
  await step("运行中增加容器，确认域服务、DNS 与安全通道保持有效", async () => {
    const previous = [dc.instanceId, member.instanceId];
    environment = await api(`/environments/${environment.id}`);
    const spec = structuredClone(environment.spec);
    if (!spec.assets.some((asset) => asset.name === "AD support container")) {
      spec.assets.push({
        id: randomUUID(),
        name: "AD support container",
        templateId: container.id,
        resources: container.resources,
        interfaces: [
          {
            id: randomUUID(),
            networkId: spec.networks[0].id,
            address: "",
            mac: "",
            primary: true,
          },
        ],
      });
      await complete(
        (
          await api(`/environments/${environment.id}/changes`, "POST", {
            expectedRevision: environment.revision,
            apply: true,
            spec,
            clientRequestId: randomUUID(),
          })
        ).id,
      );
    }
    environment = await api(`/environments/${environment.id}`);
    const actual = (await api(`/environments/${environment.id}/state`)).assets;
    assert.equal(actual.length, 3);
    assert.ok(
      previous.every((id) => actual.some((asset) => asset.instanceId === id)),
    );
    await verifyDomain();
  });
  await step(
    "VSS 磁盘恢复后更新 VM Generation ID，域控与成员正常恢复",
    async () => {
      const before = (
        await nodeCommand(dc, ["virsh", "dumpxml", dc.instanceId])
      ).match(/<genid>([^<]+)<\/genid>/)?.[1];
      assert.ok(before);
      point =
        (await api(`/environments/${environment.id}/recovery-points`)).find(
          (item) => item.name === "AD consistent recovery",
        ) ||
        (await api(`/environments/${environment.id}/recovery-points`, "POST", {
          name: "AD consistent recovery",
          expectedRevision: environment.revision,
        }));
      await complete(point.operationId);
      const stored = (
        await api(`/environments/${environment.id}/recovery-points`)
      ).find((item) => item.id === point.id);
      report.pointConsistency = stored.consistency;
      assert.equal(stored.consistency, "crash");
      report.windowsConsistency = [];
      for (const actual of [dc, member]) {
        const manifest = JSON.parse(
          await nodeCommand(actual, [
            "cat",
            `/var/lib/netlab-dev/recovery-points/${point.id}/${actual.assetId}/manifest.json`,
          ]),
        );
        assert.equal(manifest.consistency, "application");
        assert.equal(
          await qga(actual, { execute: "guest-fsfreeze-status" }),
          "thawed",
        );
        report.windowsConsistency.push({
          assetId: actual.assetId,
          consistency: manifest.consistency,
        });
      }
      await powershell(
        dc,
        "New-ADUser -Name 'AfterRecoveryPoint' -SamAccountName 'AfterRecoveryPoint'",
      );
      environment = await api(`/environments/${environment.id}`);
      await complete(
        (
          await api(
            `/environments/${environment.id}/recovery-points/${point.id}/restore`,
            "POST",
            { expectedRevision: environment.revision },
          )
        ).id,
      );
      const actual = (await api(`/environments/${environment.id}/state`))
        .assets;
      dc = actual.find((asset) => asset.assetId === dc.assetId);
      member = actual.find((asset) => asset.assetId === member.assetId);
      const after = (
        await nodeCommand(dc, ["virsh", "dumpxml", dc.instanceId])
      ).match(/<genid>([^<]+)<\/genid>/)?.[1];
      assert.ok(
        after && after !== before,
        "VM Generation ID did not change after disk restore",
      );
      report.generationIdChanged = true;
      await verifyDomain();
      assert.equal(
        await powershell(
          dc,
          "@(Get-ADUser -Filter \"SamAccountName -eq 'AfterRecoveryPoint'\").Count",
        ),
        "0",
      );
    },
  );
} catch (error) {
  report.error = clean(error);
  process.exitCode = 1;
  if (member)
    try {
      report.domainJoinLog = await powershell(
        member,
        "Get-Content C:\\Windows\\debug\\NetSetup.log -Tail 60",
      );
    } catch (diagnostic) {
      report.domainJoinLog = clean(diagnostic);
    }
  report.screenshots = [];
  for (const actual of [dc, member].filter(Boolean)) {
    const path = `/var/lib/netlab-dev/test-tmp/ad-${randomUUID()}.ppm`;
    try {
      await nodeCommand(actual, [
        "virsh",
        "screenshot",
        actual.instanceId,
        path,
      ]);
      await writeFile(
        `data/ad-${actual.assetId}.ppm`,
        Buffer.from(
          await nodeCommand(actual, ["base64", "-w0", path]),
          "base64",
        ),
      );
      report.screenshots.push(`data/ad-${actual.assetId}.ppm`);
      await nodeCommand(actual, ["rm", path]);
    } catch (diagnostic) {
      report.screenshots.push({ error: clean(diagnostic) });
    }
  }
} finally {
  if (point && !report.error)
    try {
      await complete(
        (
          await api(
            `/environments/${environment.id}/recovery-points/${point.id}`,
            "DELETE",
          )
        ).id,
      );
    } catch (error) {
      report.cleanupErrors.push(clean(error));
    }
  if (environment && !report.error)
    try {
      await complete(
        (
          await api(`/environments/${environment.id}/actions`, "POST", {
            action: "destroy",
            clientRequestId: randomUUID(),
          })
        ).id,
      );
      assert.equal(
        (await api(`/environments/${environment.id}/state`)).assets.length,
        0,
      );
    } catch (error) {
      report.cleanupErrors.push(clean(error));
    }
  report.finishedAt = new Date().toISOString();
  report.passed = !report.error && !report.cleanupErrors.length;
  if (!report.passed) process.exitCode = 1;
  await writeFile(
    "data/windows-ad-result.json",
    JSON.stringify(report, null, 2),
  );
  console.log(
    JSON.stringify({
      passed: report.passed,
      error: report.error,
      cleanupErrors: report.cleanupErrors,
    }),
  );
}
