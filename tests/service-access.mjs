import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { createSocket } from 'node:dgram'
import { once } from 'node:events'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { Agent, request } from 'node:http'
import { execFileSync, spawn } from 'node:child_process'
import { guestKey, guestSSH, wsl, delay } from './guest-ssh.mjs'

const base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const report = { startedAt: new Date().toISOString(), environmentIds: [], steps: [] }
const environments = []
const principals = []
const instances = new Set()
let cookie, baseline, key, occupied

async function api(path, method = 'GET', body, token) {
  const response = await fetch(`${base}/api/v1${path}`, {
    method,
    headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: `Bearer ${token}` } : { Cookie: cookie || '' }) },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(15_000),
  })
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie')?.split(';')[0]
  const result = response.status === 204 ? undefined : await response.json()
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${JSON.stringify(result)}`)
  return result
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
async function finished(id, token) {
  const deadline = Date.now() + 120_000
  while (Date.now() < deadline) {
    const result = await api(`/operations/${id}`, 'GET', undefined, token)
    assert.ok(result.completed <= result.total)
    if (['succeeded', 'failed', 'partially_applied'].includes(result.state)) return result
    await delay(200)
  }
  throw new Error(`任务 ${id} 在120秒内未完成`)
}
async function completed(id, token) {
  const result = await finished(id, token)
  assert.equal(result.state, 'succeeded', `${result.phase}: ${result.error}`)
  assert.equal(result.completed, result.total)
  return result
}
const detail = env => api(`/environments/${env.id}`)
const services = env => api(`/environments/${env.id}/services`)
async function state(env) {
  const value = await api(`/environments/${env.id}/state`)
  value.assets.forEach(item => instances.add(item.instanceId))
  return value
}
function inside(instance, ...args) { return wsl('ctr', '-n', 'netlab', 'tasks', 'exec', '--exec-id', randomUUID(), instance, ...args) }
function http(endpoint, agent) {
  return new Promise((resolve, reject) => {
    const req = request({ host: endpoint.address, port: endpoint.port, path: '/environment-id', agent }, response => {
      let output = ''
      response.setEncoding('utf8')
      response.on('data', chunk => output += chunk)
      response.on('end', () => response.statusCode === 200 ? resolve(output) : reject(new Error(`HTTP ${response.statusCode}`)))
      response.on('error', reject)
    })
    req.setTimeout(4000, () => req.destroy(new Error('HTTP连接超时')))
    req.on('error', reject)
    req.end()
  })
}
function udp(endpoint, message = randomUUID(), socket) {
  const client = socket || createSocket('udp4')
  return new Promise((resolve, reject) => {
    const done = (error, value) => {
      clearTimeout(timer)
      client.off('message', onMessage)
      client.off('error', onError)
      if (!socket) client.close()
      error ? reject(error) : resolve(value)
    }
    const onMessage = data => done(undefined, data.toString())
    const onError = error => done(error)
    const timer = setTimeout(() => done(new Error('UDP连接超时')), 4000)
    client.once('message', onMessage)
    client.once('error', onError)
    client.send(message, endpoint.port, endpoint.address, error => { if (error) done(error) })
  })
}
async function expose(env, asset, input, token) {
  const current = await detail(env)
  const body = { ...input, expectedRevision: current.revision, clientRequestId: randomUUID() }
  const path = `/environments/${env.id}/assets/${asset.id}/services`
  const operation = await api(path, 'POST', body, token)
  assert.equal((await api(path, 'POST', body, token)).id, operation.id)
  const result = await completed(operation.id, token)
  assert.equal(result.total, 1, '服务变更不应统计整环境资产')
  const endpoints = await services(env)
  const endpoint = endpoints.find(item => item.assetId === asset.id && item.protocol === input.protocol && item.targetPort === input.targetPort)
  assert.ok(endpoint?.address && endpoint.port > 0, '执行成功后没有真实入口')
  return endpoint
}
async function revoke(env, endpoint, token) {
  const current = await detail(env)
  const result = await api(`/environments/${env.id}/services/${endpoint.id}?expectedRevision=${current.revision}&clientRequestId=${randomUUID()}`, 'DELETE', undefined, token)
  await completed(result.id, token)
  assert.ok(!(await services(env)).some(item => item.id === endpoint.id))
}
async function action(env, action, asset) {
  await completed((await api(`/environments/${env.id}${asset ? `/assets/${asset.id}` : ''}/actions`, 'POST', { action, clientRequestId: randomUUID() })).id)
}

try {
  await step('准备现有模板与带服务声明的环境模板', async () => {
    const login = JSON.parse(await readFile('data/dev-login.json', 'utf8'))
    await api('/sessions/login', 'POST', login)
    baseline = await api('/nodes')
    const templates = await api('/templates?limit=100')
    const container = templates.find(item => item.kind === 'container' && item.state === 'ready' && item.name === 'API container')
    const vm = templates.find(item => item.kind === 'vm' && item.state === 'ready' && item.os === 'Ubuntu 24.04')
    assert.ok(container && vm, '现有容器和Ubuntu模板尚未就绪')
    key = guestKey()
    const networks = ['管理网', '业务网'].map((name, index) => ({ id: randomUUID(), name, cidr: `10.78.${index}.0/24` }))
    const make = (name, template) => ({
      id: randomUUID(), name, templateId: template.id, resources: template.resources,
      interfaces: networks.map((network, index) => ({ id: randomUUID(), networkId: network.id, mac: '', address: '', primary: index === 0 })),
      ...(template.kind === 'vm' ? { guest: { username: 'netlab', sshAuthorizedKeys: [key.publicKey] } } : {}),
    })
    const assets = [make('web', container), make('VM', vm)]
    const declared = { id: randomUUID(), assetId: assets[0].id, interfaceId: assets[0].interfaces[1].id, protocol: 'tcp', targetPort: 80, listenPort: 38080 }
    const seed = await api('/environments', 'POST', { name: '服务入口模板源', spec: { networks, assets, services: [declared] } })
    report.environmentIds.push(seed.id)
    const blueprint = await api(`/environments/${seed.id}/blueprints`, 'POST', { name: `服务入口 ${seed.id}`, expectedRevision: seed.revision, spec: seed.spec })
    for (const marker of ['A', 'B']) {
      const env = await api('/environments', 'POST', { name: `服务入口验收 ${marker}`, run: true, blueprintVersionId: blueprint.latestVersionId, clientRequestId: randomUUID() })
      environments.push(env)
      report.environmentIds.push(env.id)
      await completed(env.operationId)
      Object.assign(env, await detail(env))
      assert.notEqual(env.spec.services[0].id, declared.id)
      assert.equal(env.spec.services[0].listenPort, undefined, '模板创建应为各环境自动分配端口')
      const actual = await state(env)
      const web = env.spec.assets.find(item => item.name === 'web')
      inside(actual.assets.find(item => item.assetId === web.id).instanceId, 'sh', '-c', `printf %s ${env.id} > /usr/share/nginx/html/environment-id`)
    }
    await action(seed, 'destroy')
  })
  const [a, b] = environments
  const web = a.spec.assets.find(item => item.name === 'web')
  const vm = a.spec.assets.find(item => item.name === 'VM')
  let webA, webB, vmTCP, vmUDP, token
  await step('相同网段、自动端口与真实OVN HTTP入口隔离', async () => {
    webA = (await services(a))[0]
    webB = (await services(b))[0]
    assert.notEqual(webA.port, webB.port)
    await Promise.all(Array.from({ length: 32 }, async (_, index) => {
      const endpoint = index % 2 ? webA : webB
      assert.equal(await http(endpoint), index % 2 ? a.id : b.id)
    }))
    report.httpConcurrency = 32
  })
  await step('真实Ubuntu非主网卡TCP和UDP服务', async () => {
    const actual = await state(a)
    const ssh = await guestSSH(actual.assets.find(item => item.assetId === web.id).instanceId,
      actual.assets.find(item => item.assetId === vm.id).instanceId, () => vm.interfaces[0].address, key)
    const source = await readFile(new URL('./fixtures/service-server.py', import.meta.url))
    ssh.run(`sudo sh -c 'printf %s ${source.toString('base64')} | base64 -d > /tmp/netlab-service.py; nohup python3 /tmp/netlab-service.py ${a.id} >/tmp/netlab-service.log 2>&1 </dev/null &'`)
    const deadline = Date.now() + 10_000
    while (!ssh.run('ss -lnt').includes(':8080') && Date.now() < deadline) await delay(100)
    assert.ok(ssh.run('ss -lnt').includes(':8080'), '测试服务没有真实监听')
    vmTCP = await expose(a, vm, { protocol: 'tcp', targetPort: 8080, interfaceId: vm.interfaces[1].id })
    vmUDP = await expose(a, vm, { protocol: 'udp', targetPort: 8081, interfaceId: vm.interfaces[1].id })
    await Promise.all(Array.from({ length: 16 }, async (_, index) => {
      assert.equal(await http(vmTCP), a.id)
      const marker = `packet-${index}`
      assert.equal(await udp(vmUDP, marker), `${a.id}:${marker}`)
    }))
    report.udpConcurrency = 16
  })
  await step('资产范围授权、任务查询与拒绝越权', async () => {
    const issued = await api('/service-tokens', 'POST', { name: `服务验收 ${a.id}`, grants: [{ scopeKind: 'asset', scopeId: `${a.id}/${web.id}`, permissions: ['read', 'access'] }] })
    token = issued.token
    principals.push(issued.principal.id)
    assert.deepEqual((await api(`/environments/${a.id}/services`, 'GET', undefined, token)).map(item => item.assetId), [web.id])
    const forbidden = await fetch(`${base}/api/v1/environments/${a.id}/assets/${vm.id}/services`, { method: 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' }, body: JSON.stringify({ protocol: 'tcp', targetPort: 22, expectedRevision: (await detail(a)).revision }) })
    assert.equal(forbidden.status, 403)
    const actual = await state(a)
    const beforePID = wsl('ctr', '-n', 'netlab', 'tasks', 'list')
    const added = await expose(a, web, { protocol: 'tcp', targetPort: 80, interfaceId: web.interfaces[0].id }, token)
    assert.equal(await http(added), a.id)
    const after = await state(a)
    assert.deepEqual(after.assets.map(item => item.instanceId), actual.assets.map(item => item.instanceId))
    assert.equal(wsl('ctr', '-n', 'netlab', 'tasks', 'list'), beforePID, '服务操作改变了容器进程')
    await revoke(a, added, token)
  })
  await step('宿主手工端口冲突真实失败并保留原入口', async () => {
    occupied = spawn('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--', 'python3', '-u', '-c', 'import socket,sys; s=socket.socket(); s.bind(("0.0.0.0",0)); s.listen(); print(s.getsockname()[1],flush=True); sys.stdin.read()'], { stdio: ['pipe', 'pipe', 'pipe'] })
    const [output] = await once(occupied.stdout, 'data')
    const port = Number(output.toString().trim())
    assert.ok(port > 0)
    const before = await detail(a)
    const beforeServices = await services(a)
    const op = await api(`/environments/${a.id}/assets/${web.id}/services`, 'POST', { protocol: 'tcp', targetPort: 81, listenPort: port, expectedRevision: before.revision })
    const failed = await finished(op.id)
    assert.equal(failed.state, 'failed')
    assert.equal(failed.phase, 'rolled-back', failed.error)
    assert.match(failed.error, /address already in use|端口.*占用|bind/i)
    assert.equal((await detail(a)).revision, before.revision)
    assert.deepEqual(await services(a), beforeServices)
    assert.equal(await http(webA), a.id)
    const exited = once(occupied, 'exit')
    occupied.stdin.end()
    await exited
    occupied = undefined
    const manual = await expose(a, web, { protocol: 'tcp', targetPort: 80, interfaceId: web.interfaces[0].id, listenPort: port })
    assert.equal(manual.port, port)
    assert.equal(await http(manual), a.id)
    await revoke(a, manual)
  })
  await step('容器重建后稳定端口和目标身份', async () => {
    const previous = await state(a)
    await action(a, 'rebuild', web)
    const after = await state(a)
    const actual = after.assets.find(item => item.assetId === web.id)
    assert.notEqual(actual.instanceId, previous.assets.find(item => item.assetId === web.id).instanceId)
    inside(actual.instanceId, 'sh', '-c', `printf %s ${a.id} > /usr/share/nginx/html/environment-id`)
    const kept = (await services(a)).find(item => item.id === webA.id)
    assert.equal(kept.id, webA.id)
    assert.equal(kept.port, webA.port)
    assert.equal(kept.interfaceId, webA.interfaceId)
    assert.equal(await http(webA), a.id)
    assert.equal(await http(vmTCP), a.id)
  })
  await step('Agent停止期间转发保持，重启恢复端口占有', async () => {
    wsl('systemctl', 'stop', 'netlab-node-dev.service')
    try {
      assert.equal(await http(webA), a.id)
      assert.equal(await udp(vmUDP, 'offline'), `${a.id}:offline`)
    } finally { wsl('systemctl', 'start', 'netlab-node-dev.service') }
    const deadline = Date.now() + 15_000
    while (!(await api('/nodes')).every(node => node.state === 'ready') && Date.now() < deadline) await delay(250)
    assert.equal(await http(webA), a.id)
    const bound = wsl('ss', '-lntup')
    assert.ok(bound.includes(`:${webA.port}`) && bound.includes(`:${vmUDP.port}`), '节点没有恢复已应用端口占有')
  })
  await step('撤销现存TCP/UDP连接并重新使用端口', async () => {
    const agent = new Agent({ keepAlive: true })
    const socket = createSocket('udp4')
    try {
      assert.equal(await http(vmTCP, agent), a.id)
      assert.equal(await udp(vmUDP, 'before', socket), `${a.id}:before`)
      await revoke(a, vmTCP)
      await revoke(a, vmUDP)
      await assert.rejects(http(vmTCP, agent))
      await assert.rejects(udp(vmUDP, 'after', socket))
      const reused = await expose(b, b.spec.assets.find(item => item.name === 'web'), { protocol: 'tcp', targetPort: 80, listenPort: vmTCP.port })
      assert.equal(await http(reused), b.id)
      await revoke(b, reused)
    } finally { agent.destroy(); socket.close() }
  })
  await step('销毁、容量与数据库及真实网络残留检查', async () => {
    await Promise.all(environments.map(env => action(env, 'destroy')))
    for (const env of environments) {
      assert.equal((await detail(env)).status, 'destroyed')
      assert.deepEqual(await services(env), [])
      assert.equal((await state(env)).assets.length, 0)
      assert.equal(wsl('find', '/var/lib/netlab-dev/environments', '-maxdepth', '1', '-name', env.id).trim(), '')
    }
    const nodes = await api('/nodes')
    for (const node of baseline) assert.deepEqual(nodes.find(item => item.id === node.id).reserved, node.reserved)
    for (const table of ['Logical_Switch', 'Logical_Router', 'Load_Balancer']) {
      const rows = wsl('ovn-nbctl', '--columns=external_ids', 'list', table)
      assert.ok(report.environmentIds.every(id => !rows.includes(id)), `${table}残留`)
    }
    const rules = wsl('nft', 'list', 'ruleset')
    assert.ok(environments.every(env => !rules.includes(env.id)), 'nftables规则残留')
    const active = new Set([...wsl('virsh', 'list', '--all', '--uuid').trim().split(/\s+/), ...wsl('ctr', '-n', 'netlab', 'containers', 'list', '-q').trim().split(/\s+/)])
    assert.ok([...instances].every(id => !active.has(id)), '运行实例残留')
    const ids = report.environmentIds.map(id => `'${id}'`).join(',')
    const remaining = execFileSync('docker', ['exec', 'netlab-postgres-1', 'psql', '-U', 'netlab', '-d', 'netlab', '-Atc', `select count(*) from service_ports where environment_id in (${ids});`], { encoding: 'utf8', timeout: 10_000 })
    assert.equal(remaining.trim(), '0')
  })
  report.passed = true
  key.remove()
} catch (error) {
  report.passed = false
  report.error = error.message
  process.exitCode = 1
} finally {
  occupied?.stdin.end()
  for (const id of principals) await api(`/service-tokens/${id}`, 'DELETE')
  report.finishedAt = new Date().toISOString()
  await mkdir('data', { recursive: true })
  await writeFile('data/service-access-result.json', JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ passed: report.passed, steps: report.steps.length, report: 'data/service-access-result.json' }))
}
