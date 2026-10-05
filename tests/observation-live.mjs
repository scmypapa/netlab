import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { execFile } from 'node:child_process'
import { readFile, writeFile } from 'node:fs/promises'
import { promisify } from 'node:util'
import { delay, guestKey, guestSSH } from './guest-ssh.mjs'

const execute = promisify(execFile)
const base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const report = { startedAt: new Date().toISOString(), steps: [], cleanupErrors: [] }
const concurrency = Number(process.env.NETLAB_OBSERVATION_CONCURRENCY || 64)
const duration = Number(process.env.NETLAB_OBSERVATION_SECONDS || 60)
report.workload = { concurrencyPerNode: concurrency, durationSeconds: duration, responseBytes: 2 * 1024 * 1024 }
const workers = new Map(JSON.parse(await readFile('D:/.cache/netlab/artifacts/multi-node-workers.json', 'utf8')).map(worker => [worker.nodeId, worker]))
const pools = []
const captures = []
let cookie, environment, current, key
const quote = value => `'${String(value).replaceAll("'", "'\"'\"'")}'`

async function api(path, method = 'GET', body) {
  const response = await fetch(base + '/api/v1' + path, { method, headers: { Cookie: cookie || '', 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(30_000) })
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie')?.split(';')[0]
  const text = await response.text()
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${text}`)
  return text ? JSON.parse(text) : undefined
}
async function step(name, action) {
  const result = { name, passed: false }, started = performance.now()
  try { await action(); result.passed = true } catch (error) { result.error = error.message; throw error } finally {
    result.durationMs = Math.round(performance.now() - started)
    report.steps.push(result)
    console.log(JSON.stringify(result))
    await writeFile('data/observation-live-result.json', JSON.stringify(report, null, 2))
  }
}
async function complete(id) {
  for (const deadline = Date.now() + 180_000; Date.now() < deadline;) {
    const operation = await api('/operations/' + id)
    if (['succeeded', 'failed', 'partially_applied'].includes(operation.state)) {
      assert.equal(operation.state, 'succeeded', `${operation.phase}: ${operation.error}`)
      return operation
    }
    await delay(250)
  }
  throw new Error(`operation timed out: ${id}`)
}
async function node(id, args, timeout = 60_000) {
  const worker = workers.get(id)
  assert.ok(worker, 'missing node connection')
  const command = worker.host ? ['ssh', '-i', worker.keyPath, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${worker.keyPath}.hosts`, `root@${worker.host}`, args.map(quote).join(' ')] : args
  return (await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', ...command], { encoding: 'utf8', timeout, maxBuffer: 4 << 20 })).stdout
}
async function namespace(actual, args, timeout) {
  const tasks = await node(actual.nodeId, ['ctr', '-n', 'netlab', 'tasks', 'list'])
  const pid = tasks.split('\n').find(line => line.trim().split(/\s+/)[0] === actual.instanceId)?.trim().split(/\s+/)[1]
  assert.match(pid || '', /^\d+$/)
  return node(actual.nodeId, ['nsenter', `--net=/proc/${pid}/ns/net`, ...args], timeout)
}

try {
  await api('/sessions/login', 'POST', JSON.parse(await readFile('data/dev-login.json', 'utf8')))
  await step('两节点、六个混合资产，通过产品 API 创建并核对真实放置', async () => {
    const nodes = (await api('/nodes')).filter(node => node.state === 'ready')
    assert.equal(nodes.length, 2)
    const templates = await api('/templates?limit=100')
    const container = templates.find(item => item.kind === 'container' && item.state === 'ready' && item.name === 'API container')
    const vm = templates.find(item => item.kind === 'vm' && item.state === 'ready' && item.name === 'Ubuntu 24.04 guest verification' && item.initialization === 'cloud-init')
    assert.ok(container && vm, 'missing prepared Linux and container templates')
    key = guestKey()
    const network = { id: randomUUID(), name: 'Observation LAN', cidr: '10.97.0.0/24', mtu: 1400 }
    const assets = []
    for (const n of nodes) {
      const directory = `/var/lib/netlab-dev/test-tmp/observation-${randomUUID()}`
      await node(n.id, ['mkdir', '-p', directory])
      const pool = await api('/storage-pools', 'POST', { nodeIds: [n.id], driver: 'directory', name: 'Observation verification', directory })
      pools.push(pool)
      for (const [index, template] of [container, container, vm].entries()) assets.push({ id: randomUUID(), name: `${n.name}-${index}`, templateId: template.id, storagePoolId: pool.id, resources: template.resources, interfaces: [{ id: randomUUID(), networkId: network.id, mac: '', address: '', primary: true }], ...(template.kind === 'vm' ? { guest: { username: 'netlab', sshAuthorizedKeys: [key.publicKey] } } : {}) })
    }
    environment = await api('/environments', 'POST', { name: 'Native observation verification', run: true, spec: { networks: [network], assets }, clientRequestId: randomUUID() })
    report.environmentId = environment.id
    await complete(environment.operationId)
    environment = await api(`/environments/${environment.id}`)
    current = (await api(`/environments/${environment.id}/state`)).assets
    for (const asset of environment.spec.assets) assert.equal(current.find(item => item.assetId === asset.id).nodeId, pools.find(pool => pool.id === asset.storagePoolId).nodeIds[0])
    report.assets = current.map(({ assetId, nodeId, instanceId }) => ({ assetId, nodeId, instanceId }))
    const server = "import os,http.server\nif os.fork()==0:\n os.setsid()\n fd=os.open('/dev/null',os.O_RDWR)\n for stream in range(3): os.dup2(fd,stream)\n body=b'x'*(2*1024*1024)\n class Handler(http.server.BaseHTTPRequestHandler):\n  def do_GET(self):\n   self.send_response(200);self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body)\n  def log_message(self,*args): pass\n class Server(http.server.ThreadingHTTPServer):\n  request_queue_size=512\n  daemon_threads=True\n Server(('0.0.0.0',8088),Handler).serve_forever()"
    const localClient = current.find(actual => !workers.get(actual.nodeId).host && environment.spec.assets.find(asset => asset.id === actual.assetId).templateId === container.id)
    for (const actual of current) {
      const asset = environment.spec.assets.find(item => item.id === actual.assetId)
      if (asset.templateId === container.id) {
        await node(actual.nodeId, ['ctr', '-n', 'netlab', 'tasks', 'exec', '--exec-id', randomUUID(), actual.instanceId, 'sh', '-c', 'dd if=/dev/zero of=/usr/share/nginx/html/large.bin bs=1048576 count=2 2>/dev/null'])
      } else {
        const address = environment.spec.assets.find(asset => asset.id === actual.assetId).interfaces[0].address
        const ssh = await guestSSH(localClient.instanceId, actual.instanceId, () => address, key)
        ssh.run('/usr/bin/python3', '-c', quote(server))
      }
    }
    const containerIds = new Set(environment.spec.assets.filter(asset => asset.templateId === container.id).map(asset => asset.id))
    report.workloads = current.map(actual => ({ ...actual, address: environment.spec.assets.find(asset => asset.id === actual.assetId).interfaces[0].address, container: containerIds.has(actual.assetId) }))
  })
  await step('真实混合资产采集进入 VictoriaMetrics，再通过历史 API 读取', async () => {
    for (const deadline = Date.now() + 45_000;;) {
      const history = await api(`/environments/${environment.id}/metrics?range=120&step=10`)
      if (current.every(asset => ['cpu', 'memory'].every(metric => history.series.some(series => series.assetId === asset.assetId && series.metric === metric && series.points.length >= 2)))) {
        report.metricSeries = history.series.map(({ metric, assetId, nodeId, points }) => ({ metric, assetId, nodeId, samples: points.length, max: Math.max(...points.map(point => point.value)) }))
        break
      }
      assert.ok(Date.now() < deadline, 'native metrics did not reach history API: ' + JSON.stringify(history.series.map(series => ({ asset: series.assetId, metric: series.metric, points: series.points.length }))))
      await delay(1000)
    }
  })
  await step('双节点 VM/容器持续大包通信，同时运行采样、抓包与资源历史', async () => {
    const capture = await api(`/environments/${environment.id}/captures`, 'POST', { assetIds: current.map(asset => asset.assetId), durationSeconds: Math.min(1800, duration + 30), fileSizeMiB: 256 })
    captures.push(...capture.segments)
    assert.deepEqual(capture.errors, {})
    assert.equal(capture.segments.length, 2)
    const source = await readFile(new URL('./fixtures/observation-load.py', import.meta.url), 'utf8')
    const clients = report.workloads.filter(asset => asset.container).filter((asset, index, list) => list.findIndex(item => item.nodeId === asset.nodeId) === index)
    const sampled = []
    const load = Promise.all(clients.map(async client => {
      const targets = report.workloads.filter(target => target.nodeId !== client.nodeId).map(target => `http://${target.address}:${target.container ? 80 : 8088}/${target.container ? 'large.bin' : ''}`)
      return { nodeId: client.nodeId, ...JSON.parse(await namespace(client, ['python3', '-c', source, JSON.stringify(targets), String(concurrency), String(duration)], (duration + 30) * 1000)) }
    }))
    for (let i = 0; i < Math.floor(duration / 4); i++) { await delay(4000); sampled.push(await api(`/environments/${environment.id}/traffic`)) }
    report.load = await load
    report.traffic = sampled
    assert.ok(sampled.some(sample => sample.flows.some(flow => flow.sourceAssetId && flow.destinationAssetId)), 'asset relationships were not sampled')
    report.captureResults = []
    for (const capture of captures) {
      const path = `/environments/${environment.id}/captures/${capture.nodeId}/${capture.id}`
      await api(path, 'POST')
      const detail = await api(path)
      assert.notEqual(detail.segment.status, 'failed', detail.segment.error)
      assert.ok(detail.segment.packets > 0)
      assert.equal(typeof detail.segment.kernelDroppedPackets, 'number', 'capture interface statistics missing')
      report.captureResults.push(detail)
    }
    const history = await api(`/environments/${environment.id}/metrics?range=180&step=10`)
    for (const asset of current) assert.ok(history.series.some(series => series.assetId === asset.assetId && ['receive', 'transmit'].includes(series.metric) && series.points.some(point => point.value > 0)), 'missing real interface rates for ' + asset.assetId)
    report.metricSeriesAfterLoad = history.series
    for (const result of report.load) { assert.equal(result.failures, 0, JSON.stringify(result.errors)); assert.ok(result.bytes > 0) }
  })
} catch (error) {
  report.error = error.message
  process.exitCode = 1
} finally {
  if (environment) {
    try { await complete((await api(`/environments/${environment.id}/actions`, 'POST', { action: 'destroy', clientRequestId: randomUUID() })).id); assert.equal((await api(`/environments/${environment.id}/state`)).assets.length, 0) } catch (error) { report.cleanupErrors.push(error.message) }
  }
  for (const pool of pools) {
    try { await complete((await api(`/storage-pools/${pool.id}`, 'DELETE')).id); await node(pool.nodeIds[0], ['rmdir', `${pool.directory}/netlab-${pool.nodeIds[0]}`, pool.directory]) } catch (error) { report.cleanupErrors.push(error.message) }
  }
  key?.remove()
  report.finishedAt = new Date().toISOString()
  report.passed = !report.error && report.cleanupErrors.length === 0
  if (!report.passed) process.exitCode = 1
  await writeFile('data/observation-live-result.json', JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ passed: report.passed, error: report.error, cleanupErrors: report.cleanupErrors }))
}
