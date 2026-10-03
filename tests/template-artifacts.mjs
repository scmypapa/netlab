import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { execFile } from 'node:child_process'
import { readFile, writeFile } from 'node:fs/promises'
import { promisify } from 'node:util'
import { delay, guestKey, guestSSH, wsl } from './guest-ssh.mjs'

const execute = promisify(execFile)
const base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const report = { startedAt: new Date().toISOString(), steps: [], environmentIds: [] }
const envs = [], workers = new Map()
const runId = randomUUID(), directory = `/var/lib/netlab-dev/tests/artifacts-${runId}`
const quote = value => `'${String(value).replaceAll("'", "'\"'\"'")}'`
let cookie, baseline, primary, origin, container, vm, key, sourceStopped = false

async function api(path, method = 'GET', body) {
  const response = await fetch(`${base}/api/v1${path}`, { method, headers: { Cookie: cookie || '', 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(15000) })
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie')?.split(';')[0]
  const value = response.status === 204 ? undefined : await response.json()
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${JSON.stringify(value)}`)
  if (path === '/templates' && method === 'POST') {
    const location = response.headers.get('Operation-Location')
    assert.match(location || '', /^\/api\/v1\/operations\/[\da-f-]+$/)
    value.operationId = location.split('/').at(-1)
  }
  return value
}
async function step(name, run) {
  const began = performance.now(), result = { name, passed: false }
  try { await run(); result.passed = true } catch (error) { result.error = error.message; throw error } finally { result.durationMs = Math.round(performance.now() - began); report.steps.push(result); console.log(JSON.stringify(result)) }
}
async function node(id, ...args) {
  const worker = workers.get(id)
  const command = worker ? ['ssh', '-i', worker.keyPath, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${worker.keyPath}.hosts`, `root@${worker.host}`, args.map(quote).join(' ')] : args
  const result = await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', ...command], { encoding: 'utf8', timeout: 60000, maxBuffer: 2 ** 20 })
  return result.stdout.trim()
}
async function operation(id, expected = 'succeeded') {
  const deadline = Date.now() + 180000
  while (Date.now() < deadline) {
    const value = await api(`/operations/${id}`)
    if (['succeeded', 'failed', 'partially_applied'].includes(value.state)) { assert.equal(value.state, expected, `${value.phase}: ${value.error}`); return value }
    await delay(200)
  }
  throw new Error(`operation timeout ${id}`)
}
async function prepared(input) {
  const created = await api('/templates', 'POST', input), deadline = Date.now() + 180000
  await operation(created.operationId)
  while (Date.now() < deadline) {
    const [value] = await api(`/templates?ids=${created.id}`)
    if (value.state !== 'importing') { assert.equal(value.state, 'ready', value.error); assert.equal(value.artifactNodeId, origin); return value }
    await delay(200)
  }
  throw new Error(`template import timeout ${created.id}`)
}
async function create(name, containerCount, vmCount) {
  const networkId = randomUUID()
  const templates = [...Array(containerCount).fill(container), ...Array(vmCount).fill(vm)]
  const spec = { networks: [{ id: networkId, name: '业务网', cidr: '10.86.0.0/24', mtu: 1400 }], assets: templates.map((template, index) => ({ id: randomUUID(), name: `${template.kind}-${index}`, templateId: template.id, resources: template.resources, interfaces: [{ id: randomUUID(), networkId, mac: '', address: '', primary: true }], ...(template.kind === 'vm' ? { guest: { username: 'netlab', sshAuthorizedKeys: [key.publicKey] } } : {}) })) }
  const env = await api('/environments', 'POST', { name, spec, run: true, clientRequestId: randomUUID() })
  envs.push(env); report.environmentIds.push(env.id)
  await operation(env.operationId)
  return env
}
const state = env => api(`/environments/${env.id}/state`)
async function verifyGuests(env) {
  const current = await api(`/environments/${env.id}`), actual = await state(env)
  const client = actual.assets.find(asset => asset.nodeId === primary && current.spec.assets.find(item => item.id === asset.assetId).templateId === container.id)
  assert.ok(client, '自动放置未提供主节点客户端')
  for (const target of actual.assets.filter(asset => current.spec.assets.find(item => item.id === asset.assetId).templateId === vm.id)) {
    const asset = current.spec.assets.find(item => item.id === target.assetId)
    const ssh = await guestSSH(client.instanceId, target.instanceId, () => asset.interfaces[0].address, key)
    assert.match(ssh.run('cat', '/etc/os-release'), /Ubuntu/)
    assert.ok(ssh.run('cat', '/proc/sys/kernel/random/boot_id').trim())
  }
}
async function nodeState(id, expected) {
  const deadline = Date.now() + 45000
  while (Date.now() < deadline) {
    const value = (await api('/nodes')).find(item => item.id === id)
    if (expected === 'ready' ? value.state === 'ready' : value.state !== 'ready') return
    await delay(200)
  }
  throw new Error(`node state timeout ${id}: ${expected}`)
}
async function destroy(env) {
  if ((await api(`/environments/${env.id}`)).status === 'destroyed') return
  await operation((await api(`/environments/${env.id}/actions`, 'POST', { action: 'destroy', clientRequestId: randomUUID() })).id)
}

