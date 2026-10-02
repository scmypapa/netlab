import assert from 'node:assert/strict'
import { generateKeyPairSync, randomUUID } from 'node:crypto'
import { execFile, execFileSync } from 'node:child_process'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { promisify } from 'node:util'
import { delay, guestKey, wsl } from './guest-ssh.mjs'

const execute = promisify(execFile)
const base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const runId = randomUUID().replaceAll('-', '').slice(0, 10)
const reportPath = 'data/vpn-access-result.json'
const report = { startedAt: new Date().toISOString(), environmentIds: [], steps: [], traffic: [] }
const environments = [], clients = [], principals = [], namespaces = new Set(), hostInterfaces = new Set()
const workers = new Map(), instances = new Set()
const quote = value => `'${String(value).replaceAll("'", "'\"'\"'")}'`
const safe = error => String(error.message || error).replace(/[A-Za-z0-9/+]{43}=/g, '[key]')
const vpnName = env => `nlvpn-${env.id}`
const vpnDevice = env => `nv${env.id.replaceAll('-', '').slice(-12)}`
let cookie, baseline, template, vm, key, payload, trigger

async function api(path, method = 'GET', body, token, expected) {
  const response = await fetch(`${base}/api/v1${path}`, {
    method, headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: `Bearer ${token}` } : { Cookie: cookie || '' }) },
    body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(15_000),
  })
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie')?.split(';')[0]
  const value = response.status === 204 ? undefined : await response.json()
  if (expected) { assert.equal(response.status, expected, `${method} ${path}`); return value }
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${value?.detail || value?.title || response.statusText}`)
  return value
}
async function step(name, run) {
  const started = performance.now(), result = { name, passed: false }
  try { await run(); result.passed = true } catch (error) { result.error = safe(error); throw error } finally {
    result.durationMs = Math.round(performance.now() - started)
    report.steps.push(result)
    console.log(JSON.stringify(result))
  }
}
async function together(promises) {
  const results = await Promise.allSettled(promises)
  const errors = results.filter(item => item.status === 'rejected').map(item => item.reason)
  if (errors.length) throw new AggregateError(errors, errors.map(safe).join('\n'))
  return results.map(item => item.value)
}
async function finished(id, token) {
  const deadline = Date.now() + 120_000
  while (Date.now() < deadline) {
    const value = await api(`/operations/${id}`, 'GET', undefined, token)
    if (['succeeded', 'failed', 'partially_applied'].includes(value.state)) return value
    await delay(200)
  }
  throw new Error(`任务 ${id} 在120秒内未完成`)
}
async function completed(id, token) {
  const value = await finished(id, token)
  assert.equal(value.state, 'succeeded', `${value.phase}: ${value.error}`)
  assert.equal(value.completed, value.total)
  return value
}
const detail = env => api(`/environments/${env.id}`)
const peers = env => api(`/environments/${env.id}/vpn-access`)
async function state(env) {
  const value = await api(`/environments/${env.id}/state`)
  value.assets.forEach(asset => instances.add(asset.instanceId))
  return value
}
async function apply(env, spec) {
  const current = await detail(env)
  return api(`/environments/${env.id}/changes`, 'POST', { expectedRevision: current.revision, spec, apply: true, clientRequestId: randomUUID() })
}
async function destroy(env) {
  const current = await detail(env)
  if (current.status === 'destroyed') return
  if (current.operationId) await finished(current.operationId)
  await completed((await api(`/environments/${env.id}/actions`, 'POST', { action: 'destroy', clientRequestId: randomUUID() })).id)
}
async function node(nodeId, ...args) {
  assert.ok(workers.has(nodeId), `缺少节点连接记录 ${nodeId}`)
  const worker = workers.get(nodeId)
  const command = worker ? ['ssh', '-i', worker.keyPath, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${worker.keyPath}.hosts`, `root@${worker.host}`, args.map(quote).join(' ')] : args
  try { return (await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', ...command], { encoding: 'utf8', timeout: 30_000, maxBuffer: 2 ** 20 })).stdout }
  catch (error) { throw new Error(`节点 ${nodeId}: ${error.stderr?.trim() || safe(error)}`) }
}
async function local(...args) {
  try { return (await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', ...args], { encoding: 'utf8', timeout: 30_000, maxBuffer: 2 ** 20 })).stdout }
  catch (error) { throw new Error(error.stderr?.trim() || safe(error)) }
}
function sql(query) {
  return execFileSync('docker', ['exec', 'netlab-postgres-1', 'psql', '-U', 'netlab', '-d', 'netlab', '-At', '-v', 'ON_ERROR_STOP=1', '-c', query], { encoding: 'utf8', timeout: 10_000 })
}
function asset(name, networks) {
  return { id: randomUUID(), name, templateId: template.id, resources: { ...template.resources, cpu: 1, memoryMiB: 128 }, interfaces: networks.map((nw, index) => ({ id: randomUUID(), networkId: nw.id, address: '', mac: '', primary: index === 0 })) }
}
async function initialize(env) {
  const current = await detail(env), actual = await state(env)
  await together(current.spec.assets.filter(item => item.templateId === template.id).map(async item => {
    const target = actual.assets.find(value => value.assetId === item.id)
    await node(target.nodeId, 'ctr', '-n', 'netlab', 'tasks', 'exec', '--exec-id', randomUUID(), target.instanceId, 'sh', '-c', `printf %s ${quote(`${env.id}/${item.id}`)} > /usr/share/nginx/html/environment-id; head -c 2097152 /dev/zero > /usr/share/nginx/html/large.bin`)
  }))
}
async function configure(client) {
  const connection = await api(`/environments/${client.env.id}/vpn-access/${client.id}/connection`)
  for (const prefix of client.connection?.allowedIPs || [])
    if (!connection.allowedIPs.includes(prefix)) await local('ip', '-n', client.ns, 'route', 'del', prefix, 'dev', client.iface)
  await local('ip', 'netns', 'exec', client.ns, 'wg', 'set', client.iface, 'peer', connection.publicKey, 'allowed-ips', connection.allowedIPs.join(','), 'endpoint', connection.endpoint, 'persistent-keepalive', '25')
  for (const address of connection.addresses) await local('ip', '-n', client.ns, 'address', 'replace', address, 'dev', client.iface, ...(address.includes(':') ? ['nodad'] : []))
  await local('ip', '-n', client.ns, 'link', 'set', client.iface, 'mtu', String(connection.mtu), 'up')
  for (const prefix of connection.allowedIPs) await local('ip', '-n', client.ns, 'route', 'replace', prefix, 'dev', client.iface)
  client.connection = connection
  client.routes = (await peers(client.env)).find(item => item.id === client.id).routes
  assert.ok(connection.allowedIPs.some(value => value.includes(':')) === client.routes.some(value => value.cidr.includes(':')), '连接地址族与授权网段不一致')
}
async function connect(env, mode, networks, space, token) {
  const pair = generateKeyPairSync('x25519')
  const privateKey = Buffer.from(pair.privateKey.export({ format: 'jwk' }).d, 'base64url').toString('base64')
  const publicKey = Buffer.from(pair.publicKey.export({ format: 'jwk' }).x, 'base64url').toString('base64')
  const current = await detail(env), name = `${mode}-${clients.length}`
  const body = { name, publicKey, mode, networkIds: networks.map(item => item.id), expectedRevision: current.revision, clientRequestId: randomUUID() }
  const operation = await api(`/environments/${env.id}/vpn-access`, 'POST', body, token)
  assert.equal((await api(`/environments/${env.id}/vpn-access`, 'POST', body, token)).id, operation.id)
  assert.equal((await completed(operation.id, token)).total, 1)
  const peer = (await peers(env)).find(item => item.publicKey === publicKey)
  assert.equal(peer.state, 'active')
  const client = { env, id: peer.id, publicKey, name, mode, routes: peer.routes, ns: `nltest-${runId}-${space}`, iface: `nc${runId}${clients.length}` }
  clients.push(client)
  if (!namespaces.has(client.ns)) { await local('ip', 'netns', 'add', client.ns); namespaces.add(client.ns); await local('ip', '-n', client.ns, 'link', 'set', 'lo', 'up') }
  await local('ip', 'link', 'add', client.iface, 'type', 'wireguard')
  hostInterfaces.add(client.iface)
  await local('ip', 'link', 'set', client.iface, 'netns', client.ns)
  hostInterfaces.delete(client.iface)
  execFileSync('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', 'ip', 'netns', 'exec', client.ns, 'wg', 'set', client.iface, 'private-key', '/dev/stdin'], { input: `${privateKey}\n`, stdio: ['pipe', 'ignore', 'pipe'], timeout: 10_000 })
  await configure(client)
  const owners = (await together([...workers.keys()].map(async id => ({ id, names: await node(id, 'ip', 'netns', 'list') })))).filter(item => item.names.split('\n').some(line => line.split(' ')[0] === vpnName(env)))
  assert.equal(owners.length, 1, 'VPN 必须有且只有一个真实网关')
  client.owner = owners[0].id
  for (const route of client.routes) assert.ok((route.cidr === route.accessCidr) === (mode === 'original'), 'VPN 地址模式没有实际生效')
  return client
}
async function targets(client, large = false) {
  const current = await detail(client.env)
  const records = current.spec.assets.filter(item => item.templateId === template.id).flatMap(item => item.interfaces.flatMap(iface => {
    const route = client.routes.find(value => value.networkId === iface.networkId)
    return route ? [{ route, address: iface.address, assetId: item.id }] : []
  }))
  const mapped = JSON.parse(await local('python3', '-c', 'import ipaddress,json,sys; records=json.loads(sys.argv[1]); print(json.dumps([str(ipaddress.ip_address(int(ipaddress.ip_network(r["route"]["accessCidr"]).network_address)+int(ipaddress.ip_address(r["address"]))-int(ipaddress.ip_network(r["route"]["cidr"]).network_address))) for r in records]))', JSON.stringify(records)))
  return records.map((item, index) => ({ url: `http://${mapped[index].includes(':') ? `[${mapped[index]}]` : mapped[index]}/${large ? 'large.bin' : 'environment-id'}`, ...(large ? { size: 2097152 } : { body: `${client.env.id}/${item.assetId}` }) }))
}
async function traffic(client, large = false) {
  const actual = await targets(client, large)
  assert.ok(actual.length > 0, '没有真实测试目标')
  const requests = Array.from({ length: large ? 8 : 16 }, (_, index) => actual[index % actual.length])
  const result = JSON.parse(await local('ip', 'netns', 'exec', client.ns, 'python3', '-c', payload, JSON.stringify(requests), large ? '4' : '8'))
  report.traffic.push({ environmentId: client.env.id, mode: client.mode, large, ...result })
  assert.ok((await local('ip', 'netns', 'exec', client.ns, 'wg', 'show', client.iface, 'latest-handshakes')).split('\n').some(line => Number(line.split(/\s+/)[1]) > 0), '数据访问没有建立真实 WireGuard 握手')
}
async function blocked(client, url) {
  await assert.rejects(local('ip', 'netns', 'exec', client.ns, 'curl', '--noproxy', '*', '--fail', '--max-time', '3', url), '未经授权的连接仍可访问')
}
async function vmAccess(client) {
  const current = await detail(client.env), actual = await state(client.env)
  const item = current.spec.assets.find(value => value.templateId === vm.id)
  const instance = actual.assets.find(value => value.assetId === item.id)
  const iface = item.interfaces[0], route = client.routes.find(value => value.networkId === iface.networkId)
  const address = (await local('python3', '-c', 'import ipaddress,sys; print(ipaddress.ip_address(int(ipaddress.ip_network(sys.argv[1]).network_address)+int(ipaddress.ip_address(sys.argv[2]))-int(ipaddress.ip_network(sys.argv[3]).network_address)))', route.accessCidr, iface.address, route.cidr)).trim()
  const deadline = Date.now() + 120_000
  let scanned, lastError
  while (!scanned && Date.now() < deadline) {
    try { scanned = await local('ip', 'netns', 'exec', client.ns, 'ssh-keyscan', '-T', '2', '-t', 'ed25519', address) }
    catch (error) { lastError = error; await delay(500) }
  }
  assert.ok(scanned?.trim(), `真实来宾 SSH 未就绪：${safe(lastError || 'no host key')}`)
  const hosts = scanned.split('\n').filter(line => line && !line.startsWith('#')).map(line => `${instance.instanceId} ${line.split(' ').slice(1).join(' ')}`).join('\n')
  execFileSync('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', 'tee', '-a', `${key.path}.hosts`], { input: `${hosts}\n`, stdio: ['pipe', 'ignore', 'pipe'], timeout: 10_000 })
  const args = ['ip', 'netns', 'exec', client.ns, 'ssh', '-i', key.path, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${key.path}.hosts`, '-o', `HostKeyAlias=${instance.instanceId}`, '-o', 'ConnectTimeout=3', `netlab@${address}`]
  assert.equal((await local(...args, 'uname', '-s')).trim(), 'Linux')
  const boot = (await local(...args, 'cat', '/proc/sys/kernel/random/boot_id')).trim()
  assert.notEqual(boot, (await node(instance.nodeId, 'cat', '/proc/sys/kernel/random/boot_id')).trim(), '访问没有到达独立 VM 来宾')
  report.traffic.push({ environmentId: client.env.id, mode: client.mode, protocol: 'IPv6 SSH', guest: 'Ubuntu', verified: true })
}
async function revoke(client) {
  const current = await detail(client.env)
  await completed((await api(`/environments/${client.env.id}/vpn-access/${client.id}?expectedRevision=${current.revision}&clientRequestId=${randomUUID()}`, 'DELETE')).id)
  assert.ok(!(await peers(client.env)).some(item => item.id === client.id), '已撤销 Peer 仍被列为有效')
}
async function nativeClean(env) {
  for (const id of workers.keys()) {
    const names = await node(id, 'ip', 'netns', 'list')
    assert.ok(!names.split('\n').some(line => line.split(' ')[0] === vpnName(env)), 'VPN namespace 残留')
    const links = JSON.parse(await node(id, 'ip', '-json', 'link', 'show'))
    assert.ok(!links.some(link => link.ifname === vpnDevice(env)), 'VPN veth 残留')
    await node(id, 'test', '!', '-e', `/var/lib/netlab-dev/vpn/${env.id}.json`)
    assert.equal((await node(id, 'find', '/var/lib/netlab-dev/environments', '-path', `/var/lib/netlab-dev/environments/${env.id}/*`, '-type', 'f')).trim(), '', '运行环境磁盘或配置残留')
    assert.ok(!(await node(id, 'ovs-vsctl', '--columns=external_ids', 'list', 'Interface')).includes(env.id), 'OVS 接口残留')
  }
  for (const table of ['Logical_Switch', 'Logical_Router', 'Logical_Switch_Port', 'Logical_Router_Port', 'NAT', 'Address_Set', 'Load_Balancer'])
    assert.ok(!wsl('ovn-nbctl', '--columns=external_ids', 'list', table).includes(env.id), `${table} 残留`)
}

try {
  await step('复用常驻双 Worker 和真实镜像，创建重叠双栈环境', async () => {
    assert.ok(['127.0.0.1', 'localhost', '[::1]'].includes(new URL(base).hostname), '本脚本包含本地数据库故障注入，仅用于本地验收环境')
    await api('/sessions/login', 'POST', JSON.parse(await readFile('data/dev-login.json', 'utf8')))
    baseline = await api('/nodes')
    const primary = wsl('cat', '/var/lib/netlab-dev/node-id').trim()
    workers.set(primary, null)
    const records = JSON.parse(await readFile(process.env.NETLAB_WORKERS_FILE || 'D:/.cache/netlab/artifacts/multi-node-workers.json', 'utf8'))
    for (const record of records) if (record.nodeId !== primary && baseline.some(item => item.id === record.nodeId)) workers.set(record.nodeId, record)
    assert.ok(baseline.every(item => item.state === 'ready' && workers.has(item.id)), '现有节点尚未就绪或缺少连接记录')
    const templates = await api('/templates?limit=100')
    template = templates.find(item => item.kind === 'container' && item.state === 'ready' && item.name === 'API container')
    vm = templates.find(item => item.kind === 'vm' && item.state === 'ready' && item.os === 'Ubuntu 24.04')
    assert.ok(template && vm, '现有 Nginx 或 Ubuntu VM 模板尚未就绪')
    key = guestKey()
    payload = await readFile(new URL('./fixtures/network-load.py', import.meta.url), 'utf8')
    const created = await together(['A', 'B'].map(async name => {
      const networks = [{ id: randomUUID(), name: 'IPv4', cidr: '10.87.0.0/24' }, { id: randomUUID(), name: 'IPv6', cidr: 'fd87::/64' }, { id: randomUUID(), name: '未授权网段', cidr: '10.87.1.0/24' }]
      const assets = [asset('web', networks.slice(0, 2)), asset('private', networks.slice(2))]
      if (name === 'A') assets.push({ ...asset('Ubuntu VPN', networks.slice(1, 2)), templateId: vm.id, resources: vm.resources, guest: { username: 'netlab', sshAuthorizedKeys: [key.publicKey] } })
      const env = await api('/environments', 'POST', { name: `VPN 验收 ${name}`, run: true, spec: { networks, assets }, clientRequestId: randomUUID() })
      environments.push(env); report.environmentIds.push(env.id)
      await completed(env.operationId)
      assert.ok((await state(env)).assets.every(item => item.state === 'running'), '部署完成但资产未实际运行')
      await initialize(env)
      return env
    }))
    environments.splice(0, environments.length, ...created)
    report.assetExecution = { environments: environments.length, assets: (await together(environments.map(state))).reduce((sum, value) => sum + value.assets.length, 0), state: 'running' }
  })
  const [a, b] = environments
  let originalA, originalB, translatedA, translatedB, limited
  await step('VPN 创建提交失败后恢复现场并释放本次地址和端口', async () => {
    const publicKey = Buffer.from(generateKeyPairSync('x25519').publicKey.export({ format: 'jwk' }).x, 'base64url').toString('base64')
    trigger = `netlab_vpn_test_${runId}`
    sql(`BEGIN; CREATE FUNCTION ${trigger}() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.environment_id='${a.id}' AND NEW.applied THEN RAISE EXCEPTION 'vpn acceptance access commit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER ${trigger} BEFORE UPDATE OF applied ON vpn_access FOR EACH ROW EXECUTE FUNCTION ${trigger}(); COMMIT;`)
    const current = await detail(a)
    const failed = await finished((await api(`/environments/${a.id}/vpn-access`, 'POST', { name: 'failed-create', publicKey, mode: 'translated', networkIds: current.spec.networks.slice(0, 2).map(item => item.id), expectedRevision: current.revision })).id)
    assert.equal(failed.state, 'failed')
    assert.equal(failed.phase, 'rolled-back', failed.error)
    assert.match(failed.error, /vpn acceptance access commit failure/)
    sql(`DROP TRIGGER ${trigger} ON vpn_access; DROP FUNCTION ${trigger}();`); trigger = undefined
    assert.equal(sql(`SELECT count(*) FROM service_ports WHERE environment_id='${a.id}' AND purpose='vpn'; SELECT count(*) FROM vpn_aliases WHERE environment_id='${a.id}';`).trim(), '0\n0')
    for (const id of workers.keys()) assert.ok(!(await node(id, 'ip', 'netns', 'list')).includes(vpnName(a)), '失败创建仍有活动 VPN namespace')
    const failedPeer = (await peers(a)).find(item => item.publicKey === publicKey)
    assert.equal(failedPeer.state, 'failed')
    await completed((await api(`/environments/${a.id}/vpn-access/${failedPeer.id}?expectedRevision=${current.revision}`, 'DELETE')).id)
  })
  await step('original 双栈连接与 translated 同客户端访问重叠环境', async () => {
    originalA = await connect(a, 'original', (await detail(a)).spec.networks.slice(0, 2), 'oa')
    originalB = await connect(b, 'original', (await detail(b)).spec.networks.slice(0, 2), 'ob')
    translatedA = await connect(a, 'translated', (await detail(a)).spec.networks.slice(0, 2), 'translated')
    translatedB = await connect(b, 'translated', (await detail(b)).spec.networks.slice(0, 2), 'translated')
    assert.ok(translatedA.routes.every(route => !translatedB.routes.some(other => other.accessCidr === route.accessCidr)), '重叠环境获得了重复访问前缀')
    for (const client of clients) assert.ok(client.connection.addresses.some(item => item.includes(':')) && client.connection.addresses.some(item => !item.includes(':')), '双栈客户端地址缺失')
    await together(clients.map(client => traffic(client)))
    await together([translatedA, translatedB].map(client => traffic(client, true)))
    await vmAccess(originalA)
    await vmAccess(translatedA)
    report.realAccess = { peers: clients.length, protocols: ['IPv4 HTTP', 'IPv6 HTTP'], verified: true }
  })
  await step('多 Peer 地址稳定、环境授权和资产授权越权拒绝', async () => {
    const before = translatedA.connection
    const issued = await api('/service-tokens', 'POST', { name: `VPN ${runId}`, grants: [{ scopeKind: 'environment', scopeId: a.id, permissions: ['access'] }] })
    principals.push(issued.principal.id)
    limited = await connect(a, 'original', (await detail(a)).spec.networks.slice(0, 1), 'limited', issued.token)
    await api(`/environments/${b.id}/vpn-access`, 'GET', undefined, issued.token, 403)
    await api(`/environments/${b.id}/vpn-access/${translatedB.id}/connection`, 'GET', undefined, issued.token, 403)
    const current = await detail(a)
    const narrow = await api('/service-tokens', 'POST', { name: `VPN asset ${runId}`, grants: [{ scopeKind: 'asset', scopeId: `${a.id}/${current.spec.assets[0].id}`, permissions: ['read', 'access'] }] })
    principals.push(narrow.principal.id)
    await api(`/environments/${a.id}/vpn-access`, 'GET', undefined, narrow.token, 403)
    await api(`/environments/${a.id}/vpn-access`, 'POST', { name: 'forbidden', publicKey: limited.publicKey, networkIds: [current.spec.networks[0].id], expectedRevision: current.revision }, narrow.token, 403)
    assert.ok(JSON.stringify(await api(`/environments/${a.id}/vpn-access/${translatedA.id}/connection`)) === JSON.stringify(before), '新增 Peer 改变了旧连接地址或参数')
    const destination = current.spec.assets.find(item => item.name === 'private').interfaces[0].address
    const denied = current.spec.networks[2].cidr
    await local('ip', 'netns', 'exec', limited.ns, 'wg', 'set', limited.iface, 'peer', limited.connection.publicKey, 'allowed-ips', [...limited.connection.allowedIPs, denied].join(','))
    await local('ip', '-n', limited.ns, 'route', 'add', denied, 'dev', limited.iface)
    await blocked(limited, `http://${destination}/environment-id`)
    await local('ip', '-n', limited.ns, 'route', 'del', denied, 'dev', limited.iface)
    await configure(limited)
    await together([originalA, translatedA, limited].map(client => traffic(client)))
  })
  await step('网关 Agent 停止和重启保持原 VPN 配置及真实访问', async () => {
    const before = translatedA.connection
    await node(translatedA.owner, 'systemctl', 'stop', 'netlab-node-dev.service')
    try {
      await together([originalA, translatedA, translatedB].map(client => traffic(client)))
    } finally {
      await node(translatedA.owner, 'systemctl', 'start', 'netlab-node-dev.service')
    }
    const deadline = Date.now() + 15_000
    while (Date.now() < deadline && !(await api('/nodes')).find(item => item.id === translatedA.owner && item.state === 'ready')) await delay(200)
    assert.ok((await api('/nodes')).find(item => item.id === translatedA.owner && item.state === 'ready'), '网关节点未恢复接入')
    assert.deepEqual(await api(`/environments/${a.id}/vpn-access/${translatedA.id}/connection`), before)
    await together([originalA, translatedA, limited, translatedB].map(client => traffic(client)))
  })
  await step('真实父提交失败恢复原 spec、Peer、地址及数据面', async () => {
    const before = await detail(a), oldPeers = await peers(a), oldConnection = translatedA.connection
    const name = `netlab_vpn_test_${runId}`
    sql(`BEGIN; CREATE FUNCTION ${name}() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='${a.id}' AND NEW.revision<>OLD.revision AND NEW.applied_spec IS DISTINCT FROM OLD.applied_spec THEN RAISE EXCEPTION 'vpn acceptance parent commit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER ${name} BEFORE UPDATE OF applied_spec ON environments FOR EACH ROW EXECUTE FUNCTION ${name}(); COMMIT;`)
    trigger = name
    const changed = structuredClone(before.spec)
    changed.networks[0].cidr = '10.88.0.0/24'
    delete changed.networks[0].gateway
    for (const item of changed.assets) for (const iface of item.interfaces) if (iface.networkId === changed.networks[0].id) iface.address = ''
    const failed = await finished((await apply(a, changed)).id)
    assert.equal(failed.state, 'failed')
    assert.equal(failed.phase, 'rolled-back', failed.error)
    assert.match(failed.error, /vpn acceptance parent commit failure/)
    sql(`DROP TRIGGER ${trigger} ON environments; DROP FUNCTION ${trigger}();`); trigger = undefined
    const after = await detail(a)
    assert.equal(after.revision, before.revision)
    assert.ok(JSON.stringify(after.spec) === JSON.stringify(before.spec), '父提交失败后场景事实改变')
    assert.ok(JSON.stringify(await peers(a)) === JSON.stringify(oldPeers), '父提交失败后 Peer 或地址改变')
    assert.ok(JSON.stringify(await api(`/environments/${a.id}/vpn-access/${translatedA.id}/connection`)) === JSON.stringify(oldConnection), '回滚改变了客户端配置')
    await together([originalA, translatedA, limited].map(client => traffic(client)))
    await traffic(translatedB)
  })
  await step('资产新增与网段热更新保持身份和 VPN 地址', async () => {
    const before = await detail(a), previous = await state(a)
    const changed = structuredClone(before.spec)
    changed.networks[0].cidr = '10.88.0.0/24'; delete changed.networks[0].gateway
    for (const item of changed.assets) for (const iface of item.interfaces) if (iface.networkId === changed.networks[0].id) iface.address = ''
    const added = asset('hot-added', changed.networks.slice(0, 2)); changed.assets.push(added)
    await completed((await apply(a, changed)).id)
    const actual = await state(a)
    for (const old of previous.assets) assert.equal(actual.assets.find(item => item.assetId === old.assetId).instanceId, old.instanceId, '热更新重新创建了既有资产')
    assert.equal((await detail(a)).revision, before.revision + 1)
    await initialize(a)
    for (const client of [originalA, translatedA, limited]) {
      const addresses = client.connection.addresses
      await configure(client)
      assert.deepEqual(client.connection.addresses, addresses, '热更新改变了 VPN 客户端地址')
    }
    await together([originalA, translatedA, limited, translatedB].map(client => traffic(client)))
    const latest = await detail(a), removed = structuredClone(latest.spec)
    removed.assets = removed.assets.filter(item => item.id !== added.id && item.templateId !== vm.id)
    removed.networks = removed.networks.filter(item => item.id !== latest.spec.networks[1].id)
    for (const item of removed.assets) item.interfaces = item.interfaces.filter(iface => iface.networkId !== latest.spec.networks[1].id)
    await completed((await apply(a, removed)).id)
    for (const client of [originalA, translatedA, limited]) await configure(client)
    assert.ok((await peers(a)).every(peer => peer.routes.every(route => !route.cidr.includes(':'))), '被删除网段仍在 VPN 授权中')
    await initialize(a)
    await together([originalA, translatedA, limited, originalB, translatedB].map(client => traffic(client)))
  })
  await step('单 Peer 撤销立即断连，其余 Peer 和环境继续访问', async () => {
    const target = (await targets(originalA))[0].url
    const before = translatedA.connection
    await revoke(originalA)
    await blocked(originalA, target)
    const nativePeers = await node(originalA.owner, 'ip', 'netns', 'exec', vpnName(a), 'wg', 'show', 'wg0', 'peers')
    assert.ok(!nativePeers.split(/\s+/).includes(originalA.publicKey), '服务器仍持有被撤销 Peer')
    assert.ok(JSON.stringify(await api(`/environments/${a.id}/vpn-access/${translatedA.id}/connection`)) === JSON.stringify(before), '撤销其他 Peer 改变了当前配置')
    await together([translatedA, limited, translatedB].map(client => traffic(client)))
  })
  await step('环境销毁清理 WG、OVN、OVS、端口、实例与数据库', async () => {
    const oldTargets = await together([translatedA, translatedB].map(async client => ({ client, url: (await targets(client))[0].url })))
    const ports = clients.map(client => ({ nodeId: client.owner, port: Number(client.connection.endpoint.split(':').at(-1)) }))
    await together(environments.map(destroy))
    for (const env of environments) {
      assert.equal((await detail(env)).status, 'destroyed')
      assert.deepEqual((await state(env)).assets, [])
      assert.deepEqual(await peers(env), [])
      await nativeClean(env)
    }
    await together(oldTargets.map(({ client, url }) => blocked(client, url)))
    for (const { nodeId, port } of ports) await node(nodeId, 'python3', '-c', `import socket; s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(('0.0.0.0',${port})); s.close()`)
    const nodes = await api('/nodes')
    for (const old of baseline) assert.deepEqual(nodes.find(item => item.id === old.id).reserved, old.reserved)
    for (const id of workers.keys()) {
      const active = new Set([...(await node(id, 'ctr', '-n', 'netlab', 'containers', 'list', '-q')).trim().split(/\s+/), ...(await node(id, 'virsh', 'list', '--all', '--name')).trim().split(/\s+/)])
      assert.ok([...instances].every(instance => !active.has(instance)), '容器或 libvirt VM 实例残留')
    }
    const ids = report.environmentIds.map(id => `'${id}'`).join(',')
    const remaining = sql(['vpn_access', 'vpn_aliases', 'service_ports', 'runtime_assets'].map(table => `select count(*) from ${table} where environment_id in (${ids});`).join(' '))
    assert.equal(remaining.trim(), '0\n0\n0\n0')
    assert.equal(sql(`select count(*) from environments where id in (${ids}) and (vpn_public_key is not null or vpn_mtu is not null);`).trim(), '0')
  })
  report.passed = true
} catch (error) {
  report.passed = false; report.error = safe(error); process.exitCode = 1
} finally {
  const failures = []
  const clean = async (name, run) => { try { await run() } catch (error) { failures.push({ name, error: safe(error) }) } }
  if (trigger) await clean('删除本次故障 trigger', () => sql(`DROP TRIGGER IF EXISTS ${trigger} ON environments; DROP TRIGGER IF EXISTS ${trigger} ON vpn_access; DROP FUNCTION ${trigger}();`))
  for (const env of environments) await clean(`清理环境 ${env.id}`, () => destroy(env))
  for (const id of principals) await clean(`撤销测试 Token ${id}`, () => api(`/service-tokens/${id}`, 'DELETE'))
  for (const ns of namespaces) await clean(`删除测试 namespace ${ns}`, () => local('ip', 'netns', 'delete', ns))
  for (const iface of hostInterfaces) await clean(`删除测试 WG ${iface}`, () => local('ip', 'link', 'delete', iface))
  if (key) await clean('删除测试 SSH 密钥', () => key.remove())
  report.cleanupErrors = failures
  if (failures.length) { report.passed = false; process.exitCode = 1 }
  report.finishedAt = new Date().toISOString()
  await mkdir('data', { recursive: true })
  await writeFile(reportPath, JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ passed: report.passed, steps: report.steps.length, report: reportPath, cleanupErrors: failures.length }))
}
