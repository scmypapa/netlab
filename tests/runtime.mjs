import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { mkdir, writeFile } from 'node:fs/promises'
import { wsl, delay } from './guest-ssh.mjs'

const base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const report = { startedAt: new Date().toISOString(), steps: [] }
let cookie, environment, baseline
const assets = new Map()

async function api(path, method = 'GET', body) {
  const response = await fetch(`${base}/api/v1${path}`, {
    method, headers: { Cookie: cookie || '', 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie')?.split(';')[0]
  const value = response.status === 204 ? undefined : await response.json()
  assert.ok(response.ok, `${path}: ${response.status} ${JSON.stringify(value)}`)
  return value
}
async function operation(id) {
  const deadline = Date.now() + 120_000
  while (Date.now() < deadline) {
    const result = await api(`/operations/${id}`)
    if (['succeeded', 'failed', 'partially_applied'].includes(result.state)) {
      assert.equal(result.state, 'succeeded', result.error)
      return result
    }
    await delay(200)
  }
  throw new Error(`operation ${id} timed out`)
}
async function action(name, action) {
  const suffix = name ? `/assets/${assets.get(name).id}` : ''
  await operation((await api(`/environments/${environment.id}${suffix}/actions`, 'POST', { action, clientRequestId: randomUUID() })).id)
}
async function actual(name, state, after) {
  const deadline = Date.now() + 15_000
  let last
  while (Date.now() < deadline) {
    last = (await api(`/environments/${environment.id}/state`)).assets.find(item => item.assetId === assets.get(name).id)
    if (last.state === state && (!after || new Date(last.observedAt) > new Date(after))) return last
    await delay(250)
  }
  throw new Error(`${name} did not reach ${state}: ${JSON.stringify(last)}`)
}
async function step(name, run) {
  const started = performance.now()
  const result = { name, passed: false }
  try { await run(); result.passed = true } catch (error) { result.error = error.message; throw error } finally {
    result.durationMs = Math.round(performance.now() - started)
    report.steps.push(result)
    console.log(JSON.stringify(result))
  }
}
function task(instance) {
  const line = wsl('ctr', '-n', 'netlab', 'tasks', 'list').split('\n').find(line => line.trim().split(/\s+/)[0] === instance)
  const fields = line?.trim().split(/\s+/)
  return { pid: fields?.[1], state: fields?.[2] }
}
const pid = instance => task(instance).pid
function kill(instance, signal) { wsl('ctr', '-n', 'netlab', 'tasks', 'kill', '--signal', signal, instance) }
function emit(instance, marker, count = 1) {
  const output = stream => Array.from({ length: count }, (_, index) => `${marker}-${index + 1}-${stream}\\n`).join('')
  wsl('ctr', '-n', 'netlab', 'tasks', 'exec', '--exec-id', randomUUID(), instance, 'sh', '-ec', `printf '${output('out')}' > /proc/1/fd/1; printf '${output('err')}' > /proc/1/fd/2`)
}
const logsPath = (name, query) => `/environments/${environment.id}/assets/${assets.get(name).id}/logs?${query}`
const chunks = text => text.split('\n').filter(line => line.startsWith('data: ')).map(line => JSON.parse(line.slice(6)))

try {
  await step('登录、缓存模板与真实混合运行环境', async () => {
    assert.ok(process.env.NETLAB_TEST_PASSWORD, 'set NETLAB_TEST_PASSWORD')
    await api('/sessions/login', 'POST', { name: 'admin', password: process.env.NETLAB_TEST_PASSWORD })
    baseline = await api('/nodes')
    const templates = await api('/templates?limit=100')
    const container = templates.find(template => template.kind === 'container' && template.state === 'ready' && template.name === 'API container')
    const vm = templates.find(template => template.kind === 'vm' && template.state === 'ready' && template.os === 'Ubuntu 24.04')
    assert.ok(container && vm, 'run lifecycle preparation first')
    const network = { id: randomUUID(), name: 'LAN', cidr: '10.77.0.0/24' }
    const make = (name, template, restartPolicy) => ({
      id: randomUUID(), name, templateId: template.id, resources: template.resources,
      interfaces: [{ id: randomUUID(), networkId: network.id, mac: '', address: '', primary: true }],
      ...(restartPolicy ? { restartPolicy } : {}),
    })
    const spec = { networks: [network], assets: [make('always', container, 'always'), make('retry', container, 'on-failure'), make('manual', container, 'never'), make('VM', vm)] }
    spec.assets.forEach(asset => assets.set(asset.name, asset))
    environment = await api('/environments', 'POST', { name: 'Runtime events and logs', run: true, clientRequestId: randomUUID(), spec })
    report.environmentId = environment.id
    await operation(environment.operationId)
    await Promise.all(spec.assets.map(asset => actual(asset.name, 'running')))
  })
  await step('真实stdout/stderr、尾部读取和流筛选', async () => {
    const current = await actual('always', 'running')
    const marker = randomUUID()
    emit(current.instanceId, marker, 10)
    await delay(150)
    for (const stream of ['all', 'stdout', 'stderr']) {
      const response = await fetch(`${base}/api/v1${logsPath('always', `tail=5&stream=${stream}`)}`, { headers: { Cookie: cookie }, signal: AbortSignal.timeout(10_000) })
      assert.equal(response.status, 200)
      const result = chunks(await response.text())
      assert.ok(result.length)
      if (stream !== 'all') assert.ok(result.every(chunk => chunk.stream === stream))
      for (const output of stream === 'all' ? ['stdout', 'stderr'] : [stream]) {
        const lines = result.filter(chunk => chunk.stream === output).map(chunk => chunk.data).join('').trimEnd().split('\n')
        assert.deepEqual(lines, Array.from({ length: 5 }, (_, index) => `${marker}-${index + 6}-${output === 'stderr' ? 'err' : 'out'}`))
      }
    }
    const empty = await fetch(`${base}/api/v1${logsPath('always', 'tail=0')}`, { headers: { Cookie: cookie }, signal: AbortSignal.timeout(10_000) })
    assert.equal(empty.status, 200)
    assert.deepEqual(chunks(await empty.text()), [])
  })
  await step('日志实时跟随和授权撤销关闭', async () => {
    const token = await api('/service-tokens', 'POST', { name: 'Runtime log test', grants: [{ scopeKind: 'asset', scopeId: `${environment.id}/${assets.get('always').id}`, permissions: ['read', 'observe'] }] })
    const abort = new AbortController()
    const timeout = setTimeout(() => abort.abort(), 10_000)
    try {
      const response = await fetch(`${base}/api/v1${logsPath('always', 'tail=0&follow=true')}`, { headers: { Authorization: `Bearer ${token.token}` }, signal: abort.signal })
      assert.equal(response.status, 200)
      const reader = response.body.getReader()
      const decoder = new TextDecoder()
      const marker = randomUUID()
      emit((await actual('always', 'running')).instanceId, marker)
      let text = ''
      try {
        while (!text.includes(`${marker}-1-out`) || !text.includes(`${marker}-1-err`)) {
          const next = await reader.read()
          assert.equal(next.done, false, 'follow ended before output')
          text += decoder.decode(next.value, { stream: true })
        }
        await api(`/service-tokens/${token.principal.id}`, 'DELETE')
        let next
        do { next = await reader.read() } while (!next.done)
      } finally { await reader.cancel() }
    } finally { clearTimeout(timeout); abort.abort(); await api(`/service-tokens/${token.principal.id}`, 'DELETE') }
  })
  await step('非正常退出：always和on-failure重启原实例', async () => {
    for (const name of ['always', 'retry']) {
      const before = await actual(name, 'running')
      const oldPID = pid(before.instanceId)
      assert.equal(task(before.instanceId).state, 'RUNNING')
      kill(before.instanceId, 'SIGKILL')
      const deadline = Date.now() + 15_000
      while ((task(before.instanceId).state !== 'RUNNING' || pid(before.instanceId) === oldPID) && Date.now() < deadline) await delay(200)
      const after = await actual(name, 'running', before.observedAt)
      assert.equal(after.instanceId, before.instanceId)
      assert.notEqual(pid(after.instanceId), oldPID)
      assert.ok(pid(after.instanceId))
      assert.equal(task(after.instanceId).state, 'RUNNING')
      assert.ok(new Date(after.observedAt) > new Date(before.observedAt))
    }
  })
  await step('正常退出与never策略保持停止', async () => {
    kill((await actual('retry', 'running')).instanceId, 'SIGQUIT')
    kill((await actual('manual', 'running')).instanceId, 'SIGKILL')
    await Promise.all([actual('retry', 'stopped'), actual('manual', 'stopped')])
    await delay(1200)
    await Promise.all([actual('retry', 'stopped'), actual('manual', 'stopped')])
  })
  await step('显式停止不被重启策略拉起', async () => {
    await action('always', 'stop')
    await delay(1200)
    await actual('always', 'stopped')
  })
  await step('节点断线、停止状态保留和重启接管', async () => {
    const before = await api(`/environments/${environment.id}/state`)
    try {
      wsl('systemctl', 'stop', 'netlab-node-dev')
      for (const asset of before.assets) {
        const unknown = await actual([...assets].find(([, value]) => value.id === asset.assetId)[0], 'unknown')
        assert.equal(unknown.observedAt, asset.observedAt)
      }
    } finally { wsl('systemctl', 'start', 'netlab-node-dev') }
    await Promise.all(['always', 'retry', 'manual'].map(name => actual(name, 'stopped')))
    assert.equal((await actual('VM', 'running')).instanceId, before.assets.find(asset => asset.assetId === assets.get('VM').id).instanceId)
    await action('always', 'start')
    await actual('always', 'running')
  })
  await step('节点停机期间Task丢失后的always恢复与日志保留', async () => {
    const before = await actual('always', 'running')
    const marker = randomUUID()
    emit(before.instanceId, marker)
    try {
      wsl('systemctl', 'stop', 'netlab-node-dev')
      await actual('always', 'unknown')
      kill(before.instanceId, 'SIGKILL')
      wsl('ctr', '-n', 'netlab', 'tasks', 'delete', before.instanceId)
    } finally { wsl('systemctl', 'start', 'netlab-node-dev') }
    const restored = await actual('always', 'running', before.observedAt)
    assert.equal(restored.instanceId, before.instanceId)
    assert.equal(task(restored.instanceId).state, 'RUNNING')
    const response = await fetch(`${base}/api/v1${logsPath('always', 'tail=100')}`, { headers: { Cookie: cookie }, signal: AbortSignal.timeout(10_000) })
    assert.equal(response.status, 200)
    assert.ok(chunks(await response.text()).some(chunk => chunk.data.includes(`${marker}-1-out`)))
  })
  await step('libvirt原生暂停恢复事件同步至API', async () => {
    const vm = await actual('VM', 'running')
    wsl('virsh', 'suspend', vm.instanceId)
    await actual('VM', 'suspended')
    wsl('virsh', 'resume', vm.instanceId)
    await actual('VM', 'running')
  })
  await step('混合环境销毁和资源释放', async () => {
    const state = await api(`/environments/${environment.id}/state`)
    await action(undefined, 'destroy')
    assert.equal((await api(`/environments/${environment.id}/state`)).assets.length, 0)
    const current = `${wsl('virsh', 'list', '--all', '--uuid')}\n${wsl('ctr', '-n', 'netlab', 'containers', 'list', '-q')}`.trim().split(/\s+/)
    assert.ok(state.assets.every(asset => !current.includes(asset.instanceId)))
    const nodes = await api('/nodes')
    for (const node of baseline) assert.deepEqual(nodes.find(item => item.id === node.id).reserved, node.reserved)
    const resources = `${wsl('ovn-nbctl', '--columns=external_ids', 'list', 'Logical_Switch')}\n${wsl('ovs-vsctl', '--columns=external_ids', 'list', 'Interface')}`
    assert.ok(!resources.includes(environment.id))
    for (const asset of state.assets) wsl('test', '!', '-e', `/var/lib/netlab-dev/environments/${environment.id}/instances/${asset.instanceId}`)
  })
  report.passed = true
} catch (error) { report.passed = false; report.error = error.message; process.exitCode = 1 } finally {
  report.finishedAt = new Date().toISOString()
  await mkdir('data', { recursive: true })
  await writeFile('data/runtime-result.json', JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ passed: report.passed, steps: report.steps.length, report: 'data/runtime-result.json' }))
}
