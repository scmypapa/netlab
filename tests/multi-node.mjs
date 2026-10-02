import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { execFile } from 'node:child_process'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { promisify } from 'node:util'
import { delay, guestKey, guestSSH, wsl } from './guest-ssh.mjs'

const execute = promisify(execFile)
const base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const report = { startedAt: new Date().toISOString(), environmentIds: [], steps: [] }
const environments = []
const instances = new Set()
const workers = new Map()
const quote = value => `'${String(value).replaceAll("'", "'\"'\"'")}'`
let cookie, baseline, secondary, primaryId, container, vm, key, source

async function api(path, method = 'GET', body) {
  const response = await fetch(`${base}/api/v1${path}`, {
    method, headers: { Cookie: cookie || '', 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(15_000),
  })
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie')?.split(';')[0]
  const value = response.status === 204 ? undefined : await response.json()
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${JSON.stringify(value)}`)
  return value
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
async function together(promises) {
  const results = await Promise.allSettled(promises)
  const errors = results.filter(result => result.status === 'rejected').map(result => result.reason)
  if (errors.length) throw new AggregateError(errors, errors.map(error => error.message).join('\n'))
  return results.map(result => result.value)
}
async function completed(id) {
  const deadline = Date.now() + 120_000
  while (Date.now() < deadline) {
    const value = await api(`/operations/${id}`)
    if (['succeeded', 'failed', 'partially_applied'].includes(value.state)) {
      assert.equal(value.state, 'succeeded', `${value.phase}: ${value.error}`)
      assert.equal(value.completed, value.total)
      return value
    }
    await delay(200)
  }
  throw new Error(`任务 ${id} 在120秒内未完成`)
}
const detail = env => api(`/environments/${env.id}`)
async function state(env) {
  const value = await api(`/environments/${env.id}/state`)
  value.assets.forEach(asset => instances.add(asset.instanceId))
  return value
}
async function action(env, action, assetId) {
  return completed((await api(`/environments/${env.id}${assetId ? `/assets/${assetId}` : ''}/actions`, 'POST', { action, clientRequestId: randomUUID() })).id)
}
async function apply(env, spec) {
  const current = await detail(env)
  return completed((await api(`/environments/${env.id}/changes`, 'POST', { expectedRevision: current.revision, spec, apply: true, clientRequestId: randomUUID() })).id)
}
async function onNode(nodeId, ...args) {
  assert.ok(workers.has(nodeId), `缺少Worker连接信息 ${nodeId}`)
  const worker = workers.get(nodeId)
  const command = worker ? ['ssh', '-i', worker.keyPath, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${worker.keyPath}.hosts`, `root@${worker.host}`, args.map(quote).join(' ')] : args
  try {
    const result = await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', ...command], { encoding: 'utf8', timeout: 30_000, maxBuffer: 2 ** 20 })
    return result.stdout
  } catch (error) { throw new Error(`Worker ${nodeId}: ${error.stderr?.trim() || error.message}`) }
}
async function inside(asset, ...args) {
  return onNode(asset.nodeId, 'ctr', '-n', 'netlab', 'tasks', 'exec', '--exec-id', randomUUID(), asset.instanceId, ...args)
}
async function network(asset, ...args) {
  const task = (await onNode(asset.nodeId, 'ctr', '-n', 'netlab', 'tasks', 'list')).split('\n').find(line => line.trim().split(/\s+/)[0] === asset.instanceId)
  const pid = task?.trim().split(/\s+/)[1]
  assert.match(pid || '', /^\d+$/, '客户端容器未实际启动')
  return onNode(asset.nodeId, 'nsenter', `--net=/proc/${pid}/ns/net`, ...args)
}
function newAsset(name, template, networks) {
  return {
    id: randomUUID(), name, templateId: template.id, resources: template.resources,
    interfaces: networks.map((net, index) => ({ id: randomUUID(), networkId: net.id, mac: '', address: '', primary: index === 0 })),
    ...(template.kind === 'vm' ? { guest: { username: 'netlab', sshAuthorizedKeys: [key.publicKey] } } : {}),
  }
}
async function initializeWeb(env) {
  const value = await detail(env)
  const actual = await state(env)
  await together(value.spec.assets.filter(asset => asset.templateId === container.id).map(async asset => {
    const target = actual.assets.find(item => item.assetId === asset.id)
    await inside(target, 'sh', '-c', `printf %s ${env.id}/${asset.id} > /usr/share/nginx/html/environment-id; head -c 2097152 /dev/zero > /usr/share/nginx/html/large.bin`)
  }))
}
async function pairs(env) {
  const value = await detail(env)
  const actual = await state(env)
  const web = actual.assets.filter(item => value.spec.assets.find(asset => asset.id === item.assetId)?.templateId === container.id)
  const local = web.find(item => item.nodeId === primaryId)
  const remote = web.find(item => item.nodeId === secondary.nodeId)
  assert.ok(local && remote, '调度结果未覆盖两个真实Worker')
  return { value, actual, web, local, remote }
}
async function load(env, large = false) {
  const { value, web, local, remote } = await pairs(env)
  return together([local, remote].map(async client => {
    const targets = Array.from({ length: large ? 8 : 32 }, (_, index) => {
      const target = web[Math.floor(index / 2) % web.length]
      const asset = value.spec.assets.find(item => item.id === target.assetId)
      const ip = asset.interfaces[index % 2].address
      return large ? { url: `http://${ip}/large.bin`, size: 2097152 } : { url: `http://${ip}/environment-id`, body: `${env.id}/${asset.id}` }
    })
    const crossNodeRequests = targets.filter((_, index) => web[Math.floor(index / 2) % web.length].nodeId !== client.nodeId).length
    return { environmentId: env.id, nodeId: client.nodeId, crossNodeRequests, ...(large ? { crossNodeBytes: crossNodeRequests * 2097152 } : {}), ...JSON.parse(await network(client, 'python3', '-c', source, JSON.stringify(targets), large ? '8' : '16')) }
  }))
}

