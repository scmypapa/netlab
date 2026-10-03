import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";

export const wsl = (...args) =>
  execFileSync("wsl.exe", ["-d", "Ubuntu", "-u", "root", "--exec", ...args], {
    encoding: "utf8",
    stdio: "pipe",
    timeout: 10_000,
  });
export const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

export function guestKey() {
  const path = `/var/lib/netlab-dev/tests/guest-${randomUUID()}`;
  wsl("mkdir", "-p", "/var/lib/netlab-dev/tests");
  wsl("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path);
  return {
    path,
    publicKey: wsl("cat", `${path}.pub`).trim(),
    remove: () => wsl("rm", "-f", "--", path, `${path}.pub`, `${path}.hosts`),
  };
}

export async function guestSSH(clientInstance, instanceId, address, key) {
  const network = (...args) => {
    const task = wsl("ctr", "-n", "netlab", "tasks", "list")
      .split("\n")
      .find((line) => line.trim().split(/\s+/)[0] === clientInstance);
    const pid = task?.trim().split(/\s+/)[1];
    assert.match(pid || "", /^\d+$/, "客户端没有真实containerd进程");
    return wsl("nsenter", `--net=/proc/${pid}/ns/net`, ...args);
  };
  const hosts = `${key.path}.hosts`;
  const run = (...command) =>
    network(
      "ssh",
      "-i",
      key.path,
      "-o",
      "BatchMode=yes",
      "-o",
      "StrictHostKeyChecking=yes",
      "-o",
      `UserKnownHostsFile=${hosts}`,
      "-o",
      `HostKeyAlias=${instanceId}`,
      "-o",
      "ConnectTimeout=3",
      `netlab@${address()}`,
      ...command,
    );
  const ready = async () => {
    const deadline = Date.now() + 120_000;
    let error;
    while (Date.now() < deadline) {
      try {
        return run("cat", "/proc/sys/kernel/random/boot_id").trim();
      } catch (failure) {
        error = failure;
        await delay(500);
      }
    }
    throw new Error(`来宾SSH在120秒内未就绪：${error?.message}`);
  };
  let scanned = "";
  const deadline = Date.now() + 120_000;
  while (!scanned.trim() && Date.now() < deadline) {
    try {
      scanned = network("ssh-keyscan", "-T", "2", "-t", "ed25519", address());
    } catch {
      await delay(500);
    }
  }
  assert.ok(scanned.trim(), "来宾SSH在120秒内未启动");
  const knownHost = scanned
    .split("\n")
    .filter((line) => line && !line.startsWith("#"))
    .map((line) => `${instanceId} ${line.split(" ").slice(1).join(" ")}`)
    .join("\n");
  execFileSync(
    "wsl.exe",
    ["-d", "Ubuntu", "-u", "root", "--exec", "tee", "-a", hosts],
    {
      input: `${knownHost}\n`,
      stdio: ["pipe", "ignore", "pipe"],
      timeout: 10_000,
    },
  );
  await ready();
  return { run, ready };
}
