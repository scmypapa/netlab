import assert from 'node:assert/strict'
import {randomUUID} from 'node:crypto'
import {execFile} from 'node:child_process'
import {promisify} from 'node:util'
import {readFile, writeFile} from 'node:fs/promises'
import {delay} from './guest-ssh.mjs'

const execute = promisify(execFile), run = randomUUID(), base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090', secondController = process.env.NETLAB_TEST_SECONDARY_URL || 'http://127.0.0.1:8091'
const workers = new Map(JSON.parse(await readFile('D:/.cache/netlab/artifacts/multi-node-workers.json', 'utf8')).map(worker => [worker.nodeId, worker]))
const directory = `/var/lib/netlab-dev/test-tmp/lifecycle-${run}`
const registryAddress = '192.168.122.1:15051', registryInstance = `lifecycle-registry-${run}`
const report = {startedAt: new Date().toISOString(), steps: [], templates: [], environments: [], cleanupErrors: []}
let cookie, baseline, images = [], env, blueprint, stoppedNode, registryNode, placement = []
const quote = value => `'${String(value).replaceAll("'", "'\"'\"'")}'`
async function node(id, ...args) {
  const worker = workers.get(id)
  const command = worker.host ? ['ssh', '-i', worker.keyPath, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${worker.keyPath}.hosts`, `root@${worker.host}`, args.map(quote).join(' ')] : args
  return (await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', ...command], {timeout: 90000, maxBuffer: 2 ** 20})).stdout.trim()
}
async function api(path, method = 'GET', body, status = 200) {
  const response = await fetch(`${method === 'DELETE' ? secondController : base}/api/v1${path}`, {method, headers: {'Content-Type': 'application/json', Cookie: cookie || ''}, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(30000)})
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie')?.split(';')[0]
  const result = response.status === 204 ? undefined : await response.json()
  assert.equal(response.status, status, `${method} ${path}: ${JSON.stringify(result)}`)
  if (response.headers.has('Operation-Location')) result.operationId = response.headers.get('Operation-Location').split('/').at(-1)
  return result
}
async function operation(id, expected = 'succeeded') {
  const deadline = Date.now() + 90000
  while (Date.now() < deadline) {
    const result = await api(`/operations/${id}`)
    if (['succeeded', 'failed', 'partially_applied'].includes(result.state)) {
      assert.equal(result.state, expected, `${result.phase}: ${result.error}`)
      return result
    }
    await delay(200)
  }
  throw new Error(`operation timed out: ${id}`)
}
async function step(name, action) {
  const began = performance.now(), result = {name, passed: false}
  try {await action(); result.passed = true} catch (error) {result.error = error.message.replaceAll(cookie || '[session]', '[session]'); throw error}
  finally {result.durationMs = Math.round(performance.now() - began); report.steps.push(result); console.log(JSON.stringify(result))}
}
async function restarted(id, after) {
  const deadline = Date.now() + 45000
  while (Date.now() < deadline) {
    const value = (await api('/nodes')).find(node => node.id === id)
    if (value.state === 'ready' && Date.parse(value.observedAt) >= after) return
    await delay(200)
  }
  throw new Error('节点重启后没有产生新的在线观测')
}
async function destroy() {
  const current = await api(`/environments/${env.id}`)
  if (current.status !== 'destroyed') await operation((await api(`/environments/${env.id}/actions`, 'POST', {action: 'destroy', expectedRevision: current.revision}, 202)).id)
}
async function remove(image) {
  const [current] = await api(`/templates?ids=${image.id}`)
  if (!current) return
  const pending = current.state === 'deleting' ? await api(`/operations/${current.operationId}`) : await api(`/templates/${image.id}`, 'DELETE', undefined, 202)
  const op = pending.state === 'failed' ? await api(`/operations/${pending.id}/retry`, 'POST', undefined, 202) : pending
  await operation(op.id)
}
try {
  await api('/sessions/login', 'POST', JSON.parse(await readFile('data/dev-login.json', 'utf8')))
  baseline = await api('/nodes')
  const primary = [...workers].find(([, worker]) => !worker.host)[0]
  const secondary = [...workers].find(([, worker]) => worker.host)[0]
  await step('导入容器与真实 KVM 磁盘模板，来源文件保留在模板目录外', async () => {
    for (const [id, worker] of workers) {
      await node(id, 'mkdir', '-p', directory)
      if (worker.host) await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', 'scp', '-i', worker.keyPath, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${worker.keyPath}.hosts`, '/mnt/d/newgz/netlab/data/nginx.tar', `root@${worker.host}:${directory}/container.tar`], {timeout: 90000})
      else await node(id, 'cp', '/mnt/d/newgz/netlab/data/nginx.tar', `${directory}/container.tar`)
      await node(id, 'qemu-img', 'create', '-f', 'qcow2', `${directory}/system.qcow2`, '1G')
      await node(id, 'qemu-io', '-f', 'qcow2', '-c', 'write -P 0x59 0 4096', `${directory}/system.qcow2`)
    }
    for (const kind of ['container', 'vm']) {
      const image = await api('/templates', 'POST', {name: `生命周期 ${kind} ${run}`, kind, os: 'Linux', version: 1, source: `${directory}/${kind === 'vm' ? 'system.qcow2' : 'container.tar'}`, format: kind === 'vm' ? 'qcow2' : 'oci', resources: {cpu: 2, memoryMiB: 512, diskGiB: 1}, hardware: kind === 'vm' ? {machine: 'q35', firmware: 'bios', diskBus: 'virtio', nicModel: 'virtio'} : undefined}, 201)
      images.push(image); report.templates.push({id: image.id, kind})
      await operation(image.operationId)
      const [ready] = await api(`/templates?ids=${image.id}`)
      assert.equal(ready.state, 'ready', ready.error)
    }
  })
  await step('同一 Registry 镜像并发导入两个模板，保留各自引用并使用原生容器缓存', async () => {
    await node(primary, 'ctr', '-n', 'netlab', 'run', '-d', '--net-host', '--env', `REGISTRY_HTTP_ADDR=${registryAddress}`, 'docker.io/library/registry:2', registryInstance)
    registryNode = primary
    const [source] = await api(`/templates?ids=${images[0].id}`)
    const remote = `${registryAddress}/netlab/lifecycle:${run}`
    const pushName = `netlab/registry-fixture/${run}:1`
    await node(source.artifactNodeId, 'ctr', '-n', 'netlab', 'images', 'convert', '--platform', 'linux/amd64', '--oci', `netlab/template/${source.id}:1`, pushName)
    await node(source.artifactNodeId, 'ctr', '-n', 'netlab', 'images', 'push', '--plain-http', remote, pushName)
    await node(source.artifactNodeId, 'ctr', '-n', 'netlab', 'images', 'remove', pushName)
    const imported = await Promise.all([1, 2, 3].map(async index => {
      const archive = index === 3
      const image = await api('/templates', 'POST', {name: `生命周期 ${archive ? 'Archive' : 'Registry'} ${index} ${run}`, kind: 'container', os: 'Linux', version: 1, source: archive ? `${directory}/container.tar` : remote, format: 'oci', resources: images[0].resources, registry: archive ? undefined : {plainHttp: true}}, 201)
      images.push(image); report.templates.push({id: image.id, kind: image.kind})
      await operation(image.operationId)
      return image
    }))
    assert.equal(imported.length, 3)
    report.registrySource = remote
    for (const id of workers.keys()) assert.ok(!(await node(id, 'ctr', '-n', 'netlab', 'images', 'list', '-q')).split('\n').includes(remote))
  })
  await step('自动跨双节点启动 4 容器与 2 个 KVM 实例，保存环境模板并拒绝删除引用', async () => {
    const networkId = randomUUID()
    const assets = Array.from({length: 6}, (_, i) => {
      const image = images[i < 4 ? (i === 1 ? 2 : i === 2 ? 3 : i === 3 ? 4 : 0) : 1]
      return {id: randomUUID(), name: `Asset ${i + 1}`, templateId: image.id, resources: image.resources, interfaces: [{id: randomUUID(), networkId, mac: '', address: '', primary: true}]}
    })
    env = await api('/environments', 'POST', {name: `模板生命周期 ${run}`, run: true, spec: {networks: [{id: networkId, name: 'LAN', cidr: '10.83.0.0/24'}], assets}}, 201)
    report.environments.push(env.id)
    await operation(env.operationId)
    env = await api(`/environments/${env.id}`)
    placement = (await api(`/environments/${env.id}/state`)).assets
    assert.equal(new Set(placement.map(asset => asset.nodeId)).size, 2)
    assert.ok(placement.every(asset => asset.state === 'running'))
    report.placement = placement.map(({assetId, nodeId, instanceId}) => ({assetId, nodeId, instanceId}))
    blueprint = await api(`/environments/${env.id}/blueprints`, 'POST', {name: `生命周期环境模板 ${run}`, expectedRevision: env.revision, spec: env.spec}, 201)
    for (const image of images) assert.match((await api(`/templates/${image.id}`, 'DELETE', undefined, 409)).detail, /环境/)
    report.cachedNodes = []
    for (const image of images) {
      const [ready] = await api(`/templates?ids=${image.id}`)
      const required = new Set([ready.artifactNodeId, ...placement.filter(target => env.spec.assets.find(asset => asset.id === target.assetId).templateId === image.id).map(target => target.nodeId)])
      for (const id of required) {
        await node(id, 'test', '-f', `/var/lib/netlab-dev/artifacts/${image.id}/1/template.json`)
        report.cachedNodes.push({templateId: image.id, nodeId: id})
      }
    }
  })
  await step('销毁环境后仍保留环境模板引用；删除环境模板再释放资产模板', async () => {
    await destroy()
    for (const image of images) assert.match((await api(`/templates/${image.id}`, 'DELETE', undefined, 409)).detail, /环境模板/)
    await api(`/blueprints/${blueprint.id}`, 'DELETE', undefined, 204); blueprint = undefined
    const current = await api('/nodes')
    for (const previous of baseline) assert.deepEqual(current.find(node => node.id === previous.id).reserved, previous.reserved)
  })
  await step('节点离线时删除真实失败，保留模板与原任务；节点恢复后同任务重试成功', async () => {
    stoppedNode = secondary
    await node(secondary, 'systemctl', 'stop', 'netlab-node-dev.service')
    const pending = await api(`/templates/${images[0].id}`, 'DELETE', undefined, 202)
    const failed = await operation(pending.id, 'failed')
    assert.ok(failed.error)
    const [blocked] = await api(`/templates?ids=${images[0].id}`)
    assert.equal(blocked.state, 'deleting'); assert.ok(blocked.error)
    report.expectedNodeFailure = {operationId: failed.id, error: failed.error}
    const startedAt = Date.now()
    await node(secondary, 'systemctl', 'start', 'netlab-node-dev.service')
    await restarted(secondary, startedAt); stoppedNode = undefined
    const retried = await api(`/operations/${pending.id}/retry`, 'POST', undefined, 202)
    assert.equal(retried.id, pending.id)
    await operation(retried.id)
    for (const image of images.slice(1)) await remove(image)
  })
  await step('两个节点的模板、输入与镜像引用均清理，外部来源文件保留', async () => {
    for (const image of images) assert.deepEqual(await api(`/templates?ids=${image.id}`), [])
    for (const id of workers.keys()) {
      const refs = await node(id, 'ctr', '-n', 'netlab', 'images', 'list', '-q')
      for (const image of images) {
        await node(id, 'test', '!', '-e', `/var/lib/netlab-dev/artifacts/${image.id}/1`)
        await node(id, 'test', '!', '-e', `/var/lib/netlab-dev/imports/${image.id}/1`)
        assert.ok(!refs.includes(`netlab/template/${image.id}:1`))
      }
      await node(id, 'test', '-f', `${directory}/container.tar`)
      await node(id, 'test', '-f', `${directory}/system.qcow2`)
    }
    for (const target of placement) {
      await node(target.nodeId, 'test', '!', '-e', `/var/lib/netlab-dev/environments/${env.id}/instances/${target.instanceId}`)
      assert.ok(!(await node(target.nodeId, 'ctr', '-n', 'netlab', 'containers', 'list')).includes(target.instanceId))
      assert.ok(!(await node(target.nodeId, 'virsh', 'list', '--all', '--uuid')).includes(target.instanceId))
      assert.ok(!(await node(target.nodeId, 'ovs-vsctl', '--columns=external_ids', 'list', 'Interface')).includes(target.instanceId))
    }
    for (const table of ['Logical_Switch', 'Logical_Switch_Port', 'Logical_Router', 'Logical_Router_Port']) assert.ok(!(await node(primary, 'ovn-nbctl', '--columns=external_ids', 'list', table)).includes(env.id))
  })
  report.passed = true
} catch (error) {report.passed = false; report.error = error.message; process.exitCode = 1}
finally {
  const cleanup = async action => {try {await action()} catch (error) {report.cleanupErrors.push(error.message); process.exitCode = 1}}
  if (stoppedNode) await cleanup(() => node(stoppedNode, 'systemctl', 'start', 'netlab-node-dev.service'))
  if (env) await cleanup(destroy)
  if (blueprint) await cleanup(() => api(`/blueprints/${blueprint.id}`, 'DELETE', undefined, 204))
  for (const image of images) await cleanup(() => remove(image))
  if (registryNode) await cleanup(async () => {
    await node(registryNode, 'ctr', '-n', 'netlab', 'tasks', 'kill', '--signal', 'SIGTERM', registryInstance)
    await node(registryNode, 'ctr', '-n', 'netlab', 'tasks', 'delete', registryInstance)
    await node(registryNode, 'ctr', '-n', 'netlab', 'containers', 'delete', registryInstance)
  })
  for (const id of workers.keys()) await cleanup(async () => {
    await node(id, 'rm', '--', `${directory}/container.tar`, `${directory}/system.qcow2`)
    await node(id, 'rmdir', '--', directory)
  })
  if (report.cleanupErrors.length) report.passed = false
  report.finishedAt = new Date().toISOString()
  await writeFile('data/template-lifecycle-result.json', JSON.stringify(report, null, 2))
  console.log(JSON.stringify(report))
}
