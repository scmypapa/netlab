import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { execFile, execFileSync } from 'node:child_process'
import { readFile, writeFile } from 'node:fs/promises'
import { promisify } from 'node:util'
import { delay, guestKey, wsl } from './guest-ssh.mjs'

const execute = promisify(execFile)
const base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const workers = new Map(JSON.parse(await readFile('D:/.cache/netlab/artifacts/multi-node-workers.json', 'utf8')).map(worker => [worker.nodeId, worker]))
const report = { startedAt: new Date().toISOString(), steps: [], cleanupErrors: [] }
const local = process.env.NETLAB_TEST_MIGRATION_STORAGE === 'local'
report.storage = local ? 'local' : 'rbd'
const quote = value => `'${String(value).replaceAll("'", "'\"'\"'")}'`
let cookie, pool, environment, key, privateKey, asset, initial, bootId, identity, endpoint, destroyed = false

async function api(path, method = 'GET', body, binary = false) {
  const response = await fetch(base + '/api/v1' + path, { method, headers: { Cookie: cookie || '', 'Content-Type': binary ? 'application/octet-stream' : 'application/json' }, body: body === undefined ? undefined : binary ? body : JSON.stringify(body), signal: AbortSignal.timeout(180_000) })
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie')?.split(';')[0]
  const text = await response.text()
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${text}`)
  return binary ? text : text ? JSON.parse(text) : undefined
}
async function complete(id) {
  for (const deadline = Date.now() + 600_000; Date.now() < deadline;) {
    const operation = await api('/operations/' + id)
    if (['succeeded', 'failed', 'partially_applied'].includes(operation.state)) {
      assert.equal(operation.state, 'succeeded', `${operation.phase}: ${operation.error}`)
      return operation
    }
    await delay(250)
  }
  throw new Error('operation timed out: ' + id)
}
async function step(name, action) {
  const item = { name, passed: false }, start = performance.now()
  try { await action(); item.passed = true } catch (error) { item.error = error.message; throw error } finally { item.durationMs = Math.round(performance.now() - start); report.steps.push(item); console.log(JSON.stringify(item)) }
}
async function node(id, args) {
  const worker = workers.get(id)
  const command = worker.host ? ['ssh', '-i', worker.keyPath, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${worker.keyPath}.hosts`, `root@${worker.host}`, args.map(quote).join(' ')] : args
  try { return (await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', ...command], { encoding: 'utf8', timeout: 60_000 })).stdout }
  catch (error) { throw new Error([error.message, error.stdout, error.stderr].filter(Boolean).join('\n'), { cause: error }) }
}
async function state() { return (await api(`/environments/${environment.id}/state`)).assets.find(item => item.assetId === asset.id) }
async function action(kind) { await complete((await api(`/environments/${environment.id}/actions`, 'POST', { action: kind })).id); destroyed = kind === 'destroy' }
async function connect() {
  const path = `/environments/${environment.id}/assets/${asset.id}`
  let peer, last
  for (const deadline = Date.now() + 180_000; Date.now() < deadline;) {
    try { peer = await api(path + '/ssh/host-key', 'POST', { port: 22 }); break }
    catch (error) { last = error; await delay(1000) }
  }
  assert.ok(peer, 'guest SSH not ready: ' + last?.message)
  await api(path + '/ssh', 'PUT', { username: 'netlab', port: 22, authKind: 'key', hostKey: peer.fingerprint, privateKey })
}
async function file(path, contents) {
  return api(`/environments/${environment.id}/assets/${asset.id}/files/content?path=${encodeURIComponent(path)}`, contents === undefined ? 'GET' : 'PUT', contents, true)
}
async function guestBootId() {
  return wsl('ssh', '-i', key.path, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${key.path}.hosts`, '-o', `HostKeyAlias=${initial.instanceId}`, '-p', String(endpoint.port), `netlab@${endpoint.address}`, 'cat', '/proc/sys/kernel/random/boot_id').trim()
}
async function serviceBanner() {
  const value = wsl('python3', '-c', 'import socket,sys; s=socket.create_connection((sys.argv[1],int(sys.argv[2])),5); print(s.recv(1024).decode()); s.close()', endpoint.address, String(endpoint.port))
  assert.match(value, /^SSH-2\.0-/)
}
async function images() {
  return JSON.parse(await node(pool.nodeIds[0], ['rbd', '--conf', pool.storage.path + '/ceph.conf', '--id', 'netlab', '--pool', 'netlab', 'ls', '--format', 'json'])).filter(name => name.startsWith(pool.storage.rbd.imagePrefix))
}
async function migrate(targetNodeId) {
  const env = await api(`/environments/${environment.id}`)
  const destinations = await api(`/environments/${environment.id}/assets/${asset.id}/migrations`)
  assert.equal(destinations.length, 1)
  assert.equal(destinations[0].id, targetNodeId)
  const op = await api(`/environments/${environment.id}/assets/${asset.id}/migrations`, 'POST', { expectedRevision: env.revision, targetNodeId, clientRequestId: randomUUID() })
  await complete(op.id)
  const current = await state()
  assert.equal(current.nodeId, targetNodeId)
  assert.equal(current.instanceId, initial.instanceId)
  assert.equal((await api(`/environments/${environment.id}`)).revision, env.revision + (local ? 1 : 0))
  const spec = (await api(`/environments/${environment.id}`)).spec
  assert.deepEqual(spec.assets[0].interfaces, identity)
  const xml = await node(current.nodeId, ['virsh', 'dumpxml', current.instanceId])
  assert.equal(xml.match(/<genid>(.*?)<\/genid>/)?.[1], report.genId)
  const oldNode = [...workers.keys()].find(id => id !== current.nodeId)
  assert.ok(!(await node(oldNode, ['virsh', 'list', '--all', '--uuid'])).includes(current.instanceId))
  report.moves ||= []
  report.moves.push({ operationId: op.id, from: oldNode, to: targetNodeId, instanceId: current.instanceId })
  if (pool) assert.equal((await images()).length, 1)
}

try {
  await api('/sessions/login', 'POST', JSON.parse(await readFile('data/dev-login.json', 'utf8')))
  await step((local ? '本地存储' : '共享池') + '创建真实 Ubuntu 来宾，SSH、文件和启动身份确认', async () => {
    const nodes = (await api('/nodes')).filter(item => workers.has(item.id) && item.state === 'ready')
    assert.equal(nodes.length, 2)
    if (!local) {
      const cephKey = wsl('ceph-authtool', '/var/lib/netlab-dev/ceph-test/client.keyring', '-n', 'client.netlab', '--print-key').trim()
      pool = await api('/storage-pools', 'POST', { name: 'Live migration verification', nodeIds: nodes.map(item => item.id), driver: 'rbd', ceph: { monitors: ['192.168.122.1:16789'], pool: 'netlab', user: 'netlab', key: cephKey } })
    }
    const template = (await api('/templates?limit=100')).find(item => item.name === 'Ubuntu 24.04 guest verification' && item.state === 'ready' && item.resources.diskGiB === 4 && item.initialization === 'cloud-init')
    assert.ok(template, 'missing prepared Ubuntu template')
    key = guestKey(); privateKey = wsl('cat', key.path)
    const network = { id: randomUUID(), name: 'Migration LAN', cidr: '10.99.4.0/24' }
    asset = { id: randomUUID(), name: 'Migration Linux', templateId: template.id, storagePoolId: local ? `default:${nodes[0].id}` : pool.id, resources: template.resources, guest: { username: 'netlab', hostname: 'migration-linux', sshAuthorizedKeys: [key.publicKey] }, interfaces: [{ id: randomUUID(), networkId: network.id, mac: '', address: '', primary: true }] }
    environment = await api('/environments', 'POST', { name: 'Shared VM migration', run: true, spec: { networks: [network], assets: [asset] } })
    await complete(environment.operationId)
    initial = await state()
    identity = (await api(`/environments/${environment.id}`)).spec.assets[0].interfaces
    report.genId = (await node(initial.nodeId, ['virsh', 'dumpxml', initial.instanceId])).match(/<genid>(.*?)<\/genid>/)?.[1]
    assert.ok(report.genId)
    await connect()
    await file('/home/netlab/migration-proof.txt', asset.id)
    assert.equal(await file('/home/netlab/migration-proof.txt'), asset.id)
    report.instanceId = initial.instanceId
    const env = await api(`/environments/${environment.id}`)
    await complete((await api(`/environments/${environment.id}/assets/${asset.id}/services`, 'POST', { expectedRevision: env.revision, protocol: 'tcp', targetPort: 22 })).id)
    endpoint = (await api(`/environments/${environment.id}/services`))[0]
    report.endpoint = endpoint
    console.log(JSON.stringify({ endpoint }))
    await serviceBanner()
    const scanned = wsl('ssh-keyscan', '-T', '3', '-p', String(endpoint.port), '-t', 'ed25519', endpoint.address)
    const known = scanned.split('\n').filter(line => line && !line.startsWith('#')).map(line => `${initial.instanceId} ${line.split(' ').slice(1).join(' ')}`).join('\n')
    assert.ok(known)
    execFileSync('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', 'tee', key.path + '.hosts'], { input: known + '\n', stdio: ['pipe', 'ignore', 'pipe'] })
    bootId = await guestBootId()
  })
  await step((local ? '停机迁移' : '在线迁移') + '保持地址、GenID与磁盘内容', async () => {
    await migrate([...workers.keys()].find(id => id !== initial.nodeId))
    await connect()
    const movedBoot = await guestBootId()
    if (local) assert.notEqual(movedBoot, bootId); else assert.equal(movedBoot, bootId)
    bootId = movedBoot
    assert.equal(await file('/home/netlab/migration-proof.txt'), asset.id)
    assert.deepEqual((await api(`/environments/${environment.id}/services`)).map(({ updatedAt, ...item }) => item), [endpoint].map(({ updatedAt, ...item }) => item))
    await serviceBanner()
    assert.equal((await state()).state, 'running')
  })
  await step('暂停来宾原生迁回、恢复，原会话凭据和文件继续使用', async () => {
    await action('suspend')
    await migrate(initial.nodeId)
    assert.equal((await state()).state, 'suspended')
    await action('resume')
    await connect()
    const movedBoot = await guestBootId()
    if (local) assert.notEqual(movedBoot, bootId); else assert.equal(movedBoot, bootId)
    assert.equal(await file('/home/netlab/migration-proof.txt'), asset.id)
    await serviceBanner()
  })
} catch (error) { report.error = error.message; process.exitCode = 1 }
finally {
  if (environment && !destroyed) try { await action('destroy') } catch (error) { report.cleanupErrors.push(error.message) }
  if (pool && destroyed) try { assert.equal((await images()).length, 0); await complete((await api(`/storage-pools/${pool.id}`, 'DELETE')).id) } catch (error) { report.cleanupErrors.push(error.message) }
  if (initial && destroyed) try {
    for (const [id] of workers) {
      assert.ok(!(await node(id, ['virsh', 'list', '--all', '--uuid'])).includes(initial.instanceId))
      assert.ok(!(await node(id, ['ovs-vsctl', '--format=json', '--columns=external_ids', 'list', 'Interface'])).includes(environment.id))
    }
    assert.equal((await api(`/environments/${environment.id}/state`)).assets.length, 0)
  } catch (error) { report.cleanupErrors.push(error.message) }
  key?.remove()
  report.finishedAt = new Date().toISOString(); report.passed = !report.error && !report.cleanupErrors.length
  if (!report.passed) process.exitCode = 1
  await writeFile(`data/migration-${local ? 'local' : 'live'}-result.json`, JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ passed: report.passed, error: report.error, cleanupErrors: report.cleanupErrors }))
}
