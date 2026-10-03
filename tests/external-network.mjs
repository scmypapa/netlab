import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { execFile } from 'node:child_process'
import { readFile, writeFile } from 'node:fs/promises'
import { promisify } from 'node:util'
import { delay, guestKey, wsl } from './guest-ssh.mjs'

const execute = promisify(execFile)
const base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const report = { startedAt: new Date().toISOString(), steps: [], environmentIds: [] }
const envs = [], workers = new Map()
let cookie, main, other, primary, container, vm, baseline, key, trigger, fixture = false
const quote = value => `'${String(value).replaceAll("'", "'\"'\"'")}'`
async function api(path, method = 'GET', body, authorization) {
  const response = await fetch(`${base}/api/v1${path}`, { method, headers: { ...(authorization ? { Authorization: `Bearer ${authorization}` } : { Cookie: cookie || '' }), 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(15000) })
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie')?.split(';')[0]
  const value = response.status === 204 ? undefined : await response.json()
  assert.ok(response.ok, `${response.status} ${method} ${path}: ${JSON.stringify(value)}`)
  return value
}
async function step(name, run) {
  const began = performance.now(), result = { name, passed: false }
  try { await run(); result.passed = true } catch (error) { result.error = error.message; throw error } finally { result.durationMs = Math.round(performance.now() - began); report.steps.push(result); console.log(JSON.stringify(result)) }
}
async function finished(id, expected = 'succeeded') {
  const deadline = Date.now() + 120000
  while (Date.now() < deadline) {
    const op = await api(`/operations/${id}`)
    if (['succeeded', 'failed', 'partially_applied'].includes(op.state)) { assert.equal(op.state, expected, `${op.phase}: ${op.error}`); return op }
    await delay(200)
  }
  throw new Error(`operation timeout ${id}`)
}
const detail = env => api(`/environments/${env.id}`)
const state = env => api(`/environments/${env.id}/state`)
async function change(env, spec, expected) {
  const current = await detail(env)
  return finished((await api(`/environments/${env.id}/changes`, 'POST', { spec, expectedRevision: current.revision, clientRequestId: randomUUID(), apply: true })).id, expected)
}
async function node(id, ...args) {
  const worker = workers.get(id)
  const command = worker ? ['ssh', '-i', worker.keyPath, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${worker.keyPath}.hosts`, `root@${worker.host}`, args.map(quote).join(' ')] : args
  const value = await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', ...command], { encoding: 'utf8', timeout: 30000, maxBuffer: 2 ** 20 })
  return value.stdout
}
async function inside(asset, ...args) {
  const task = (await node(asset.nodeId, 'ctr', '-n', 'netlab', 'tasks', 'list')).split('\n').find(line => line.trim().split(/\s+/)[0] === asset.instanceId)
  const pid = task?.trim().split(/\s+/)[1]
  assert.match(pid || '', /^\d+$/)
  return node(asset.nodeId, 'nsenter', `--net=/proc/${pid}/ns/net`, ...args)
}
const sql = statement => execute('docker', ['exec', 'netlab-postgres-1', 'psql', '-U', 'netlab', '-d', 'netlab', '-v', 'ON_ERROR_STOP=1', '-Atc', statement], { encoding: 'utf8', timeout: 15000 }).then(result => result.stdout.trim())
async function create(name, networks, templates) {
  const assets = templates.map((template, index) => ({ id: randomUUID(), name: `${template.kind}-${index + 1}`, templateId: template.id, resources: template.resources, interfaces: networks.map((network, index) => ({ id: randomUUID(), networkId: network.id, mac: '', address: '', primary: index === 0 })), ...(template.kind === 'vm' ? { guest: { username: 'netlab', sshAuthorizedKeys: [key.publicKey] } } : {}) }))
  const env = await api('/environments', 'POST', { name, spec: { networks, assets }, run: true, clientRequestId: randomUUID() })
  envs.push(env); report.environmentIds.push(env.id)
  return env
}
async function verifyLAN(env, marker, ipv6 = false) {
  const current = await detail(env), actual = await state(env)
  for (const asset of current.spec.assets.filter(asset => asset.templateId === container.id)) {
    const target = actual.assets.find(item => item.assetId === asset.id)
    const address = ipv6 ? '[fd11::1]' : '192.0.2.1'
    if (ipv6) {
      const deadline = Date.now() + 10000
      for (;;) {
        const addresses = await inside(target, 'ip', '-6', '-o', 'address', 'show')
        assert.ok(!addresses.includes('dadfailed'), addresses)
        if (!addresses.includes('tentative')) break
        assert.ok(Date.now() < deadline, 'IPv6 地址仍在重复地址检测中')
        await delay(100)
      }
    }
    assert.equal((await inside(target, 'curl', '-g', '--noproxy', '*', '-fsS', '--max-time', '5', `http://${address}:9000/`)).trim(), marker)
    const guestIP = asset.interfaces[0].address
    await node(target.nodeId, 'ctr', '-n', 'netlab', 'tasks', 'exec', '--exec-id', randomUUID(), target.instanceId, 'sh', '-c', `printf %s ${env.id} > /usr/share/nginx/html/environment-id`)
    assert.equal((await node(primary, 'ip', 'netns', 'exec', marker === 'native' ? 'nlt-switch' : `nlt-lan${marker}`, 'curl', '-g', '--noproxy', '*', '-fsS', '--max-time', '5', `http://${ipv6 ? `[${guestIP}]` : guestIP}/environment-id`)).trim(), env.id)
  }
}
async function snapshot(name) {
  const links = JSON.parse(await node(primary, 'ip', '-json', 'address', 'show', 'dev', name))
  const item = links[0]
  return { mac: item.address, mtu: item.mtu, addresses: item.addr_info.map(({ family, local, prefixlen }) => ({ family, local, prefixlen })).sort((a, b) => a.local.localeCompare(b.local)), routes: (await node(primary, 'ip', 'route', 'show', 'dev', name)).trim() }
}
async function destroy(env) {
  if ((await detail(env)).status === 'destroyed') return
  await finished((await api(`/environments/${env.id}/actions`, 'POST', { action: 'destroy', clientRequestId: randomUUID() })).id)
}
try {
  await step('复用双 Worker 和镜像，建立真实 Linux 外部 LAN', async () => {
    assert.ok(['127.0.0.1', 'localhost'].includes(new URL(base).hostname), '该故障注入脚本限本地环境')
    await api('/sessions/login', 'POST', JSON.parse(await readFile('data/dev-login.json', 'utf8')))
    primary = wsl('cat', '/var/lib/netlab-dev/node-id').trim(); workers.set(primary, null)
    for (const record of JSON.parse(await readFile('D:/.cache/netlab/artifacts/multi-node-workers.json', 'utf8'))) if (record.nodeId !== primary) workers.set(record.nodeId, record)
    await node(primary, 'bash', '/mnt/d/newgz/netlab/tests/fixtures/external-lan.sh'); fixture = true
    baseline = await api('/nodes')
    assert.ok(baseline.every(item => item.state === 'ready'))
    assert.ok((await api(`/nodes/${primary}/interfaces`)).some(item => item.name === 'nlt-uplink' && item.available))
    report.originalLinuxBridge = await snapshot('nlt-linux'); report.originalOVSBridge = await snapshot('nlt-ovs')
    report.originalUplink = await snapshot('nlt-uplink')
    const templates = await api('/templates?limit=100')
    container = templates.find(item => item.name === 'API container' && item.state === 'ready')
    vm = templates.find(item => item.kind === 'vm' && item.os === 'Ubuntu 24.04' && item.state === 'ready')
    assert.ok(container && vm); key = guestKey()
    const networks = [{ id: randomUUID(), name: 'LAN 100', cidr: '192.0.2.0/24', gateway: '192.0.2.1', allocationPool: '192.0.2.128/26', external: { nodeId: primary, interface: 'nlt-uplink', vlan: 100 } }]
    main = await create('外部 LAN 混合验收', networks, [container, container, container, container, vm]); await finished(main.operationId)
    report.assets = (await state(main)).assets.map(({ assetId, nodeId, instanceId }) => ({ assetId, nodeId, instanceId }))
    assert.ok(report.assets.some(asset => asset.nodeId !== primary), '未实际覆盖跨节点资产')
  })
  await step('跨节点 VLAN 双向访问，真实 Ubuntu 保留外部网关', async () => {
    await verifyLAN(main, '100')
    const current = await detail(main), actual = await state(main)
    const asset = current.spec.assets.find(item => item.templateId === vm.id), target = actual.assets.find(item => item.assetId === asset.id)
    const op = await api(`/environments/${main.id}/assets/${asset.id}/services`, 'POST', { protocol: 'tcp', targetPort: 22, interfaceId: asset.interfaces[0].id, expectedRevision: current.revision, clientRequestId: randomUUID() })
    await finished(op.id)
    const binding = (await api(`/environments/${main.id}/services`)).find(item => item.assetId === asset.id)
    let scanned = '', deadline = Date.now() + 120000
    while (!scanned.trim() && Date.now() < deadline) { try { scanned = await node(primary, 'ssh-keyscan', '-T', '2', '-p', String(binding.port), binding.address) } catch { await delay(500) } }
    assert.ok(scanned.trim(), 'Ubuntu SSH 未启动')
    const known = scanned.split('\n').filter(line => line && !line.startsWith('#')).map(line => `${target.instanceId} ${line.split(' ').slice(1).join(' ')}`).join('\n')
    await node(primary, 'python3', '-c', 'import pathlib,sys;pathlib.Path(sys.argv[1]).write_text(sys.argv[2])', `${key.path}.hosts`, `${known}\n`)
    const output = await node(primary, 'ssh', '-i', key.path, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${key.path}.hosts`, '-o', `HostKeyAlias=${target.instanceId}`, '-p', String(binding.port), `netlab@${binding.address}`, 'ip route show default; cat /proc/sys/kernel/random/boot_id')
    assert.match(output, /default via 192\.0\.2\.1/); assert.ok(!output.includes((await node(target.nodeId, 'cat', '/proc/sys/kernel/random/boot_id')).trim()))
  })
  await step('外部接口租约冲突失败，原环境与容量保持', async () => {
    const current = await detail(main)
    const conflict = await create('外部端口冲突验收', current.spec.networks.map(item => ({ ...item, id: randomUUID() })), [container])
    const failed = await finished(conflict.operationId, 'failed'); assert.match(failed.error, /已由其他环境占用/)
    await verifyLAN(main, '100'); await destroy(conflict)
  })
  await step('提交失败回放原网络，Linux 桥原配置保持', async () => {
    const before = await detail(main), changed = structuredClone(before.spec)
    changed.networks[0].external.interface = 'nlt-linux'
    trigger = `nlt_external_${randomUUID().replaceAll('-', '')}`
    await sql(`CREATE FUNCTION ${trigger}() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='${main.id}' AND NEW.revision<>OLD.revision THEN RAISE EXCEPTION 'external acceptance commit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER ${trigger} BEFORE UPDATE OF applied_spec ON environments FOR EACH ROW EXECUTE FUNCTION ${trigger}();`)
    const failed = await change(main, changed, 'failed'); assert.equal(failed.phase, 'rolled-back', failed.error)
    await sql(`DROP TRIGGER ${trigger} ON environments; DROP FUNCTION ${trigger}();`); trigger = undefined
    assert.deepEqual((await detail(main)).spec, before.spec); assert.equal((await detail(main)).revision, before.revision)
    await verifyLAN(main, '100'); assert.deepEqual(await snapshot('nlt-linux'), report.originalLinuxBridge)
  })
  await step('切换 Linux 桥、OVS 桥和无标签 LAN，资产身份与宿主地址保持', async () => {
    const instances = (await state(main)).assets.map(item => item.instanceId).sort()
    for (const source of ['nlt-linux', 'nlt-ovs']) {
      const current = await detail(main), changed = structuredClone(current.spec)
      changed.networks[0].external.interface = source
      await change(main, changed); await verifyLAN(main, '100')
      assert.deepEqual((await state(main)).assets.map(item => item.instanceId).sort(), instances)
      assert.deepEqual(await snapshot('nlt-linux'), report.originalLinuxBridge); assert.deepEqual(await snapshot('nlt-ovs'), report.originalOVSBridge)
    }
    const current = await detail(main), changed = structuredClone(current.spec)
    delete changed.networks[0].external.vlan
    await change(main, changed); await verifyLAN(main, 'native')
    assert.deepEqual((await state(main)).assets.map(item => item.instanceId).sort(), instances)
  })
  await step('不同 VLAN 重叠地址隔离，IPv6 外部通信', async () => {
    const network = { id: randomUUID(), name: 'LAN 200', cidr: '192.0.2.0/24', gateway: '192.0.2.1', allocationPool: '192.0.2.128/26', external: { nodeId: primary, interface: 'nlt-uplink', vlan: 200 } }
    other = await create('VLAN 200 验收', [network], [container, container]); await finished(other.operationId)
    await verifyLAN(main, 'native'); await verifyLAN(other, '200')
    const current = await detail(other), changed = structuredClone(current.spec)
    Object.assign(changed.networks[0], { cidr: 'fd11::/64', allocationPool: 'fd11::100/120', gateway: 'fd11::1' })
    for (const asset of changed.assets) asset.interfaces[0].address = ''
    await change(other, changed); await verifyLAN(other, '200', true)
    await node(primary, 'systemctl', 'restart', 'netlab-node-dev.service')
    const deadline = Date.now() + 15000
    while (Date.now() < deadline && !(await api('/nodes')).find(item => item.id === primary && item.state === 'ready')) await delay(200)
    await verifyLAN(main, 'native'); await verifyLAN(other, '200', true)
  })
  await step('销毁全部环境，租约、数据面与文件无残留', async () => {
    await node(primary, 'ovn-nbctl', 'lr-del', `lr_${main.id.replaceAll('-', '')}_gateway`)
    for (const env of envs) await destroy(env)
    const ids = envs.map(item => `'${item.id}'`).join(',')
    assert.equal(await sql(['external_network_leases', 'runtime_assets', 'service_ports'].map(table => `select count(*) from ${table} where environment_id in (${ids});`).join(' ')), '0\n0\n0')
    for (const id of workers.keys()) for (const env of envs) await node(id, 'test', '!', '-e', `/var/lib/netlab-dev/external/${env.id}.json`)
    assert.ok(!(await node(primary, 'ovs-vsctl', 'get', 'Open_vSwitch', '.', 'external_ids:ovn-bridge-mappings')).includes('netlab-external-'))
    assert.deepEqual(await snapshot('nlt-linux'), report.originalLinuxBridge); assert.deepEqual(await snapshot('nlt-ovs'), report.originalOVSBridge)
    assert.deepEqual(await snapshot('nlt-uplink'), report.originalUplink)
    const nodes = await api('/nodes')
    for (const before of baseline) assert.deepEqual(nodes.find(item => item.id === before.id).reserved, before.reserved)
    for (const table of ['Logical_Switch', 'Logical_Switch_Port', 'Logical_Router', 'Logical_Router_Port']) {
      const output = await node(primary, 'ovn-nbctl', '--columns=external_ids', 'list', table)
      for (const env of envs) assert.ok(!output.includes(env.id), `${table} 残留`)
    }
  })
  report.passed = true
} catch (error) { report.passed = false; report.error = error.message; process.exitCode = 1 } finally {
  report.cleanupErrors = []
  if (trigger) try { await sql(`DROP TRIGGER ${trigger} ON environments; DROP FUNCTION ${trigger}();`) } catch (error) { report.cleanupErrors.push(error.message) }
  for (const env of envs) try { await destroy(env) } catch (error) { report.cleanupErrors.push(error.message) }
  if (fixture && !report.cleanupErrors.length) try { await node(primary, 'bash', '/mnt/d/newgz/netlab/tests/fixtures/external-lan.sh', 'stop') } catch (error) { report.cleanupErrors.push(error.message) }
  key?.remove(); report.finishedAt = new Date().toISOString()
  await writeFile('data/external-network-result.json', JSON.stringify(report, null, 2))
  console.log(JSON.stringify(report))
}
