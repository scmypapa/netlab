import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { readFile, writeFile } from "node:fs/promises";
import { createServer, request as httpsRequest } from "node:https";
import { delay } from "./guest-ssh.mjs";

const execute = promisify(execFile),
  run = randomUUID(),
  base = "http://127.0.0.1:8090",
  second = "http://127.0.0.1:8091";
const workers = new Map(
  JSON.parse(
    await readFile(
      "D:/.cache/netlab/artifacts/multi-node-workers.json",
      "utf8",
    ),
  ).map((worker) => [worker.nodeId, worker]),
);
const report = {
    startedAt: new Date().toISOString(),
    steps: [],
    cleanupErrors: [],
  },
  pools = [],
  clones = [];
const source = `/var/lib/netlab-dev/test-tmp/recovery-${run}`;
let cookie,
  environment,
  point,
  vmTemplate,
  containerTemplate,
  before,
  baseline,
  stoppedNode,
  readOnlyCapture;
const originalGenerations = new Map();
let tlsClient;
const quote = (value) => `'${String(value).replaceAll("'", "'\"'\"'")}'`;
async function node(id, ...args) {
  const w = workers.get(id),
    command = w.host
      ? [
          "ssh",
          "-i",
          w.keyPath,
          "-o",
          "BatchMode=yes",
          "-o",
          "StrictHostKeyChecking=yes",
          "-o",
          `UserKnownHostsFile=${w.keyPath}.hosts`,
          `root@${w.host}`,
          args.map(quote).join(" "),
        ]
      : args;
  try {
    return (
      await execute(
        "wsl.exe",
        ["-d", "Ubuntu", "-u", "root", "--exec", ...command],
        { timeout: 120000, maxBuffer: 2 ** 20 },
      )
    ).stdout;
  } catch (error) {
    error.message += `\n${error.stdout ?? ""}`;
    throw error;
  }
}
async function api(path, method = "GET", body, status = 200) {
  const response = await fetch(
    `${method === "DELETE" ? second : base}/api/v1${path}`,
    {
      method,
      headers: {
        "Content-Type": "application/json",
        ...(cookie ? { Cookie: cookie } : {}),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    },
  );
  if (response.headers.get("set-cookie"))
    cookie = response.headers.get("set-cookie").split(";")[0];
  const text = await response.text();
  assert.equal(response.status, status, `${method} ${path}: ${text}`);
  return text ? JSON.parse(text) : undefined;
}
async function operation(id, expected = "succeeded") {
  for (const deadline = Date.now() + 120000; Date.now() < deadline;) {
    const op = await api(`/operations/${id}`);
    if (["succeeded", "failed", "partially_applied"].includes(op.state)) {
      assert.equal(op.state, expected, `${op.phase}: ${op.error}`);
      return op;
    }
    await delay(150);
  }
  throw new Error(`operation timeout: ${id}`);
}
async function connected(ids, after = -Infinity) {
  for (const deadline = Date.now() + 30000; Date.now() < deadline;) {
    const nodes = await api("/nodes");
    if (
      ids.every((id) =>
        nodes.some(
          (n) =>
            n.id === id &&
            n.state === "ready" &&
            Date.parse(n.observedAt) > after,
        ),
      )
    )
      return;
    await delay(200);
  }
  throw new Error("test workers did not reconnect");
}
async function action(kind, assetId) {
  const op = await api(
    `/environments/${environment.id}${assetId ? `/assets/${assetId}` : ""}/actions`,
    "POST",
    { action: kind },
    202,
  );
  return operation(op.id);
}
async function restore() {
  const state = await api(`/environments/${environment.id}/state`);
  const op = await api(
    `/environments/${environment.id}/recovery-points/${point.id}/restore`,
    "POST",
    { expectedRevision: state.revision },
    202,
  );
  await operation(op.id);
  return op;
}
async function verifyRestore(op) {
  const state = await api(`/environments/${environment.id}/state`);
  assert.equal(state.assets.length, before.assets.length);
  for (const original of before.assets) {
    const actual = state.assets.find((a) => a.assetId === original.assetId);
    assert.equal(actual.instanceId, original.instanceId);
    assert.equal(actual.state, original.state);
    const root = pools.find((p) => p.nodeId === actual.nodeId).storage.path;
    const directory = `${root}/environments/${environment.id}/instances/${actual.instanceId}.${op.id}`;
    const manifest = JSON.parse(
      await node(original.nodeId, "cat", `${dir(original)}/manifest.json`),
    );
    if (manifest.execution.template.kind === "container") {
      if (actual.state === "suspended") await action("resume", actual.assetId);
      const content = await node(
        actual.nodeId,
        "ctr",
        "-n",
        "netlab",
        "tasks",
        "exec",
        "--exec-id",
        randomUUID(),
        actual.instanceId,
        "/bin/sh",
        "-c",
        'cat /root/recovery-marker /data/marker; test ! -e /usr/share/nginx/html/50x.html; test "$(stat -c %a /data/marker)" = 600; test "$(readlink /data/marker-link)" = marker',
      );
      assert.equal(content, "originaloriginal");
      const native = JSON.parse(
        await node(
          actual.nodeId,
          "ctr",
          "-n",
          "netlab",
          "containers",
          "info",
          actual.instanceId,
        ),
      );
      assert.equal(
        JSON.parse(native.Labels["netlab.execution"]).dataSetId,
        op.id,
      );
      if (actual.state === "suspended") await action("suspend", actual.assetId);
    } else {
      await node(
        actual.nodeId,
        "qemu-io",
        "-f",
        "qcow2",
        "-c",
        "read -P 0x59 0 4096",
        `${directory}/disk-0.qcow2`,
      );
      await node(
        actual.nodeId,
        "qemu-io",
        "-f",
        "qcow2",
        "-c",
        "read -P 0x59 0 4096",
        `${root}/environments/${environment.id}/volumes/${actual.assetId}/${op.id}/data.qcow2`,
      );
      const xml = await node(
        actual.nodeId,
        "virsh",
        "dumpxml",
        actual.instanceId,
        "--inactive",
      );
      assert(xml.includes(`<uuid>${actual.instanceId}</uuid>`));
      const generation = xml.match(/<genid>(.*?)<\/genid>/)?.[1];
      assert(generation);
      assert.notEqual(generation, originalGenerations.get(actual.instanceId));
      originalGenerations.set(actual.instanceId, generation);
      await node(
        actual.nodeId,
        "cmp",
        `${directory}/nvram.fd`,
        `${dir(original)}/nvram.fd`,
      );
      await node(
        actual.nodeId,
        "test",
        "-d",
        `/var/lib/libvirt/swtpm/${actual.instanceId}`,
      );
    }
    await node(actual.nodeId, "test", "!", "-e", `${directory}/before`);
  }
  return state;
}
async function step(name, fn) {
  const result = { name, passed: false },
    start = performance.now();
  try {
    await fn();
    result.passed = true;
  } catch (error) {
    result.error = error.message;
    throw error;
  } finally {
    result.durationMs = Math.round(performance.now() - start);
    report.steps.push(result);
    console.log(JSON.stringify(result));
  }
}
async function nodeRequest(endpoint, path, body) {
  return new Promise((resolve, reject) => {
    const request = httpsRequest(
      new URL(path, endpoint),
      {
        ...tlsClient,
        method: body === undefined ? "GET" : "POST",
        headers: { "Content-Type": "application/json" },
      },
      (response) => {
        let text = "";
        response.on("data", (chunk) => (text += chunk));
        response.on("end", () => {
          try {
            assert.equal(response.statusCode, 200, text);
            const result = JSON.parse(text);
            assert(!result.error, result.error);
            for (const item of result.results) assert(!item.error, item.error);
            resolve(result);
          } catch (e) {
            reject(e);
          }
        });
      },
    );
    request.on("error", reject);
    request.end(body === undefined ? undefined : JSON.stringify(body));
  });
}
function dir(actual) {
  return `${pools.find((p) => p.nodeId === actual.nodeId).storage.path}/recovery-points/${point.id}/${actual.assetId}`;
}
async function deletePoint() {
  if (!point) return;
  const found = (
    await api(`/environments/${environment.id}/recovery-points`)
  ).find((p) => p.id === point.id);
  if (!found) return;
  const op =
    found.state === "deleting"
      ? await api(
          `/operations/${found.operationId}/retry`,
          "POST",
          undefined,
          202,
        )
      : await api(
          `/environments/${environment.id}/recovery-points/${point.id}`,
          "DELETE",
          undefined,
          202,
        );
  await operation(op.id);
}
try {
  await api(
    "/sessions/login",
    "POST",
    JSON.parse(await readFile("data/dev-login.json", "utf8")),
  );
  await connected([...workers.keys()]);
  baseline = (await api("/nodes")).map((n) => ({
    id: n.id,
    reserved: n.reserved,
  }));
  tlsClient = {
    ca: await readFile("data/pki/ca.crt"),
    cert: await readFile("data/pki/controller.crt"),
    key: await readFile("data/pki/controller.key"),
    minVersion: "TLSv1.3",
  };
  await step(
    "双节点真实 containerd 与 UEFI、Secure Boot、TPM KVM 混合环境",
    async () => {
      const primary = [...workers.keys()].find((id) => !workers.get(id).host);
      for (const id of workers.keys()) {
        await node(id, "mkdir", "-p", source);
        await node(
          id,
          "qemu-img",
          "create",
          "-f",
          "qcow2",
          `${source}/base.qcow2`,
          "1G",
        );
        pools.push(
          await api(
            "/storage-pools",
            "POST",
            { nodeId: id, name: `Recovery ${run}`, directory: source },
            201,
          ),
        );
      }
      containerTemplate = (await api("/templates?kind=container")).find(
        (t) => t.state === "ready" && t.name.includes("container"),
      );
      assert(containerTemplate, "missing existing nginx container template");
      vmTemplate = await api(
        "/templates",
        "POST",
        {
          name: `Recovery UEFI ${run}`,
          kind: "vm",
          os: "Linux",
          version: 1,
          source: `${source}/base.qcow2`,
          format: "qcow2",
          resources: { cpu: 1, memoryMiB: 128, diskGiB: 1 },
          hardware: {
            machine: "q35",
            firmware: "uefi",
            secureBoot: true,
            tpm: true,
            diskBus: "virtio",
            nicModel: "virtio",
          },
        },
        201,
      );
      await operation(vmTemplate.operationId);
      vmTemplate = (await api(`/templates?ids=${vmTemplate.id}`))[0];
      const network = {
        id: randomUUID(),
        name: "Recovery LAN",
        cidr: "192.168.84.0/24",
      };
      const assets = pools.flatMap((pool) =>
        [containerTemplate, vmTemplate].map((t) => ({
          id: randomUUID(),
          name: `${t.kind}-${pool.nodeId.slice(0, 4)}`,
          templateId: t.id,
          storagePoolId: pool.id,
          resources: t.resources,
          interfaces: [
            {
              id: randomUUID(),
              networkId: network.id,
              mac: "",
              address: "",
              primary: true,
            },
          ],
          volumes: [{ id: "data", mountPath: "/data", sizeGiB: 1 }],
        })),
      );
      environment = await api(
        "/environments",
        "POST",
        {
          name: `Recovery ${run}`,
          spec: { networks: [network], assets },
          run: true,
        },
        201,
      );
      report.environmentId = environment.id;
      await operation(environment.operationId);
      await action("force-stop");
      before = await api(`/environments/${environment.id}/state`);
      for (const actual of before.assets) {
        const root = pools.find((p) => p.nodeId === actual.nodeId).storage.path;
        if (
          assets.find((a) => a.id === actual.assetId).templateId ===
          vmTemplate.id
        ) {
          for (const path of [
            `${root}/environments/${environment.id}/instances/${actual.instanceId}/disk-0.qcow2`,
            `${root}/environments/${environment.id}/volumes/${actual.assetId}/data.qcow2`,
          ])
            await node(
              actual.nodeId,
              "qemu-io",
              "-f",
              "qcow2",
              "-c",
              "write -P 0x59 0 4096",
              path,
            );
        } else {
          await action("start", actual.assetId);
          await node(
            actual.nodeId,
            "ctr",
            "-n",
            "netlab",
            "tasks",
            "exec",
            "--exec-id",
            randomUUID(),
            actual.instanceId,
            "/bin/sh",
            "-c",
            "printf original > /root/recovery-marker; rm /usr/share/nginx/html/50x.html; printf original > /data/marker; chmod 600 /data/marker; ln -s marker /data/marker-link",
          );
        }
      }
      before = await api(`/environments/${environment.id}/state`);
      const suspended = before.assets.find(
        (a) =>
          a.nodeId !== primary &&
          assets.find((x) => x.id === a.assetId).templateId ===
            containerTemplate.id,
      );
      await action("suspend", suspended.assetId);
      before = await api(`/environments/${environment.id}/state`);
      report.originalStates = before.assets.map((a) => ({
        assetId: a.assetId,
        instanceId: a.instanceId,
        nodeId: a.nodeId,
        state: a.state,
      }));
    },
  );
  await step("捕获写入真实失败、恢复原状态、解除故障后原任务重试", async () => {
    const pool = pools[1],
      path = `${pool.storage.path}/recovery-points`;
    await node(pool.nodeId, "mkdir", "-p", path);
    await node(pool.nodeId, "mount", "--bind", path, path);
    readOnlyCapture = { nodeId: pool.nodeId, path };
    await node(pool.nodeId, "mount", "-o", "remount,bind,ro", path);
    point = await api(
      `/environments/${environment.id}/recovery-points`,
      "POST",
      { name: `Before change ${run}`, expectedRevision: before.revision },
      201,
    );
    const failed = await operation(point.operationId, "failed");
    assert.match(failed.error, /read-only file system/);
    assert.equal(
      (await api(`/environments/${environment.id}/recovery-points`))[0].state,
      "failed",
    );
    const after = await api(`/environments/${environment.id}/state`);
    assert.equal(after.revision, before.revision);
    for (const original of before.assets) {
      const actual = after.assets.find((a) => a.assetId === original.assetId);
      assert.equal(actual.instanceId, original.instanceId);
      assert.equal(actual.state, original.state);
    }
    report.captureFailure = { phase: failed.phase, error: failed.error };
    await node(pool.nodeId, "umount", path);
    readOnlyCapture = undefined;
    await api(`/operations/${point.operationId}/retry`, "POST", undefined, 202);
  });
  await step("环境恢复点捕获、原实例和运行状态恢复、修订不变", async () => {
    await operation(point.operationId);
    await api(
      `/environments/${environment.id}/recovery-points`,
      "POST",
      { name: "Stale revision", expectedRevision: before.revision - 1 },
      409,
    );
    point = (await api(`/environments/${environment.id}/recovery-points`))[0];
    report.point = point;
    assert.equal(point.state, "ready");
    assert.equal(point.assetCount, 4);
    assert(point.sizeBytes > 0);
    const after = await api(`/environments/${environment.id}/state`);
    assert.equal(after.revision, before.revision);
    for (const original of before.assets) {
      const actual = after.assets.find((a) => a.assetId === original.assetId);
      assert.equal(actual.instanceId, original.instanceId);
      assert.equal(actual.state, original.state);
    }
  });
  await step(
    "容器可写层和卷原生还原核对；VM 全盘、NVRAM、TPM 固定数据核对",
    async () => {
      const primary = [...workers.keys()].find((id) => !workers.get(id).host);
      for (const actual of before.assets) {
        const directory = dir(actual),
          manifest = JSON.parse(
            await node(actual.nodeId, "cat", `${directory}/manifest.json`),
          );
        assert.equal(manifest.execution.instanceId, actual.instanceId);
        assert.equal(manifest.environmentId, environment.id);
        if (manifest.execution.template.kind === "container") {
          if (actual.state === "suspended")
            await action("resume", actual.assetId);
          await node(
            actual.nodeId,
            "ctr",
            "-n",
            "netlab",
            "tasks",
            "exec",
            "--exec-id",
            randomUUID(),
            actual.instanceId,
            "/bin/sh",
            "-c",
            "printf changed > /root/recovery-marker; printf changed > /data/marker",
          );
          if (actual.nodeId === primary)
            await node(
              primary,
              "env",
              `NETLAB_REAL_RECOVERY=${directory}`,
              "GOCACHE=/root/.cache/go-build",
              "GOMODCACHE=/root/go/pkg/mod",
              "TMPDIR=/var/lib/netlab-dev/test-tmp",
              "/usr/local/bin/go",
              "-C",
              "/mnt/d/newgz/netlab",
              "test",
              "-buildvcs=false",
              "-tags",
              "libvirt_dlopen",
              "./internal/engine",
              "-run",
              "^TestRealRecoveryArchive$",
              "-count=1",
              "-v",
            );
        } else {
          assert.equal(manifest.disks.length, 2);
          for (const disk of manifest.disks)
            await node(
              actual.nodeId,
              "qemu-io",
              "-f",
              "qcow2",
              "-c",
              "read -P 0x59 0 4096",
              `${directory}/${disk.file}`,
            );
          await node(actual.nodeId, "test", "-s", `${directory}/nvram.fd`);
          await node(actual.nodeId, "test", "-s", `${directory}/tpm.tar`);
          const xml = await node(
            actual.nodeId,
            "virsh",
            "dumpxml",
            actual.instanceId,
            "--inactive",
          );
          originalGenerations.set(
            actual.instanceId,
            xml.match(/<genid>(.*?)<\/genid>/)?.[1],
          );
          const root = pools.find((p) => p.nodeId === actual.nodeId).storage
            .path;
          for (const path of [
            `${root}/environments/${environment.id}/instances/${actual.instanceId}/disk-0.qcow2`,
            `${root}/environments/${environment.id}/volumes/${actual.assetId}/data.qcow2`,
          ])
            await node(
              actual.nodeId,
              "qemu-io",
              "-f",
              "qcow2",
              "-c",
              "write -P 0x63 0 4096",
              path,
            );
        }
      }
    },
  );
  await step(
    "跨节点流式传输与完整恢复准备；清理临时数据与原生租约",
    async () => {
      const nodes = await api("/nodes"),
        primary = [...workers.keys()].find((id) => !workers.get(id).host),
        secondary = [...workers.keys()].find((id) => workers.get(id).host);
      for (const original of before.assets.filter(
        (a) => a.nodeId === primary,
      )) {
        const manifest = JSON.parse(
            await node(primary, "cat", `${dir(original)}/manifest.json`),
          ),
          target = structuredClone(manifest.execution),
          opId = randomUUID(),
          pool = pools.find((p) => p.nodeId === secondary);
        target.dataSetId = opId;
        target.storagePath = pool.storage.path;
        target.storagePoolId = pool.id;
        target.storageFilesystem = pool.storage.filesystem;
        const plan = {
          operationId: opId,
          environmentId: environment.id,
          phase: "prepare-recovery",
          assets: [target],
          spec: environment.spec,
          recoveryPointId: point.id,
          recoverySources: {
            [target.asset.id]: {
              environmentId: environment.id,
              nodeId: primary,
              endpoint: nodes.find((n) => n.id === primary).endpoint,
              execution: manifest.execution,
            },
          },
          artifactEndpoints: Object.fromEntries(
            nodes.map((n) => [n.id, n.endpoint]),
          ),
        };
        const directory = `${pool.storage.path}/environments/${environment.id}/instances/${target.instanceId}.${opId}`;
        try {
          await nodeRequest(
            nodes.find((n) => n.id === secondary).endpoint,
            "/node/v1/plans",
            plan,
          );
          await node(secondary, "test", "-s", `${directory}/manifest.json`);
          if (target.template.kind === "vm") {
            await node(
              secondary,
              "qemu-io",
              "-f",
              "qcow2",
              "-c",
              "read -P 0x59 0 4096",
              `${directory}/disk-0.qcow2`,
            );
            await node(secondary, "test", "-s", `${directory}/tpm.tar`);
          } else {
            const native = JSON.parse(
              await node(secondary, "cat", `${directory}/container.json`),
            );
            await node(
              secondary,
              "ctr",
              "-n",
              "netlab",
              "snapshots",
              "info",
              native.SnapshotKey,
            );
          }
        } finally {
          await nodeRequest(
            nodes.find((n) => n.id === secondary).endpoint,
            "/node/v1/plans",
            {
              ...plan,
              phase: "rollback-recovery",
            },
          );
        }
        await node(secondary, "test", "!", "-e", directory);
        assert(
          !(
            await node(secondary, "ctr", "-n", "netlab", "leases", "ls", "-q")
          ).includes(`recovery-${target.instanceId}.${opId}`),
        );
      }
    },
  );
  await step(
    "恢复准备真实失败；原现场与修订保留；解除故障后原任务重试",
    async () => {
      const pool = pools[1],
        path = `${pool.storage.path}/environments/${environment.id}/instances`;
      await node(pool.nodeId, "mount", "--bind", path, path);
      readOnlyCapture = { nodeId: pool.nodeId, path };
      await node(pool.nodeId, "mount", "-o", "remount,bind,ro", path);
      const current = await api(`/environments/${environment.id}/state`);
      const op = await api(
        `/environments/${environment.id}/recovery-points/${point.id}/restore`,
        "POST",
        { expectedRevision: current.revision },
        202,
      );
      const failed = await operation(op.id, "failed");
      assert.match(failed.error, /read-only file system/);
      report.restoreFailure = { phase: failed.phase, error: failed.error };
      const after = await api(`/environments/${environment.id}/state`);
      assert.equal(after.revision, current.revision);
      for (const asset of current.assets) {
        const actual = after.assets.find((a) => a.assetId === asset.assetId);
        assert.equal(actual.instanceId, asset.instanceId);
        assert.equal(actual.state, asset.state);
      }
      await node(pool.nodeId, "umount", path);
      readOnlyCapture = undefined;
      await api(`/operations/${op.id}/retry`, "POST", undefined, 202);
      await operation(op.id);
      await verifyRestore(op);
      assert.equal(
        (await api(`/environments/${environment.id}/state`)).revision,
        current.revision + 1,
      );
      report.firstRestoreId = op.id;
    },
  );
  await step(
    "资产增删及网络变更后恢复原资产身份、配置、全盘和容器数据",
    async () => {
      const full = await api(`/environments/${environment.id}`),
        spec = structuredClone(full.spec);
      const removed = spec.assets.find((a) => a.templateId === vmTemplate.id);
      spec.assets = spec.assets.filter((a) => a.id !== removed.id);
      spec.assets.push({
        ...structuredClone(
          spec.assets.find((a) => a.templateId === containerTemplate.id),
        ),
        id: randomUUID(),
        name: "Temporary asset",
        interfaces: [
          {
            id: randomUUID(),
            networkId: spec.networks[0].id,
            mac: "",
            address: "",
            primary: true,
          },
        ],
      });
      spec.networks[0].name = "Changed network";
      const change = await api(
        `/environments/${environment.id}/changes`,
        "POST",
        { expectedRevision: full.revision, spec, apply: true },
        202,
      );
      await operation(change.id);
      const op = await restore();
      const state = await verifyRestore(op);
      assert.equal(state.revision, full.revision + 2);
      assert.equal(
        (await api(`/environments/${environment.id}`)).spec.networks[0].name,
        "Recovery LAN",
      );
      report.changedRestoreId = op.id;
    },
  );
  await step(
    "同一恢复点重复恢复，原 UUID 保留、代次改变、旧数据集合清理",
    async () => {
      const current = await api(`/environments/${environment.id}/state`),
        op = await restore();
      const state = await verifyRestore(op);
      assert.equal(state.revision, current.revision + 1);
      for (const actual of state.assets) {
        const root = pools.find((p) => p.nodeId === actual.nodeId).storage.path;
        await node(
          actual.nodeId,
          "test",
          "!",
          "-e",
          `${root}/environments/${environment.id}/instances/${actual.instanceId}.${report.changedRestoreId}`,
        );
      }
      report.repeatedRestoreId = op.id;
    },
  );
  await step(
    "同一恢复点并行克隆两个独立混合环境，数据隔离及默认停止",
    async () => {
      const original = await api(`/environments/${environment.id}/state`);
      const cloneStarted = Date.now();
      const request = {
        name: `Clone ${run}`,
        recoveryPointId: point.id,
        clientRequestId: run,
      };
      const created = await Promise.all([
        api("/environments", "POST", request, 201),
        api(
          "/environments",
          "POST",
          {
            name: `Clone running ${run}`,
            recoveryPointId: point.id,
            run: true,
          },
          201,
        ),
      ]);
      clones.push(...created);
      report.clones = clones.map((clone) => ({
        id: clone.id,
        operationId: clone.operationId,
      }));
      const replay = await api("/environments", "POST", request, 201);
      assert.equal(replay.id, created[0].id);
      const completed = await Promise.allSettled(
        created.map((clone) => operation(clone.operationId)),
      );
      for (const result of completed)
        if (result.status === "rejected") throw result.reason;
      report.cloneDeploymentMs = Date.now() - cloneStarted;
      const endpoints = new Map(
        (await api("/nodes")).map((n) => [n.id, n.endpoint]),
      );
      const cloneExecutions = new Map();
      for (let i = 0; i < created.length; i++) {
        const clone = created[i],
          state = await api(`/environments/${clone.id}/state`);
        assert.notEqual(clone.id, environment.id);
        assert.equal(state.revision, 1);
        assert.equal(state.assets.length, before.assets.length);
        assert.equal(state.status, i === 0 ? "stopped" : "running");
        const inventories = new Map(
          await Promise.all(
            [...workers.keys()].map(async (id) => [
              id,
              await nodeRequest(
                endpoints.get(id),
                `/node/v1/inventory?environmentId=${clone.id}`,
              ),
            ]),
          ),
        );
        for (const actual of state.assets) {
          const sourceAsset = before.assets.find(
            (a) => a.assetId === actual.assetId,
          );
          assert.notEqual(actual.instanceId, sourceAsset.instanceId);
          assert.equal(actual.state, i === 0 ? "stopped" : "running");
          const execution = inventories
            .get(actual.nodeId)
            .results.find((a) => a.instanceId === actual.instanceId).execution;
          assert(execution);
          cloneExecutions.set(actual.instanceId, execution);
          const root = execution.storagePath ?? "/var/lib/netlab-dev";
          const directory = `${root}/environments/${clone.id}/instances/${actual.instanceId}.${clone.operationId}`;
          const manifest = JSON.parse(
            await node(
              sourceAsset.nodeId,
              "cat",
              `${dir(sourceAsset)}/manifest.json`,
            ),
          );
          if (manifest.execution.template.kind === "container") {
            if (i === 0) {
              const start = await api(
                `/environments/${clone.id}/assets/${actual.assetId}/actions`,
                "POST",
                { action: "start" },
                202,
              );
              await operation(start.id);
            }
            const info = JSON.parse(
              await node(
                actual.nodeId,
                "ctr",
                "-n",
                "netlab",
                "containers",
                "info",
                actual.instanceId,
              ),
            );
            const recorded = JSON.parse(info.Labels["netlab.execution"]);
            assert.equal(info.Labels["netlab.environment"], clone.id);
            assert.equal(recorded.dataSetId, clone.operationId);
            for (const nic of execution.interfaces) {
              const originalNic = manifest.execution.interfaces.find(
                (n) => n.id === nic.id,
              );
              assert.notEqual(nic.portName, originalNic.portName);
              assert.equal(nic.mac, originalNic.mac);
              assert.equal(nic.address, originalNic.address);
            }
            const content = await node(
              actual.nodeId,
              "ctr",
              "-n",
              "netlab",
              "tasks",
              "exec",
              "--exec-id",
              randomUUID(),
              actual.instanceId,
              "/bin/sh",
              "-c",
              'cat /root/recovery-marker /data/marker; test ! -e /usr/share/nginx/html/50x.html; test "$(stat -c %a /data/marker)" = 600; test "$(readlink /data/marker-link)" = marker',
            );
            assert.equal(content, "originaloriginal");
            await node(
              actual.nodeId,
              "ctr",
              "-n",
              "netlab",
              "tasks",
              "exec",
              "--exec-id",
              randomUUID(),
              actual.instanceId,
              "/bin/sh",
              "-c",
              `printf clone-${i} > /root/recovery-marker; printf clone-${i} > /data/marker`,
            );
          } else {
            for (const path of [
              `${directory}/disk-0.qcow2`,
              `${root}/environments/${clone.id}/volumes/${actual.assetId}/${clone.operationId}/data.qcow2`,
            ]) {
              await node(
                actual.nodeId,
                "qemu-io",
                "-r",
                "-U",
                "-f",
                "qcow2",
                "-c",
                "read -P 0x59 0 4096",
                path,
              );
            }
            const xml = await node(
              actual.nodeId,
              "virsh",
              "dumpxml",
              actual.instanceId,
              "--inactive",
            );
            assert(xml.includes(`<uuid>${actual.instanceId}</uuid>`));
            assert.notEqual(
              xml.match(/<genid>(.*?)<\/genid>/)?.[1],
              originalGenerations.get(sourceAsset.instanceId),
            );
            assert(xml.includes(clone.id));
            if (i === 0) {
              const sourceHash = (
                await node(
                  sourceAsset.nodeId,
                  "sha256sum",
                  `${dir(sourceAsset)}/nvram.fd`,
                )
              ).split(/\s/)[0];
              const targetHash = (
                await node(actual.nodeId, "sha256sum", `${directory}/nvram.fd`)
              ).split(/\s/)[0];
              assert.equal(targetHash, sourceHash);
            }
            await node(
              actual.nodeId,
              "test",
              "-d",
              `/var/lib/libvirt/swtpm/${actual.instanceId}`,
            );
          }
          await node(actual.nodeId, "test", "!", "-e", `${directory}/before`);
          const leases = await node(
            actual.nodeId,
            "ctr",
            "-n",
            "netlab",
            "leases",
            "ls",
            "-q",
          );
          assert(
            !leases.includes(
              `recovery-${actual.instanceId}.${clone.operationId}`,
            ),
          );
        }
      }
      const after = await api(`/environments/${environment.id}/state`);
      assert.equal(after.revision, original.revision);
      assert.deepEqual(
        after.assets.map((a) => [a.instanceId, a.state]),
        original.assets.map((a) => [a.instanceId, a.state]),
      );
      for (const actual of before.assets) {
        const captured = JSON.parse(
          await node(actual.nodeId, "cat", `${dir(actual)}/manifest.json`),
        );
        if (captured.execution.template.kind !== "container") continue;
        if (actual.state === "suspended")
          await node(
            actual.nodeId,
            "ctr",
            "-n",
            "netlab",
            "tasks",
            "resume",
            actual.instanceId,
          );
        const content = await node(
          actual.nodeId,
          "ctr",
          "-n",
          "netlab",
          "tasks",
          "exec",
          "--exec-id",
          randomUUID(),
          actual.instanceId,
          "/bin/sh",
          "-c",
          "cat /root/recovery-marker /data/marker",
        );
        if (actual.state === "suspended")
          await node(
            actual.nodeId,
            "ctr",
            "-n",
            "netlab",
            "tasks",
            "pause",
            actual.instanceId,
          );
        assert.equal(content, "originaloriginal");
      }
      for (const clone of clones) {
        const targets = (await api(`/environments/${clone.id}/state`)).assets;
        const op = await api(
          `/environments/${clone.id}/actions`,
          "POST",
          { action: "destroy" },
          202,
        );
        await operation(op.id);
        assert.equal(
          (await api(`/environments/${clone.id}/state`)).assets.length,
          0,
        );
        for (const actual of targets) {
          const execution = cloneExecutions.get(actual.instanceId);
          const root = execution.storagePath ?? "/var/lib/netlab-dev";
          await node(
            actual.nodeId,
            "test",
            "!",
            "-e",
            `${root}/environments/${clone.id}/instances/${actual.instanceId}.${clone.operationId}`,
          );
          const remaining = await node(
            actual.nodeId,
            "sh",
            "-c",
            'if [ -d "$1" ]; then find "$1" -mindepth 1 -print -quit; fi',
            "sh",
            `${root}/environments/${clone.id}/volumes/${actual.assetId}/${clone.operationId}`,
          );
          assert.equal(remaining, "", "destroy left cloned volume data");
        }
        for (const worker of workers.keys()) {
          const inventory = await nodeRequest(
            endpoints.get(worker),
            `/node/v1/inventory?environmentId=${clone.id}`,
          );
          assert.equal(inventory.results.length, 0);
        }
      }
      report.clones = clones.map((clone) => ({
        id: clone.id,
        operationId: clone.operationId,
      }));
      clones.length = 0;
    },
  );
  await step(
    "节点切换成功但响应丢失；完整回滚原数据和状态；原任务重试",
    async () => {
      const primary = [...workers.keys()].find((id) => !workers.get(id).host),
        originalNode = (await api("/nodes")).find((n) => n.id === primary),
        current = await api(`/environments/${environment.id}/state`);
      const generationBefore = new Map();
      for (const actual of current.assets) {
        const a = environment.spec.assets.find((a) => a.id === actual.assetId),
          root = pools.find((p) => p.nodeId === actual.nodeId).storage.path;
        if (a.templateId === vmTemplate.id) {
          await node(
            actual.nodeId,
            "qemu-io",
            "-f",
            "qcow2",
            "-c",
            "write -P 0x73 0 4096",
            `${root}/environments/${environment.id}/instances/${actual.instanceId}.${report.repeatedRestoreId}/disk-0.qcow2`,
          );
          generationBefore.set(
            actual.instanceId,
            (
              await node(
                actual.nodeId,
                "virsh",
                "dumpxml",
                actual.instanceId,
                "--inactive",
              )
            ).match(/<genid>(.*?)<\/genid>/)?.[1],
          );
        } else {
          if (actual.state === "suspended")
            await action("resume", actual.assetId);
          await node(
            actual.nodeId,
            "ctr",
            "-n",
            "netlab",
            "tasks",
            "exec",
            "--exec-id",
            randomUUID(),
            actual.instanceId,
            "/bin/sh",
            "-c",
            "printf before-failure > /root/recovery-marker; printf before-failure > /data/marker",
          );
          if (actual.state === "suspended")
            await action("suspend", actual.assetId);
        }
      }
      let injected = false;
      const proxy = createServer(
        {
          ca: tlsClient.ca,
          cert: await node(
            primary,
            "cat",
            "/home/fisher/.local/share/netlab-dev/pki/node.crt",
          ),
          key: await node(
            primary,
            "cat",
            "/home/fisher/.local/share/netlab-dev/pki/node.key",
          ),
          requestCert: true,
          rejectUnauthorized: true,
          minVersion: "TLSv1.3",
        },
        (request, response) => {
          const chunks = [];
          request.on("data", (chunk) => chunks.push(chunk));
          request.on("end", () => {
            const body = Buffer.concat(chunks),
              plan =
                request.url === "/node/v1/plans" ? JSON.parse(body) : undefined,
              lost = !injected && plan?.phase === "apply-recovery";
            if (lost) injected = true;
            const upstream = httpsRequest(
              new URL(request.url, originalNode.endpoint),
              {
                ...tlsClient,
                method: request.method,
                headers: request.headers,
              },
              (reply) => {
                if (lost) {
                  let text = "";
                  reply.on("data", (chunk) => (text += chunk));
                  reply.on("end", () => {
                    report.lostResponse = JSON.parse(text);
                    response.writeHead(503);
                    response.end("Injected lost recovery response");
                  });
                } else {
                  response.writeHead(reply.statusCode, reply.headers);
                  reply.pipe(response);
                }
              },
            );
            upstream.on("error", (error) => {
              response.writeHead(502);
              response.end(error.message);
            });
            response.on("close", () => upstream.destroy());
            upstream.end(body);
          });
        },
      );
      await new Promise((resolve) => proxy.listen(0, "127.0.0.1", resolve));
      try {
        await api(
          "/nodes",
          "POST",
          {
            name: originalNode.name,
            endpoint: `https://127.0.0.1:${proxy.address().port}`,
          },
          201,
        );
        const op = await api(
            `/environments/${environment.id}/recovery-points/${point.id}/restore`,
            "POST",
            { expectedRevision: current.revision },
            202,
          ),
          failed = await operation(op.id, "failed");
        assert(injected);
        assert.equal(failed.phase, "rolled-back");
        assert.match(failed.error, /Injected lost recovery response/);
        assert(report.lostResponse.results.every((r) => !r.error));
        const after = await api(`/environments/${environment.id}/state`);
        assert.equal(after.revision, current.revision);
        for (const original of current.assets) {
          const actual = after.assets.find(
            (a) => a.assetId === original.assetId,
          );
          assert.equal(actual.instanceId, original.instanceId);
          assert.equal(actual.state, original.state);
          const root = pools.find((p) => p.nodeId === actual.nodeId).storage
            .path;
          if (generationBefore.has(actual.instanceId)) {
            const xml = await node(
              actual.nodeId,
              "virsh",
              "dumpxml",
              actual.instanceId,
              "--inactive",
            );
            assert.equal(
              xml.match(/<genid>(.*?)<\/genid>/)?.[1],
              generationBefore.get(actual.instanceId),
            );
            await node(
              actual.nodeId,
              "qemu-io",
              "-f",
              "qcow2",
              "-c",
              "read -P 0x73 0 4096",
              `${root}/environments/${environment.id}/instances/${actual.instanceId}.${report.repeatedRestoreId}/disk-0.qcow2`,
            );
          } else {
            if (actual.state === "suspended")
              await node(
                actual.nodeId,
                "ctr",
                "-n",
                "netlab",
                "tasks",
                "resume",
                actual.instanceId,
              );
            assert.equal(
              await node(
                actual.nodeId,
                "ctr",
                "-n",
                "netlab",
                "tasks",
                "exec",
                "--exec-id",
                randomUUID(),
                actual.instanceId,
                "/bin/sh",
                "-c",
                "cat /root/recovery-marker /data/marker",
              ),
              "before-failurebefore-failure",
            );
            if (actual.state === "suspended")
              await node(
                actual.nodeId,
                "ctr",
                "-n",
                "netlab",
                "tasks",
                "pause",
                actual.instanceId,
              );
          }
          await node(
            actual.nodeId,
            "test",
            "!",
            "-e",
            `${root}/environments/${environment.id}/instances/${actual.instanceId}.${op.id}`,
          );
        }
        await api(`/operations/${op.id}/retry`, "POST", undefined, 202);
        await operation(op.id);
        await verifyRestore(op);
        report.lostResponseRestoreId = op.id;
      } finally {
        await api(
          "/nodes",
          "POST",
          { name: originalNode.name, endpoint: originalNode.endpoint },
          201,
        );
        proxy.closeAllConnections();
        await new Promise((resolve) => proxy.close(resolve));
      }
    },
  );
  await step("销毁环境保留恢复点；模板和存储引用仍阻止误删", async () => {
    await action("destroy");
    await api(`/templates/${vmTemplate.id}`, "DELETE", undefined, 409);
    for (const pool of pools)
      await api(`/storage-pools/${pool.id}`, "DELETE", undefined, 409);
    const points = await api(`/environments/${environment.id}/recovery-points`);
    assert.equal(points[0].state, "ready");
    for (const actual of before.assets)
      await node(actual.nodeId, "test", "-s", `${dir(actual)}/manifest.json`);
  });
  await step(
    "已销毁环境从恢复点重新创建，原 UUID 和数据保持，随后完整销毁",
    async () => {
      const current = await api(`/environments/${environment.id}/state`);
      const op = await restore();
      const state = await verifyRestore(op);
      assert.equal(state.revision, current.revision + 1);
      await action("destroy");
      assert.deepEqual(
        (await api(`/environments/${environment.id}/state`)).assets,
        [],
      );
      report.destroyedRestoreId = op.id;
    },
  );
  await step("离线删除真实失败，原任务重试；销毁状态保持不变", async () => {
    stoppedNode = [...workers.keys()].find((id) => workers.get(id).host);
    await node(stoppedNode, "systemctl", "stop", "netlab-node-dev.service");
    const op = await api(
      `/environments/${environment.id}/recovery-points/${point.id}`,
      "DELETE",
      undefined,
      202,
    );
    await operation(op.id, "failed");
    assert.equal(
      (await api(`/environments/${environment.id}/state`)).status,
      "destroyed",
    );
    assert.equal(
      (await api(`/environments/${environment.id}/recovery-points`))[0].state,
      "deleting",
    );
    const restarting = stoppedNode,
      restartTime = Date.now();
    await node(stoppedNode, "systemctl", "start", "netlab-node-dev.service");
    stoppedNode = undefined;
    await connected([restarting], restartTime);
    await api(`/operations/${op.id}/retry`, "POST", undefined, 202);
    await operation(op.id);
    assert.deepEqual(
      await api(`/environments/${environment.id}/recovery-points`),
      [],
    );
    assert.equal(
      (await api(`/environments/${environment.id}/state`)).status,
      "destroyed",
    );
    for (const actual of before.assets) {
      await node(actual.nodeId, "test", "!", "-e", dir(actual));
      await node(actual.nodeId, "test", "!", "-e", `${dir(actual)}.pending`);
    }
    point = undefined;
  });
  report.passed = true;
} catch (error) {
  report.passed = false;
  report.error = error.message;
} finally {
  if (readOnlyCapture)
    try {
      await node(readOnlyCapture.nodeId, "umount", readOnlyCapture.path);
    } catch (e) {
      report.cleanupErrors.push(e.message);
    }
  if (stoppedNode)
    try {
      await node(stoppedNode, "systemctl", "start", "netlab-node-dev.service");
    } catch (e) {
      report.cleanupErrors.push(e.message);
    }
  for (const clone of clones) {
    try {
      const state = await api(`/environments/${clone.id}/state`);
      if (state.status !== "destroyed") {
        const op = await api(
          `/environments/${clone.id}/actions`,
          "POST",
          { action: "destroy" },
          202,
        );
        await operation(op.id);
      }
    } catch (e) {
      report.cleanupErrors.push(e.message);
    }
  }
  try {
    await deletePoint();
  } catch (e) {
    report.cleanupErrors.push(e.message);
  }
  if (environment)
    try {
      const state = await api(`/environments/${environment.id}/state`);
      if (state.status !== "destroyed") await action("destroy");
    } catch (e) {
      report.cleanupErrors.push(e.message);
    }
  if (vmTemplate)
    try {
      const op = await api(
        `/templates/${vmTemplate.id}`,
        "DELETE",
        undefined,
        202,
      );
      await operation(op.id);
    } catch (e) {
      report.cleanupErrors.push(e.message);
    }
  for (const pool of pools)
    try {
      const op = await api(
        `/storage-pools/${pool.id}`,
        "DELETE",
        undefined,
        202,
      );
      await operation(op.id);
      await node(pool.nodeId, "rm", "-r", "--", source);
    } catch (e) {
      report.cleanupErrors.push(e.message);
    }
  try {
    const nodes = await api("/nodes");
    for (const expected of baseline ?? [])
      assert.deepEqual(
        nodes.find((n) => n.id === expected.id).reserved,
        expected.reserved,
      );
  } catch (e) {
    report.cleanupErrors.push(e.message);
  }
  if (report.cleanupErrors.length) report.passed = false;
  report.finishedAt = new Date().toISOString();
  await writeFile(
    process.env.NETLAB_RECOVERY_REPORT ?? "data/recovery-restore-result.json",
    JSON.stringify(report, null, 2),
  );
  console.log(
    JSON.stringify({
      passed: report.passed,
      error: report.error,
      cleanupErrors: report.cleanupErrors,
    }),
  );
  if (!report.passed) process.exitCode = 1;
}