try {
  await step('单节点导入容器包与真实 Ubuntu，持久记录不可变制品', async () => {
    await api('/sessions/login', 'POST', JSON.parse(await readFile('data/dev-login.json', 'utf8')))
    baseline = await api('/nodes'); assert.equal(baseline.length, 2)
    await Promise.all(baseline.map(item => nodeState(item.id, 'ready')))
    primary = wsl('cat', '/var/lib/netlab-dev/node-id').trim()
    const connections = JSON.parse(await readFile('D:/.cache/netlab/artifacts/multi-node-workers.json', 'utf8'))
    for (const worker of connections) workers.set(worker.nodeId, worker.host ? worker : null)
    origin = baseline.toSorted((a, b) => a.id.localeCompare(b.id))[0].id
    assert.notEqual(origin, primary, '本用例用 Guest Worker 导入，主节点首次远程获取')
    report.originNodeId = origin
    const templates = await api('/templates?limit=100')
    const ubuntu = templates.find(item => item.kind === 'vm' && item.os === 'Ubuntu 24.04' && item.state === 'ready' && item.source === '/var/lib/netlab-dev/templates/ubuntu-24.04.qcow2')
    assert.ok(ubuntu)
    key = guestKey(); await node(origin, 'mkdir', '-p', directory)
    const worker = workers.get(origin)
    const initial = await api('/templates', 'POST', { name: `固定容器 ${runId}`, kind: 'container', os: 'Linux', version: 1, source: `${directory}/container.tar`, format: 'oci', resources: { cpu: 1, memoryMiB: 128, diskGiB: 1 } })
    const failed = await operation(initial.operationId, 'failed')
    assert.match(failed.error, /no such file or directory/)
    report.expectedImportFailure = { operationId: initial.operationId, error: failed.error }
    await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', 'scp', '-i', worker.keyPath, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${worker.keyPath}.hosts`, '/mnt/d/newgz/netlab/data/nginx.tar', `root@${worker.host}:${directory}/container.tar`], { timeout: 60000 })
    await node(origin, 'cp', '--reflink=auto', ubuntu.source, `${directory}/system.qcow2`)
    await node(primary, 'test', '!', '-e', directory)
    const retried = await api(`/operations/${initial.operationId}/retry`, 'POST')
    assert.equal(retried.id, initial.operationId)
    await operation(retried.id)
    ;[container] = await api(`/templates?ids=${initial.id}`)
    assert.equal(container.state, 'ready'); assert.equal(container.artifactNodeId, origin)
    vm = await prepared({ name: `固定 Ubuntu ${runId}`, kind: 'vm', os: ubuntu.os, version: 1, source: `${directory}/system.qcow2`, format: 'qcow2', resources: ubuntu.resources, hardware: ubuntu.hardware, initialization: ubuntu.initialization })
    report.templates = [container, vm].map(({ id, kind, artifactNodeId }) => ({ id, kind, artifactNodeId }))
    for (const template of [container, vm]) await node(origin, 'test', '-f', `/var/lib/netlab-dev/artifacts/${template.id}/1/template.json`)
    await node(origin, 'rm', '--', `${directory}/container.tar`, `${directory}/system.qcow2`)
  })
  await step('原始文件已移除，自动跨双 Worker 创建 4 容器和 2 台 Ubuntu', async () => {
    const env = await create('不可变制品双节点验收', 4, 2), actual = await state(env)
    report.placement = actual.assets.map(({ assetId, nodeId, instanceId }) => ({ environmentId: env.id, assetId, nodeId, instanceId }))
    assert.equal(new Set(actual.assets.map(asset => asset.nodeId)).size, 2)
    assert.ok(actual.assets.every(asset => asset.state === 'running'))
    for (const template of [container, vm]) {
      const file = template.kind === 'vm' ? 'disk-0.qcow2' : 'image.tar'
      const hash = id => node(id, 'sha256sum', `/var/lib/netlab-dev/artifacts/${template.id}/1/${file}`).then(value => value.split(' ')[0])
      const [original, copied] = await Promise.all([hash(origin), hash(primary)])
      assert.equal(copied, original); report[`${template.kind}Artifact`] = { sha256: original }
    }
    await verifyGuests(env)
    const current = await api(`/environments/${env.id}`), binaries = []
    for (const target of actual.assets.filter(item => current.spec.assets.find(asset => asset.id === item.assetId).templateId === container.id)) {
      const digest = await node(target.nodeId, 'ctr', '-n', 'netlab', 'tasks', 'exec', '--exec-id', randomUUID(), target.instanceId, 'sha256sum', '/usr/sbin/nginx')
      binaries.push(digest.split(' ')[0])
    }
    assert.equal(binaries.length, 4); assert.equal(new Set(binaries).size, 1)
    report.containerBinary = binaries[0]
  })
  await step('源 Agent 停止后，以已有缓存创建新的混合环境并真实 SSH', async () => {
    await node(origin, 'systemctl', 'stop', 'netlab-node-dev.service'); sourceStopped = true
    await nodeState(origin, 'offline')
    const env = await create('制品缓存离线验收', 2, 1)
    const actual = await state(env)
    assert.ok(actual.assets.every(asset => asset.nodeId === primary))
    report.placement.push(...actual.assets.map(({ assetId, nodeId, instanceId }) => ({ environmentId: env.id, assetId, nodeId, instanceId })))
    await verifyGuests(env)
    await node(origin, 'systemctl', 'start', 'netlab-node-dev.service'); sourceStopped = false
    await nodeState(origin, 'ready')
  })
  await step('销毁两环境，容量、实例、网络和运行数据无残留', async () => {
    for (const env of envs) await destroy(env)
    const nodes = await api('/nodes')
    for (const previous of baseline) assert.deepEqual(nodes.find(item => item.id === previous.id).reserved, previous.reserved)
    for (const env of envs) {
      assert.equal((await state(env)).assets.length, 0)
    }
    for (const target of report.placement) {
      await node(target.nodeId, 'test', '!', '-e', `/var/lib/netlab-dev/environments/${target.environmentId}/instances/${target.instanceId}`)
      assert.ok(!(await node(target.nodeId, 'ctr', '-n', 'netlab', 'containers', 'list')).includes(target.instanceId))
      assert.ok(!(await node(target.nodeId, 'virsh', 'list', '--all', '--uuid')).includes(target.instanceId))
      assert.ok(!(await node(target.nodeId, 'ovs-vsctl', '--columns=external_ids', 'list', 'Interface')).includes(target.instanceId))
    }
    for (const table of ['Logical_Switch', 'Logical_Switch_Port', 'Logical_Router', 'Logical_Router_Port']) {
      const output = await node(primary, 'ovn-nbctl', '--columns=external_ids', 'list', table)
      for (const env of envs) assert.ok(!output.includes(env.id), `${table} 残留`)
    }
    await node(origin, 'rmdir', '--', directory)
  })
  report.passed = true
} catch (error) { report.passed = false; report.error = error.message; process.exitCode = 1 } finally {
  report.cleanupErrors = []
  if (sourceStopped) try { await node(origin, 'systemctl', 'start', 'netlab-node-dev.service'); await nodeState(origin, 'ready') } catch (error) { report.cleanupErrors.push(error.message) }
  for (const env of envs) try { await destroy(env) } catch (error) { report.cleanupErrors.push(error.message) }
  key?.remove(); report.finishedAt = new Date().toISOString()
  await writeFile('data/template-artifacts-result.json', JSON.stringify(report, null, 2))
  console.log(JSON.stringify(report))
}