try {
  await step('复用真实制品，在原Worker并行创建两个混合环境', async () => {
    await api('/sessions/login', 'POST', JSON.parse(await readFile('data/dev-login.json', 'utf8')))
    baseline = await api('/nodes')
    report.initialNodes = baseline.map(({ id, capacity, reserved }) => ({ id, capacity, reserved }))
    primaryId = wsl('cat', '/var/lib/netlab-dev/node-id').trim()
    assert.equal(baseline.filter(node => node.state === 'ready').length, 1, '本用例从单Worker扩容到双Worker；请复用未注册的Guest Worker')
    workers.set(primaryId, null)
    const candidates = JSON.parse(await readFile(process.env.NETLAB_WORKERS_FILE || 'D:/.cache/netlab/artifacts/multi-node-workers.json', 'utf8'))
    assert.equal(candidates.length, 2)
    secondary = candidates.find(item => item.nodeId !== primaryId)
    assert.ok(secondary, '连接记录中缺少第二Worker')
    workers.set(secondary.nodeId, secondary)
    source = await readFile(new URL('./fixtures/network-load.py', import.meta.url), 'utf8')
    const templates = await api('/templates?limit=100')
    container = templates.find(item => item.kind === 'container' && item.name === 'API container' && item.state === 'ready' && item.source === '/mnt/d/newgz/netlab/data/nginx.tar')
    vm = templates.find(item => item.kind === 'vm' && item.os === 'Ubuntu 24.04' && item.state === 'ready')
    assert.ok(container && vm, '现有模板尚未就绪')
    key = guestKey()
    await together(['A', 'B'].map(async marker => {
      const networks = ['业务网', '内网'].map((name, index) => ({ id: randomUUID(), name, cidr: `10.83.${index}.0/24`, mtu: 1400 }))
      const assets = [...Array.from({ length: 4 }, (_, index) => newAsset(`web-${index}`, container, networks)), newAsset('Ubuntu', vm, networks)]
      const env = await api('/environments', 'POST', { name: `跨节点验收 ${marker}`, run: true, spec: { networks, assets }, clientRequestId: randomUUID() })
      environments.push(env)
      report.environmentIds.push(env.id)
      await completed(env.operationId)
      assert.ok((await state(env)).assets.every(asset => asset.nodeId === primaryId))
      await initializeWeb(env)
    }))
  })
  await step('真实Guest Worker接入，共享OVN，按容量自动扩容资产', async () => {
    const registered = await api('/nodes', 'POST', { name: 'Ubuntu Worker', endpoint: secondary.address })
    assert.equal(registered.id, secondary.nodeId)
    report.addedNode = { id: registered.id, capacity: registered.capacity, reserved: registered.reserved }
    baseline.push(registered)
    const chassis = await together([...workers.keys()].map(node => onNode(node, 'ovs-vsctl', 'get', 'Open_vSwitch', '.', 'external_ids:system-id')))
    assert.notEqual(chassis[0].trim(), chassis[1].trim(), '两个Worker使用了同一OVS身份')
    await together(environments.map(async env => {
      const before = await detail(env)
      const spec = structuredClone(before.spec)
      spec.assets.push(newAsset('扩容-1', container, spec.networks), newAsset('扩容-2', container, spec.networks))
      await apply(env, spec)
      const after = await pairs(env)
      assert.equal(after.value.revision, before.revision + 1)
      for (const old of (await api(`/environments/${env.id}/state`)).assets.filter(asset => before.spec.assets.some(item => item.id === asset.assetId))) {
        assert.equal(old.nodeId, primaryId, '新增资产不应搬迁既有资产')
      }
      await initializeWeb(env)
      report[env.id] = { placement: after.actual.assets.map(({ assetId, nodeId }) => ({ assetId, nodeId })) }
    }))
  })
  await step('双环境双网卡跨节点HTTP与重叠地址隔离', async () => {
    report.http = (await together(environments.map(env => load(env)))).flat()
    assert.equal(report.http.reduce((sum, item) => sum + item.requests, 0), 128)
  })
  await step('真实Geneve大数据传输与MTU边界', async () => {
    report.large = (await together(environments.map(env => load(env, true)))).flat()
    assert.equal(report.large.reduce((sum, item) => sum + item.bytes, 0), 64 * 2 ** 20)
    await together(environments.map(async env => {
      const { value, local, remote } = await pairs(env)
      const target = value.spec.assets.find(asset => asset.id === remote.assetId).interfaces[0].address
      await network(local, 'ping', '-M', 'do', '-s', '1372', '-c', '2', '-W', '3', target)
      await assert.rejects(network(local, 'ping', '-M', 'do', '-s', '1373', '-c', '1', '-W', '1', target), /Message too long|message too long|mtu|MTU/)
    }))
  })
  const [a, b] = environments
  let endpoint, remoteAsset
  await step('入口网关访问另一Worker上的资产', async () => {
    const { value, remote } = await pairs(a)
    remoteAsset = value.spec.assets.find(asset => asset.id === remote.assetId)
    await completed((await api(`/environments/${a.id}/assets/${remoteAsset.id}/services`, 'POST', { expectedRevision: value.revision, protocol: 'tcp', targetPort: 80, interfaceId: remoteAsset.interfaces[1].id })).id)
    endpoint = (await api(`/environments/${a.id}/services`))[0]
    assert.equal((await onNode(primaryId, 'curl', '--noproxy', endpoint.address, '--fail', '--max-time', '5', `http://${endpoint.address}:${endpoint.port}/environment-id`)), `${a.id}/${remoteAsset.id}`)
  })
  await step('跨节点暂停恢复和网段策略变更', async () => {
    await action(a, 'suspend')
    assert.ok((await state(a)).assets.every(asset => asset.state === 'suspended'))
    await action(a, 'resume')
    const before = await detail(a)
    const actual = await state(a)
    const spec = structuredClone(before.spec)
    spec.policies = [{ id: randomUUID(), networkId: spec.networks[1].id, direction: 'both', action: 'shape', delayMs: 2 }]
    await apply(a, spec)
    assert.deepEqual((await state(a)).assets.map(asset => asset.instanceId), actual.assets.map(asset => asset.instanceId))
    await load(a)
    assert.equal((await api(`/environments/${a.id}/services`))[0].port, endpoint.port)
  })
  await step('节点服务重启保留真实Geneve数据面及运行实例', async () => {
    const before = await state(a)
    await onNode(secondary.nodeId, 'systemctl', 'stop', 'netlab-node-dev.service')
    try { await load(a) } finally { await onNode(secondary.nodeId, 'systemctl', 'start', 'netlab-node-dev.service') }
    const deadline = Date.now() + 15_000
    let refreshed, reconnectError
    while (!refreshed && Date.now() < deadline) {
      try { refreshed = await api('/nodes', 'POST', { endpoint: secondary.address }) } catch (error) { reconnectError = error; await delay(200) }
    }
    assert.equal(refreshed?.id, secondary.nodeId, `Agent未恢复真实API连接：${reconnectError?.message}`)
    assert.deepEqual((await state(a)).assets.map(asset => asset.instanceId), before.assets.map(asset => asset.instanceId))
    await load(a)
  })
  await step('释放部分远端资产并在线替换为真实Ubuntu VM', async () => {
    for (const env of environments) {
      const before = await detail(env)
      const actual = await state(env)
      const remove = new Set(actual.assets.filter(asset => asset.nodeId === secondary.nodeId && asset.assetId !== remoteAsset.id).map(asset => asset.assetId))
      if (remove.size) {
        const spec = structuredClone(before.spec)
        spec.assets = spec.assets.filter(asset => !remove.has(asset.id))
        await apply(env, spec)
      }
    }
    const before = await detail(a)
    const spec = structuredClone(before.spec)
    const replacement = spec.assets.find(asset => asset.id === remoteAsset.id)
    replacement.templateId = vm.id
    replacement.resources = vm.resources
    replacement.guest = { username: 'netlab', sshAuthorizedKeys: [key.publicKey] }
    spec.services[0].targetPort = 22
    await apply(a, spec)
    const actual = await state(a)
    const target = actual.assets.find(asset => asset.assetId === remoteAsset.id)
    assert.equal(target.nodeId, secondary.nodeId, '本用例应实际验证Guest Worker中的KVM')
    assert.match(await onNode(secondary.nodeId, 'virsh', 'dumpxml', target.instanceId), /<domain\b[^>]*\btype=['"]kvm['"]/)
    const client = actual.assets.find(asset => asset.nodeId === primaryId && spec.assets.find(item => item.id === asset.assetId)?.templateId === container.id)
    const ssh = await guestSSH(client.instanceId, target.instanceId, () => replacement.interfaces[0].address, key)
    assert.match(ssh.run('uname -a'), /Linux/)
    const bootId = ssh.run('cat', '/proc/sys/kernel/random/boot_id').trim()
    assert.notEqual(bootId, (await onNode(secondary.nodeId, 'cat', '/proc/sys/kernel/random/boot_id')).trim(), '资产不是独立VM')
    const services = await api(`/environments/${a.id}/services`)
    assert.equal(services[0].port, endpoint.port)
    assert.equal(services[0].assetId, remoteAsset.id)
    assert.match(await onNode(primaryId, 'python3', '-c', `import socket; s=socket.create_connection(("${endpoint.address}",${endpoint.port}),5); print(s.recv(256).decode()); s.close()`), /SSH-2\.0/)
  })
  await step('双Worker并行销毁及容量、OVN、实例和文件残留检查', async () => {
    let finished = false
    const offline = new Set()
    const destroying = together(environments.map(env => action(env, 'destroy'))).finally(() => { finished = true })
    const watching = (async () => {
      do {
        for (const node of await api('/nodes')) if (workers.has(node.id) && node.state !== 'ready') offline.add(node.id)
        await delay(100)
      } while (!finished)
    })()
    try { await together([destroying, watching]) } finally { report.offlineNodesDuringDestroy = [...offline] }
    assert.equal(offline.size, 0, '正常销毁不应中断节点观测')
    for (const env of environments) {
      assert.equal((await detail(env)).status, 'destroyed')
      assert.deepEqual((await state(env)).assets, [])
      assert.deepEqual(await api(`/environments/${env.id}/services`), [])
      for (const id of workers.keys()) {
        assert.equal((await onNode(id, 'find', '/var/lib/netlab-dev/environments', '-path', `/var/lib/netlab-dev/environments/${env.id}/*`, '-type', 'f')).trim(), '')
      }
    }
    const nodes = await api('/nodes')
    for (const node of baseline) assert.deepEqual(nodes.find(item => item.id === node.id).reserved, node.reserved)
    for (const id of workers.keys()) {
      const active = new Set([...((await onNode(id, 'ctr', '-n', 'netlab', 'containers', 'list', '-q')).trim().split(/\s+/)), ...((await onNode(id, 'virsh', 'list', '--all', '--uuid')).trim().split(/\s+/))])
      assert.ok([...instances].every(instance => !active.has(instance)), `Worker ${id}实例残留`)
      const ports = await onNode(id, 'ovs-vsctl', '--columns=external_ids', 'list', 'Interface')
      assert.ok(report.environmentIds.every(envId => !ports.includes(envId)), `Worker ${id}OVS端口残留`)
    }
    const rules = await onNode(primaryId, '/usr/sbin/nft', 'list', 'ruleset')
    assert.ok(!rules.includes(`tcp . ${endpoint.port}`), '入口Worker服务转发残留')
    await onNode(primaryId, 'python3', '-c', `import socket; s=socket.socket(); s.bind(("0.0.0.0",${endpoint.port}))`)
    for (const table of ['Logical_Switch', 'Logical_Router', 'Load_Balancer']) {
      const rows = wsl('ovn-nbctl', '--columns=external_ids', 'list', table)
      assert.ok(report.environmentIds.every(id => !rows.includes(id)), `${table}残留`)
    }
    const ids = report.environmentIds.map(id => `'${id}'`).join(',')
    const records = await execute('docker', ['exec', 'netlab-postgres-1', 'psql', '-U', 'netlab', '-d', 'netlab', '-Atc', `select count(*) from runtime_assets where environment_id in (${ids}); select count(*) from service_ports where environment_id in (${ids});`], { encoding: 'utf8', timeout: 10_000 })
    assert.equal(records.stdout.trim(), '0\n0')
  })
  key.remove()
  report.passed = true
} catch (error) {
  report.passed = false
  report.error = error.message
  process.exitCode = 1
} finally {
  report.finishedAt = new Date().toISOString()
  await mkdir('data', { recursive: true })
  await writeFile('data/multi-node-result.json', JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ passed: report.passed, steps: report.steps.length, report: 'data/multi-node-result.json' }))
}
