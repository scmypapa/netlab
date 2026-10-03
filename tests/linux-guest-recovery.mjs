import assert from "node:assert/strict";
import { readFile, writeFile } from "node:fs/promises";
import { delay } from "./guest-ssh.mjs";
import { verifyLinuxGuest } from "./linux-guest.mjs";

const base = process.env.NETLAB_TEST_URL || "http://127.0.0.1:8090";
const report = { startedAt: new Date().toISOString(), passed: false };
let cookie;
async function api(path, method = "GET", body) {
  const response = await fetch(`${base}/api/v1${path}`, {
    method,
    headers: {
      "Content-Type": "application/json",
      ...(cookie ? { Cookie: cookie } : {}),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (response.headers.get("set-cookie"))
    cookie = response.headers.get("set-cookie").split(";")[0];
  const result = response.status === 204 ? undefined : await response.json();
  assert(
    response.ok,
    `${method} ${path}: ${response.status} ${JSON.stringify(result)}`,
  );
  return result;
}
async function completed(id) {
  for (const deadline = Date.now() + 180000; Date.now() < deadline;) {
    const operation = await api(`/operations/${id}`);
    if (
      ["succeeded", "failed", "partially_applied"].includes(operation.state)
    ) {
      assert.equal(
        operation.state,
        "succeeded",
        `${operation.phase}: ${operation.error}`,
      );
      return operation;
    }
    await delay(200);
  }
  throw new Error(`operation ${id} timed out`);
}

const started = performance.now();
try {
  await api(
    "/sessions/login",
    "POST",
    JSON.parse(await readFile("data/dev-login.json", "utf8")),
  );
  const template = (await api("/templates?kind=container&limit=100")).find(
    (item) => item.kind === "container" && item.state === "ready",
  );
  assert(template, "常驻环境没有已就绪的容器模板");
  report.result = await verifyLinuxGuest(api, completed, template);
  report.passed = true;
} catch (error) {
  report.error = error.message;
  process.exitCode = 1;
} finally {
  report.durationMs = Math.round(performance.now() - started);
  await writeFile(
    process.env.NETLAB_LINUX_RECOVERY_REPORT ||
      "data/linux-guest-recovery.json",
    JSON.stringify(report, null, 2),
  );
  console.log(JSON.stringify(report));
}
