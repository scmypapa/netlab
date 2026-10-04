import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { execFile } from 'node:child_process'
import { readFile, writeFile } from 'node:fs/promises'
import { promisify } from 'node:util'
import { delay, guestKey, wsl } from './guest-ssh.mjs'

const execute = promisify(execFile)
const base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const workers = new Map(JSON.parse(await readFile('D:/.cache/netlab/artifacts/multi-node-workers.json', 'utf8')).map(worker => [worker.nodeId, worker]))
const report = { startedAt: new Date().toISOString(), steps: [], cleanupErrors: [] }
const quote = value => `'${String(value).replaceAll("'", "'\"'\"'")}'`
let cookie, pool, environment, point, key, privateKey, assets, initial, repository, backup, destroyed = false
const backupRoot = '/mnt/d/.cache/netlab/ceph-backup-' + randomUUID()

async function raw(path, method = 'GET', body, type = 'application/json') {
  return fetch(base + '/api/v1' + path, { method, headers: { Cookie: cookie || '', 'Content-Type': type }, body: body === undefined ? undefined : type === 'application/json' ? JSON.stringify(body) : body, signal: AbortSignal.timeout(180_000) })
}
async function api(path, method = 'GET', body) {
  const response = await raw(path, method, body)
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie')?.split(';')[0]
  const text = await response.text()
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${text}`)
  return text ? JSON.parse(text) : undefined
}
async function complete(id) {
  for (const deadline = Date.now() + 900_000; Date.now() < deadline;) {
    const operation = await api('/operations/' + id)
    if (['succeeded', 'failed', 'partially_applied'].includes(operation.state)) { assert.equal(operation.state, 'succeeded', `${operation.phase}: ${operation.error}`); return }
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
  try {
    return (await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', ...command], { encoding: 'utf8', timeout: 60_000 })).stdout
  } catch (error) {
    throw new Error([error.message, error.stdout, error.stderr].filter(Boolean).join('\n'), { cause: error })
  }
}
async function action(kind) { await complete((await api(`/environments/${environment.id}/actions`, 'POST', { action: kind })).id); destroyed = kind === 'destroy' }
async function connect(asset) {
  const path = `/environments/${environment.id}/assets/${asset.id}`
  let peer, error
  for (const deadline = Date.now() + 180_000; Date.now() < deadline;) {
    const response = await raw(path + '/ssh/host-key', 'POST', { port: 22 })
    if (response.ok) { peer = await response.json(); break }
    error = await response.text(); await delay(1000)
  }
  assert.ok(peer, 'SSH did not become ready: ' + error)
  await api(path + '/ssh', 'PUT', { username: 'netlab', port: 22, authKind: 'key', hostKey: peer.fingerprint, privateKey })
}
async function proof(asset, value) {
  const path = `/environments/${environment.id}/assets/${asset.id}/files/content?path=${encodeURIComponent('/home/netlab/ceph-proof.txt')}`
  const response = await raw(path, value === undefined ? 'GET' : 'PUT', value, 'application/octet-stream')
  const text = await response.text()
  assert.ok(response.ok, 'file operation: ' + response.status + ' ' + text)
  return text
}
async function nativeImages() {
  const first = pool.nodeIds[0]
  return JSON.parse(await node(first, ['rbd', '--conf', pool.storage.path + '/ceph.conf', '--id', 'netlab', '--pool', 'netlab', 'ls', '--format', 'json'])).filter(name => name.startsWith(pool.storage.rbd.imagePrefix))
}

try {
  await api('/sessions/login', 'POST', JSON.parse(await readFile('data/dev-login.json', 'utf8')))
  await step('两个 Worker 接入同一个真实 Ceph 池，容量及原生快照能力贯通', async () => {
    const nodes = (await api('/nodes')).filter(item => item.state === 'ready' && item.capabilities.includes('vm'))
    assert.equal(nodes.length, 2)
    const cephKey = (await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', 'ceph-authtool', '/var/lib/netlab-dev/ceph-test/client.keyring', '-n', 'client.netlab', '--print-key'], { encoding: 'utf8' })).stdout.trim()
    pool = await api('/storage-pools', 'POST', { name: 'Ceph lifecycle verification', nodeIds: nodes.map(item => item.id), driver: 'rbd', ceph: { monitors: ['192.168.122.1:16789'], pool: 'netlab', user: 'netlab', key: cephKey } })
    assert.equal(pool.nodeIds.length, 2)
    assert.equal(pool.storage.nativeSnapshots, true)
    assert.ok(pool.storage.availableBytes > 25 * 2 ** 30)
    assert.equal((await api('/storage-pools')).find(item => item.id === pool.id).allocatedGiB, 0)
    report.capacityBytes = pool.storage.capacityBytes
  })
  await step('真实 RBD 运行及停机容量、原生恢复和便携导出定向验证', async () => {
    const primary = pool.nodeIds.find(id => !workers.get(id).host)
    assert.ok(primary)
    await node(primary, ['env', 'PATH=/usr/local/go/bin:/usr/bin:/bin', 'GOCACHE=/mnt/d/.cache/netlab/go-build-linux', 'GOMODCACHE=/mnt/d/.cache/netlab/go-mod', 'GOTMPDIR=/mnt/d/.cache/netlab/go-temp', 'NETLAB_REAL_RBD_ROOT=' + pool.storage.path, 'go', '-C', '/mnt/d/newgz/netlab', 'test', './internal/engine', '-run', '^TestRealRBDRecovery$', '-count=1'])
  })
  await step('自动调度 5 台真实 Linux VM，系统盘及数据盘直接使用 RBD', async () => {
    const template = (await api('/templates?limit=100')).find(item => item.name === 'Ubuntu 24.04 guest verification' && item.state === 'ready' && item.resources.diskGiB === 4 && item.initialization === 'cloud-init')
    assert.ok(template, 'missing prepared Ubuntu template')
    key = guestKey(); privateKey = wsl('cat', key.path)
    const network = { id: randomUUID(), name: 'Ceph LAN', cidr: '10.99.0.0/24' }
    assets = Array.from({ length: 5 }, (_, i) => ({ id: randomUUID(), name: `Ceph Linux ${i + 1}`, templateId: template.id, storagePoolId: pool.id, resources: template.resources, guest: { username: 'netlab', hostname: 'ceph-' + (i + 1), sshAuthorizedKeys: [key.publicKey] }, interfaces: [{ id: randomUUID(), networkId: network.id, mac: '', address: '', primary: true }], volumes: [{ id: 'data', mountPath: '/data', sizeGiB: 1 }] }))
    environment = await api('/environments', 'POST', { name: 'Ceph native lifecycle', run: true, spec: { networks: [network], assets } })
    await complete(environment.operationId)
    initial = (await api(`/environments/${environment.id}/state`)).assets
    report.placement = initial.map(item => ({ assetId: item.assetId, nodeId: item.nodeId, instanceId: item.instanceId }))
    assert.equal(new Set(initial.map(item => item.nodeId)).size, 2)
    assert.equal((await nativeImages()).length, 10)
    assert.equal((await api('/storage-pools')).find(item => item.id === pool.id).allocatedGiB, 25)
    for (const item of initial) {
      const xml = await node(item.nodeId, ['virsh', 'dumpxml', item.instanceId])
      assert.equal((xml.match(/protocol='rbd'/g) || []).length, 2)
    }
  })
  await step('每台 VM 完成真实 SSH 与文件写入、读取，再正常关机', async () => {
    for (const asset of assets) { await connect(asset); await proof(asset, asset.id); assert.equal(await proof(asset), asset.id) }
    await action('stop')
  })
  await step('捕获 10 个原生磁盘快照并核对实际数据量', async () => {
    environment = await api(`/environments/${environment.id}`)
    point = await api(`/environments/${environment.id}/recovery-points`, 'POST', { name: 'Native RBD point', expectedRevision: environment.revision })
    await complete(point.operationId)
    report.point = (await api(`/environments/${environment.id}/recovery-points`)).find(item => item.id === point.id)
    assert.ok(report.point.sizeBytes > 0)
  })
  await step('原生恢复点导出便携备份，跨节点收集真实磁盘与模板', async () => {
    const primary = pool.nodeIds.find(id => !workers.get(id).host)
    assert.ok(primary)
    repository = await api('/backup-repositories', 'POST', { name: 'Ceph portable verification', nodeId: primary, location: backupRoot, initialize: true })
    await complete(repository.operationId)
    backup = await api(`/environments/${environment.id}/backups`, 'POST', { name: 'Portable Ceph point', repositoryId: repository.id, recoveryPointId: point.id })
    await complete(backup.operationId)
    report.backup = (await api(`/environments/${environment.id}/backups`)).find(item => item.id === backup.id)
    assert.equal(report.backup.state, 'ready')
    assert.ok(report.backup.sizeBytes > 0)
  })
  await step('修改文件并销毁源 VM，恢复点保持原生磁盘引用', async () => {
    await action('start')
    for (const asset of assets) { await connect(asset); await proof(asset, 'changed') }
    await action('destroy')
    assert.equal((await api(`/environments/${environment.id}/state`)).assets.length, 0)
    assert.equal((await nativeImages()).length, 10, 'snapshots retain their source disks')
  })
  await step('从原生恢复点重新运行环境，身份及每台 VM 文件内容保持', async () => {
    environment = await api(`/environments/${environment.id}`)
    await complete((await api(`/environments/${environment.id}/recovery-points/${point.id}/restore`, 'POST', { expectedRevision: environment.revision })).id)
    destroyed = false
    await action('start')
    const restored = (await api(`/environments/${environment.id}/state`)).assets
    assert.deepEqual(restored.map(item => item.instanceId).sort(), initial.map(item => item.instanceId).sort())
    for (const asset of assets) { await connect(asset); assert.equal(await proof(asset), asset.id) }
    await complete((await api(`/environments/${environment.id}/recovery-points/${point.id}`, 'DELETE')).id)
    point = undefined
    assert.equal((await nativeImages()).length, 10, 'restore owns independent disks')
  })
  await step('使用便携备份恢复同一个环境，磁盘、文件和实例身份保持', async () => {
    await action('destroy')
    environment = await api(`/environments/${environment.id}`)
    await complete((await api(`/environments/${environment.id}/backups/${backup.id}/restore`, 'POST', { expectedRevision: environment.revision })).id)
    destroyed = false
    await action('start')
    for (const asset of assets) { await connect(asset); assert.equal(await proof(asset), asset.id) }
    assert.deepEqual((await api(`/environments/${environment.id}/state`)).assets.map(item => item.instanceId).sort(), initial.map(item => item.instanceId).sort())
  })
} catch (error) { report.error = error.message; process.exitCode = 1 }
finally {
  if (environment && !destroyed) try { await action('destroy') } catch (error) { report.cleanupErrors.push(error.message) }
  if (point) try { await complete((await api(`/environments/${environment.id}/recovery-points/${point.id}`, 'DELETE')).id) } catch (error) { report.cleanupErrors.push(error.message) }
  if (backup) try { await complete((await api(`/environments/${environment.id}/backups/${backup.id}`, 'DELETE')).id) } catch (error) { report.cleanupErrors.push(error.message) }
  if (repository) try { await api(`/backup-repositories/${repository.id}`, 'DELETE'); await node(repository.nodeId, ['rm', '-rf', '--', backupRoot]) } catch (error) { report.cleanupErrors.push(error.message) }
  if (pool) try { assert.equal((await nativeImages()).length, 0); await complete((await api(`/storage-pools/${pool.id}`, 'DELETE')).id); assert.ok(!(await api('/storage-pools')).some(item => item.id === pool.id)) } catch (error) { report.cleanupErrors.push(error.message) }
  if (initial && destroyed) try {
    assert.equal((await api(`/environments/${environment.id}/state`)).assets.length, 0)
    for (const [id] of workers) {
      const domains = await node(id, ['virsh', 'list', '--all', '--uuid'])
      for (const asset of initial.filter(item => item.nodeId === id)) assert.ok(!domains.includes(asset.instanceId))
      assert.ok(!(await node(id, ['ovs-vsctl', '--format=json', '--columns=external_ids', 'list', 'Interface'])).includes(environment.id))
    }
    const primary = [...workers].find(([, worker]) => !worker.host)[0]
    for (const table of ['Logical_Switch', 'Logical_Router', 'Logical_Switch_Port', 'Logical_Router_Port']) assert.ok(!(await node(primary, ['ovn-nbctl', '--format=json', '--columns=external_ids', 'list', table])).includes(environment.id))
  } catch (error) { report.cleanupErrors.push(error.message) }
  if (key) try { key.remove() } catch (error) { report.cleanupErrors.push(error.message) }
  report.finishedAt = new Date().toISOString(); report.passed = !report.error && !report.cleanupErrors.length
  if (!report.passed) process.exitCode = 1
  await writeFile('data/ceph-live-result.json', JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ passed: report.passed, error: report.error, cleanupErrors: report.cleanupErrors }))
}
