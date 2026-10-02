import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { mkdir, writeFile } from 'node:fs/promises'
import { execFileSync } from 'node:child_process'

const base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const password = process.env.NETLAB_TEST_PASSWORD
const endpoint = process.env.NETLAB_TEST_NODE
const vmSource = process.env.NETLAB_TEST_VM_SOURCE
const containerSource = process.env.NETLAB_TEST_CONTAINER_SOURCE || 'docker.io/library/nginx:1.27.5'
const report = { startedAt: new Date().toISOString(), environmentIds: [], steps: [] }
let cookie
let baseline
const instances = new Set()

async function api(path, method = 'GET', body) {
  const response = await fetch(`${base}/api/v1${path}`, {
    method,
    headers: { 'Content-Type': 'application/json', ...(cookie ? { Cookie: cookie } : {}) },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie')?.split(';')[0]
  const value = response.status === 204 ? undefined : await response.json()
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${JSON.stringify(value)}`)
  return value
}

async function step(name, run) {
  const started = performance.now()
  try {
    const value = await run()
    const item = { name, passed: true, durationMs: Math.round(performance.now() - started) }
    report.steps.push(item)
    console.log(JSON.stringify(item))
    return value
  } catch (error) {
    const item = { name, passed: false, durationMs: Math.round(performance.now() - started), error: error.message }
    report.steps.push(item)
    console.error(JSON.stringify(item))
    throw error
  }
}

const delay = ms => new Promise(resolve => setTimeout(resolve, ms))
async function finished(id) {
  const deadline = Date.now() + 120_000
  while (Date.now() < deadline) {
    const operation = await api(`/operations/${id}`)
    assert.ok(operation.completed <= operation.total, '完成数量超过任务目标数量')
    if (['succeeded', 'failed', 'partially_applied'].includes(operation.state)) return operation
    await delay(200)
  }
  throw new Error(`任务 ${id} 在 120 秒内未完成`)
}
async function completed(id) {
  const operation = await finished(id)
  assert.equal(operation.state, 'succeeded', `${operation.kind}/${operation.phase}: ${operation.error}`)
  assert.equal(operation.completed, operation.total)
  return operation
}

async function state(id) {
  const value = await api(`/environments/${id}/state`)
  value.assets.forEach(a => instances.add(a.instanceId))
  return value
}

function inContainer(instance, ...args) {
  return execFileSync('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--', 'ctr', '-n', 'netlab', 'tasks', 'exec', '--exec-id', randomUUID(), instance, ...args], { encoding: 'utf8', timeout: 15_000 })
}

async function template(kind, source, hardware) {
  const value = await api('/templates', 'POST', {
    id: randomUUID(), name: `API ${kind}`, kind, os: kind === 'container' ? 'linux' : 'hardware-fixture',
    version: 1, source, resources: { cpu: 1, memoryMiB: 256, diskGiB: 2 }, ...(hardware ? { hardware } : {}),
  })
  const deadline = Date.now() + 120_000
  while (Date.now() < deadline) {
    const current = (await api('/templates?limit=500')).find(t => t.id === value.id)
    assert.notEqual(current.state, 'failed', current.error)
    if (current.state === 'ready') return current
    await delay(200)
  }
  throw new Error(`模板 ${value.name} 准备超时`)
}

function asset(name, templateId, networkId) {
  return { id: randomUUID(), name, templateId, resources: { cpu: 1, memoryMiB: 256, diskGiB: 2 },
    interfaces: [{ id: randomUUID(), networkId, mac: '', address: '', primary: true }] }
}

async function action(environmentId, action, assetId) {
  const suffix = assetId ? `/assets/${assetId}` : ''
  const operation = await api(`/environments/${environmentId}${suffix}/actions`, 'POST', { action, clientRequestId: randomUUID() })
  return completed(operation.id)
}

try {
  assert.ok(password && endpoint && vmSource, '设置 NETLAB_TEST_PASSWORD、NETLAB_TEST_NODE、NETLAB_TEST_VM_SOURCE')
  await step('登录与真实节点登记', async () => {
    const identity = await api('/sessions/login', 'POST', { name: 'admin', password })
    assert.equal(identity.administrator, true)
    const node = await api('/nodes', 'POST', { endpoint })
    assert.ok(node.capabilities.includes('container') && node.capabilities.includes('vm'))
    baseline = await api('/nodes')
  })
  const [container, vm] = await step('准备 OCI 与 BIOS KVM 模板', () => Promise.all([
    template('container', containerSource),
    template('vm', vmSource, { firmware: 'bios', machine: 'pc', diskBus: 'ide', nicModel: 'e1000' }),
  ]))
  const seed = await step('保存环境模板、追加版本与读取历史版本', async () => {
    const network = { id: randomUUID(), name: 'LAN', cidr: '10.76.0.0/24' }
    const web = asset('web', container.id, network.id)
    web.volumes = [{ id: randomUUID(), mountPath: '/var/netlab', sizeGiB: 1 }]
    const value = await api('/environments', 'POST', { name: 'Lifecycle Blueprint Source', spec: {
      networks: [network], assets: [web, asset('client', container.id, network.id), asset('VM', vm.id, network.id)],
      policies: [{ id: randomUUID(), networkId: network.id, direction: 'both', action: 'shape', delayMs: 2 }],
    } })
    report.environmentIds.push(value.id)
    const blueprint = await api(`/environments/${value.id}/blueprints`, 'POST', { name: 'Mixed Lifecycle', expectedRevision: value.revision, spec: value.spec })
    const next = structuredClone(value.spec)
    next.assets[0].name = 'future-web'
    const version = await api(`/blueprints/${blueprint.id}/versions`, 'POST', { environmentId: value.id, expectedRevision: value.revision, spec: next })
    assert.equal(version.version, 2)
    const original = await api(`/blueprint-versions/${blueprint.latestVersionId}`)
    assert.deepEqual(original.spec, value.spec)
    assert.equal((await api(`/blueprints/${blueprint.id}/versions`)).length, 2)
    assert.equal((await api(`/environments/${value.id}`)).revision, value.revision)
    return { environment: value, blueprint }
  })
  const environments = await step('由同一历史模板并行创建两个独立混合环境', async () => {
    return Promise.all(['A', 'B'].map(async name => {
      const request = { name: `API Lifecycle ${name}`, clientRequestId: randomUUID(), run: true, blueprintVersionId: seed.blueprint.latestVersionId }
      const result = await api('/environments', 'POST', request)
      report.environmentIds.push(result.id)
      const repeated = await api('/environments', 'POST', request)
      assert.equal(repeated.id, result.id)
      assert.equal(repeated.operationId, result.operationId)
      await completed(result.operationId)
      const actual = await api(`/environments/${result.id}`)
      assert.equal(actual.status, 'running')
      assert.equal(actual.revision, 1)
      assert.equal(actual.blueprintVersionId, seed.blueprint.latestVersionId)
      assert.notEqual(actual.spec.assets[0].id, seed.environment.spec.assets[0].id)
      assert.notEqual(actual.spec.assets[0].volumes[0].id, seed.environment.spec.assets[0].volumes[0].id)
      await state(actual.id)
      return actual
    }))
  })
  const [a, b] = environments
  await step('OVN 双向流控下首次跨容器 HTTP 通信', async () => {
    const actual = await state(a.id)
    const client = actual.assets.find(s => s.assetId === a.spec.assets.find(x => x.name === 'client').id)
    const address = a.spec.assets.find(x => x.name === 'web').interfaces[0].address
    const output = inContainer(client.instanceId, 'curl', '-sf', `http://${address}`)
    assert.ok(output.includes('Welcome to nginx'))
    for (const environment of environments) {
      const actual = await state(environment.id)
      const web = actual.assets.find(s => s.assetId === environment.spec.assets.find(x => x.name === 'web').id)
      const client = actual.assets.find(s => s.assetId === environment.spec.assets.find(x => x.name === 'client').id)
      inContainer(web.instanceId, 'sh', '-c', `printf %s ${environment.id} > /usr/share/nginx/html/environment-id`)
      assert.equal(inContainer(client.instanceId, 'curl', '-sf', '--max-time', '8', `http://${environment.spec.assets[0].interfaces[0].address}/environment-id`), environment.id)
    }
  })
  await step('容器与 KVM 暂停、恢复', async () => {
    await action(a.id, 'suspend')
    assert.equal((await api(`/environments/${a.id}`)).status, 'suspended')
    await action(a.id, 'resume')
    assert.equal((await api(`/environments/${a.id}`)).status, 'running')
  })
  await step('容器正常停止与重新启动', async () => {
    const target = a.spec.assets.find(x => x.name === 'web')
    const before = await api(`/environments/${a.id}`)
    await action(a.id, 'stop', target.id)
    await action(a.id, 'start', target.id)
    assert.equal((await api(`/environments/${a.id}`)).revision, before.revision)
  })
  await step('资产更新与新增、移除、链路策略', async () => {
    const before = await api(`/environments/${a.id}`)
    const oldState = await state(a.id)
    const webInstance = oldState.assets.find(x => x.assetId === before.spec.assets[0].id).instanceId
    const startTime = inContainer(webInstance, 'sh', '-c', 'cut -d " " -f22 /proc/1/stat')
    const spec = structuredClone(before.spec)
    spec.assets[0].resources.memoryMiB = 320
    const added = asset('new', container.id, spec.networks[0].id)
    spec.assets.push(added)
    spec.policies = [{ id: randomUUID(), networkId: spec.networks[0].id, direction: 'both', action: 'shape', delayMs: 2 }]
    const preview = await api(`/environments/${a.id}/changes`, 'POST', { expectedRevision: before.revision - 1, spec, apply: false })
    assert.equal(preview.revision, before.revision)
    assert.ok(preview.changes.some(x => x.effect === 'add') && preview.changes.some(x => x.effect === 'update'))
    assert.equal(preview.changes.find(x => x.id === before.spec.assets[0].id).requiresStop, false)
    const operation = await api(`/environments/${a.id}/changes`, 'POST', { expectedRevision: preview.revision, spec, apply: true, clientRequestId: randomUUID() })
    await completed(operation.id)
    const after = await api(`/environments/${a.id}`)
    assert.equal(after.revision, before.revision + 1)
    assert.equal(after.spec.assets.length, 4)
    const updated = await state(a.id)
    assert.equal(inContainer(webInstance, 'sh', '-c', 'cut -d " " -f22 /proc/1/stat'), startTime)
    const clientInstance = updated.assets.find(x => x.assetId === after.spec.assets.find(x => x.name === 'client').id).instanceId
    assert.ok(inContainer(clientInstance, 'curl', '-sf', '--max-time', '8', `http://${after.spec.assets[0].interfaces[0].address}`).includes('Welcome to nginx'))
    for (const previous of oldState.assets) assert.equal(updated.assets.find(x => x.assetId === previous.assetId).instanceId, previous.instanceId)
    assert.equal((await api(`/environments/${b.id}`)).revision, 1)
    const clean = structuredClone(after.spec)
    clean.assets = clean.assets.filter(x => x.id !== added.id)
    clean.policies = []
    const removed = await api(`/environments/${a.id}/changes`, 'POST', { expectedRevision: after.revision, spec: clean, apply: true, clientRequestId: randomUUID() })
    await completed(removed.id)
    assert.equal((await state(a.id)).assets.length, 3)
  })
  await step('容器替换保留数据卷与稳定接口，VM 原地扩盘和重建', async () => {
    const before = await api(`/environments/${a.id}`)
    const previous = await state(a.id)
    const web = before.spec.assets.find(x => x.name === 'web')
    const oldWeb = previous.assets.find(x => x.assetId === web.id)
    inContainer(oldWeb.instanceId, 'sh', '-c', 'printf netlab-lifecycle > /var/netlab/proof')
    const other = await state(b.id)
    inContainer(other.assets.find(x => x.assetId === b.spec.assets[0].id).instanceId, 'sh', '-c', 'test ! -e /var/netlab/proof')
    await action(a.id, 'rebuild', web.id)
    const replaced = await state(a.id)
    const newWeb = replaced.assets.find(x => x.assetId === web.id)
    assert.notEqual(newWeb.instanceId, oldWeb.instanceId)
    assert.equal(inContainer(newWeb.instanceId, 'cat', '/var/netlab/proof'), 'netlab-lifecycle')
    const current = await api(`/environments/${a.id}`)
    assert.deepEqual(current.spec.assets.find(x => x.id === web.id).interfaces, web.interfaces)
    const spec = structuredClone(current.spec)
    const vmAsset = spec.assets.find(x => x.name === 'VM')
    vmAsset.resources.diskGiB = 3
    const update = await api(`/environments/${a.id}/changes`, 'POST', { expectedRevision: current.revision, spec, apply: true })
    await completed(update.id)
    assert.equal((await state(a.id)).assets.find(x => x.assetId === vmAsset.id).instanceId, previous.assets.find(x => x.assetId === vmAsset.id).instanceId)
    await action(a.id, 'rebuild', vmAsset.id)
    assert.notEqual((await state(a.id)).assets.find(x => x.assetId === vmAsset.id).instanceId, previous.assets.find(x => x.assetId === vmAsset.id).instanceId)
  })
  await step('真实执行失败后的配置、数据卷与容量回退', async () => {
    const before = await api(`/environments/${a.id}`)
    const reservations = await api('/nodes')
    const spec = structuredClone(before.spec)
    spec.assets.find(x => x.name === 'VM').resources.diskGiB = 1
    const target = spec.assets.find(x => x.name === 'web')
    const volume = { id: randomUUID(), mountPath: '/var/netlab-failed', sizeGiB: 1 }
    target.volumes.push(volume)
    const op = await api(`/environments/${a.id}/changes`, 'POST', { expectedRevision: before.revision, spec, apply: true })
    const failed = await finished(op.id)
    assert.equal(failed.state, 'failed')
    assert.equal(failed.phase, 'rolled-back', failed.error)
    assert.match(failed.error, /shrinking/)
    const restored = await api(`/environments/${a.id}`)
    assert.equal(restored.status, 'running')
    assert.equal(restored.revision, before.revision)
    assert.deepEqual(restored.appliedSpec, before.appliedSpec)
    assert.deepEqual((await api('/nodes')).map(n => n.reserved), reservations.map(n => n.reserved))
    const volumeDirectories = execFileSync('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--', 'find', `/var/lib/netlab-dev/environments/${a.id}`, '-type', 'd', '-name', volume.id], { encoding: 'utf8' })
    assert.equal(volumeDirectories.trim(), '')
  })
  await step('KVM 强停保盘并重新启动', async () => {
    await action(a.id, 'force-stop')
    assert.equal((await api(`/environments/${a.id}`)).status, 'stopped')
    await action(a.id, 'start')
    assert.equal((await api(`/environments/${a.id}`)).status, 'running')
  })
  await step('并行销毁与容量释放', async () => {
    await Promise.all([...environments, seed.environment].map(e => action(e.id, 'destroy')))
    for (const e of [...environments, seed.environment]) {
      assert.equal((await api(`/environments/${e.id}`)).status, 'destroyed')
      assert.equal((await api(`/environments/${e.id}/state`)).assets.length, 0)
    }
    const nodes = await api('/nodes')
    for (const node of baseline) assert.deepEqual(nodes.find(n => n.id === node.id).reserved, node.reserved)
    const output = execFileSync('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--', 'virsh', 'list', '--all', '--uuid'], { encoding: 'utf8' })
    const containers = execFileSync('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--', 'ctr', '-n', 'netlab', 'containers', 'list', '-q'], { encoding: 'utf8' })
    const active = new Set([...output.trim().split(/\s+/), ...containers.trim().split(/\s+/)])
    assert.ok([...instances].every(id => !active.has(id)))
    const networks = execFileSync('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--', 'ovn-nbctl', '--columns=external_ids', 'list', 'Logical_Switch'], { encoding: 'utf8' })
    assert.ok(report.environmentIds.every(id => !networks.includes(id)))
  })
  report.passed = true
} catch (error) {
  report.passed = false
  report.error = error.message
  process.exitCode = 1
} finally {
  report.finishedAt = new Date().toISOString()
  await mkdir('data', { recursive: true })
  await writeFile('data/api-lifecycle-result.json', JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ passed: report.passed, steps: report.steps.length, report: 'data/api-lifecycle-result.json' }))
}
