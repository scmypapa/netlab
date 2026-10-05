import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { readFile } from "node:fs/promises";
import { promisify } from "node:util";
import { delay } from "./guest-ssh.mjs";

const execute = promisify(execFile);
const workers = new Map(
  JSON.parse(
    await readFile(
      "D:/.cache/netlab/artifacts/multi-node-workers.json",
      "utf8",
    ),
  ).map((worker) => [worker.nodeId, worker]),
);
const quote = (value) => `'${String(value).replaceAll("'", "'\"'\"'")}'`;

export async function nodeCommand(actual, command) {
  const worker = workers.get(actual.nodeId);
  assert.ok(worker, "node connection missing");
  const args = worker.host
    ? [
        "ssh",
        "-i",
        worker.keyPath,
        "-o",
        "BatchMode=yes",
        "-o",
        "StrictHostKeyChecking=yes",
        "-o",
        `UserKnownHostsFile=${worker.keyPath}.hosts`,
        `root@${worker.host}`,
        command.map(quote).join(" "),
      ]
    : command;
  try {
    return (
      await execute(
        "wsl.exe",
        ["-d", "Ubuntu", "-u", "root", "--exec", ...args],
        {
          encoding: "utf8",
          timeout: 30_000,
          maxBuffer: 4 << 20,
        },
      )
    ).stdout;
  } catch (error) {
    // execFile.message includes its command arguments, which can contain encoded guest credentials.
    throw new Error(
      `Node command ${command[0]} ${command[1] || ""} failed (${error.code ?? error.signal}): ${error.stderr || ""}`,
    );
  }
}

export async function qga(actual, request) {
  const response = JSON.parse(
    await nodeCommand(actual, [
      "virsh",
      "qemu-agent-command",
      actual.instanceId,
      JSON.stringify(request),
    ]),
  );
  assert.ok(!response.error, JSON.stringify(response.error));
  return response.return;
}

export async function ready(probe, timeout = 240_000) {
  for (const deadline = Date.now() + timeout; ;) {
    try {
      return await probe();
    } catch (error) {
      if (Date.now() >= deadline) throw error;
      await delay(1000);
    }
  }
}

export async function powershell(actual, script, timeout = 180_000) {
  const command = await qga(actual, {
    execute: "guest-exec",
    arguments: {
      path: "C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe",
      arg: [
        "-NoProfile",
        "-NonInteractive",
        "-EncodedCommand",
        Buffer.from(
          "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'; " +
            script,
          "utf16le",
        ).toString("base64"),
      ],
      "capture-output": true,
    },
  });
  for (const deadline = Date.now() + timeout; Date.now() < deadline;) {
    const status = await qga(actual, {
      execute: "guest-exec-status",
      arguments: { pid: command.pid },
    });
    if (status.exited) {
      const output = Buffer.from(status["out-data"] || "", "base64")
        .toString()
        .trim();
      assert.equal(
        status.exitcode,
        0,
        output +
          "\n" +
          Buffer.from(status["err-data"] || "", "base64").toString(),
      );
      return output;
    }
    await delay(500);
  }
  throw new Error("Windows guest command timed out");
}

export async function reboot(actual) {
  const readBoot = () =>
    powershell(
      actual,
      "(Get-CimInstance Win32_OperatingSystem).LastBootUpTime.Ticks.ToString()",
    );
  const before = await readBoot();
  await powershell(actual, "shutdown.exe /r /t 3 | Out-Null");
  const after = await ready(async () => {
    const value = await readBoot();
    assert.notEqual(value, before, "Windows has not restarted");
    return value;
  });
  return { before, after };
}
