import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { execFile, execFileSync } from 'node:child_process'
import { promisify } from 'node:util'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { chromium } from '../web/node_modules/@playwright/test/index.mjs'
import { delay, guestKey } from './guest-ssh.mjs'

const execute = promisify(execFile), run = randomUUID(), base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const directory = `D:/.cache/netlab/artifacts/workspace-${run}`
const report = { startedAt: new Date().toISOString(), steps: [], environments: [], templates: [], cleanupErrors: [] }
const workers = new Map(JSON.parse(await readFile('D:/.cache/netlab/artifacts/multi-node-workers.json', 'utf8')).map(worker => [worker.nodeId, worker]))
const envs = [], key = guestKey()
const reuse = process.env.NETLAB_TEST_IMPORTS
let cookie, baseline, ubuntu, modern, installer, captured, source, installation, browser
const quote = value => `'${String(value).replaceAll("'", "'\"'\"'")}'`
async function node(id, ...args) {
  const worker = workers.get(id)
  const command = worker.host ? ['ssh', '-i', worker.keyPath, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${worker.keyPath}.hosts`, `root@${worker.host}`, args.map(quote).join(' ')] : args
  return (await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', ...command], {timeout: 180000, maxBuffer: 2 ** 20})).stdout.trim()
}
async function api(path, method = 'GET', body, status = 200) {
  const response = await fetch(`${base}/api/v1${path}`, {method, headers: {'Content-Type': 'application/json', Cookie: cookie || ''}, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(20000)})
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie')?.split(';')[0]
  const result = response.status === 204 ? undefined : await response.json()
  assert.equal(response.status, status, `${method} ${path}: ${JSON.stringify(result)}`)
  if (response.headers.has('Operation-Location')) result.operationId = response.headers.get('Operation-Location').split('/').at(-1)
  return result
}
async function operation(id) {
  const deadline = Date.now() + 180000
  while (Date.now() < deadline) {
    const result = await api(`/operations/${id}`)
    if (['succeeded', 'failed', 'partially_applied'].includes(result.state)) {
      assert.equal(result.state, 'succeeded', `${result.phase}: ${result.error}`)
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
async function upload(template, files) {
  const metadata = `${directory}/${template.id}.json`, headers = `${directory}/${template.id}.headers`
  await writeFile(metadata, JSON.stringify(template))
  const args = ['--silent', '--show-error', '--max-time', '180', '-H', `Cookie: ${cookie}`, '--form', `template=@${metadata};type=application/json`]
  for (const file of files) args.push('--form', `files=@${file.path};filename=${file.name}`)
  args.push('-D', headers, '-w', '\n%{http_code}', `${base}/api/v1/templates`)
  const raw = (await execute('curl.exe', args, {timeout: 190000, maxBuffer: 2 ** 20})).stdout
  assert.equal(Number(raw.slice(raw.lastIndexOf('\n') + 1)), 201, raw)
  const item = JSON.parse(raw.slice(0, raw.lastIndexOf('\n')))
  await operation((await readFile(headers, 'utf8')).match(/Operation-Location: \/api\/v1\/operations\/([^\r\n]+)/i)[1])
  const [prepared] = await api(`/templates?ids=${item.id}`)
  assert.equal(prepared.state, 'ready', prepared.error)
  report.templates.push({id: prepared.id, format: prepared.format, artifactNodeId: prepared.artifactNodeId})
  return prepared
}
function asset(template, name, networkId) {
  return {id: randomUUID(), name, templateId: template.id, resources: {...template.resources, cpu: 1}, guest: template.initialization === 'cloud-init' ? {username: 'netlab', sshAuthorizedKeys: [key.publicKey]} : undefined,
    interfaces: [{id: randomUUID(), networkId, mac: '', address: '', primary: true}]}
}
async function create(name, templates, volumes) {
  const networkId = randomUUID()
  const env = await api('/environments', 'POST', {name, run: true, spec: {networks: [{id: networkId, name: 'LAN', cidr: '10.76.0.0/24'}], assets: templates.map((template, index) => ({...asset(template, `${name}-${index}`, networkId), volumes}))}}, 201)
  envs.push(env); report.environments.push(env.id)
  await operation(env.operationId)
  return api(`/environments/${env.id}`)
}
async function action(env, assetId, kind) {
  await operation((await api(`/environments/${env.id}/assets/${assetId}/actions`, 'POST', {action: kind}, 202)).id)
}
async function ssh(env, assetId) {
  const current = await api(`/environments/${env.id}`)
  await operation((await api(`/environments/${env.id}/assets/${assetId}/services`, 'POST', {protocol: 'tcp', targetPort: 22, expectedRevision: current.revision}, 202)).id)
  const endpoint = (await api(`/environments/${env.id}/services`)).find(item => item.assetId === assetId)
  const primary = baseline.find(node => !workers.get(node.id).host).id
  const deadline = Date.now() + 120000
  let scanned, lastError
  while (Date.now() < deadline) {
    try {scanned = await node(primary, 'ssh-keyscan', '-T', '2', '-t', 'ed25519', '-p', String(endpoint.port), endpoint.address); if (scanned) break} catch (error) {lastError = error}
    await delay(500)
  }
  assert.ok(scanned, `SSH did not start: ${lastError?.message}`)
  const host = randomUUID(), hosts = `${key.path}.hosts`
  const lines = scanned.split('\n').filter(line => !line.startsWith('#') && line).map(line => `${host} ${line.split(' ').slice(1).join(' ')}`).join('\n')
  execFileSync('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', 'tee', '-a', hosts], {input: `${lines}\n`, stdio: ['pipe', 'ignore', 'pipe'], timeout: 10000})
  return {endpoint, host, hosts, primary}
}
async function guest(connection, ...command) {
  return node(connection.primary, 'ssh', '-i', key.path, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${connection.hosts}`, '-o', `HostKeyAlias=${connection.host}`, '-o', 'ConnectTimeout=3', '-p', String(connection.endpoint.port), `netlab@${connection.endpoint.address}`, ...command)
}
async function destroy(env) {
  if ((await api(`/environments/${env.id}`)).status !== 'destroyed') await operation((await api(`/environments/${env.id}/actions`, 'POST', {action: 'destroy'}, 202)).id)
}

try {
  await mkdir(directory, {recursive: true})
  await step(reuse ? '复用已验收的安装盘与 UEFI 模板' : '准备 BIOS 与 UEFI / Secure Boot / TPM 模板，上传真实安装盘和辅助光盘', async () => {
    await api('/sessions/login', 'POST', JSON.parse(await readFile('data/dev-login.json', 'utf8')))
    baseline = await api('/nodes'); assert.ok(baseline.every(node => node.state === 'ready'))
    ubuntu = (await api('/templates?limit=100')).find(item => item.source === '/var/lib/netlab-dev/templates/ubuntu-24.04.qcow2' && item.state === 'ready')
    assert.ok(ubuntu)
    if (reuse) {
      const previous = JSON.parse(await readFile(reuse, 'utf8'))
      const templates = await api(`/templates?ids=${previous.templates.slice(0, 2).map(template => template.id).join(',')}`)
      ;[modern, installer] = previous.templates.slice(0, 2).map(template => templates.find(item => item.id === template.id))
      assert.ok([modern, installer].every(template => template?.state === 'ready'))
      report.reusedFrom = reuse
      report.templates = previous.templates.slice(0, 2)
      return
    }
    const machine = baseline[0].vmHardware.machines.find(machine => machine.aliases.includes('q35') && machine.secureBoot && machine.tpm2)
    assert.ok(machine)
    modern = await upload({...ubuntu, id: randomUUID(), name: `UEFI Ubuntu ${run}`, source: 'ubuntu.qcow2', disks: undefined, stateFiles: undefined, nicModels: undefined, hardware: {...ubuntu.hardware, machine: machine.name, firmware: 'uefi', firmwareCode: undefined, firmwareVars: undefined, secureBoot: true, tpm: true, diskBus: 'virtio', diskController: undefined, nicModel: 'virtio'}, resources: {cpu: 1, memoryMiB: 1024, diskGiB: 4}}, [{path: '//wsl.localhost/Ubuntu/var/lib/netlab-dev/templates/ubuntu-24.04.qcow2', name: 'ubuntu.qcow2'}])
    const extra = `${directory}/auxiliary`; await mkdir(extra)
    await writeFile(`${extra}/netlab.txt`, run)
    const primary = baseline.find(node => !workers.get(node.id).host).id
    await node(primary, 'genisoimage', '-quiet', '-o', `/mnt/d/.cache/netlab/artifacts/workspace-${run}/auxiliary.iso`, '-V', 'NETLAB', `/mnt/d/.cache/netlab/artifacts/workspace-${run}/auxiliary`)
    installer = await upload({id: randomUUID(), name: `Alpine installer ${run}`, kind: 'vm', os: 'Linux', version: 1, format: 'iso', source: 'alpine.iso', resources: {cpu: 1, memoryMiB: 512, diskGiB: 4}, hardware: ubuntu.hardware, media: [{id: 'auxiliary', source: 'auxiliary.iso'}]}, [{path: 'D:/.cache/netlab/artifacts/alpine-standard-3.22.2.iso', name: 'alpine.iso'}, {path: `${directory}/auxiliary.iso`, name: 'auxiliary.iso'}])
    assert.equal(installer.disks.length, 1); assert.equal(installer.media.length, 2)
  })
  await step('真实 ISO 引导及浏览器控制台，弹出和重新挂载光驱保持原实例', async () => {
    installation = await create(`ISO installation ${run}`, [installer])
    const vm = installation.spec.assets[0], before = (await api(`/environments/${installation.id}/state`)).assets[0]
    browser = await chromium.launch({channel: 'msedge', headless: true})
    const context = await browser.newContext({viewport: {width: 1366, height: 900}})
    await context.addCookies([{name: 'netlab_session', value: cookie.split('=').slice(1).join('='), url: base}])
    const page = await context.newPage(); await page.goto(`${base}/environments/${installation.id}`)
    await page.getByRole('button', {name: '资产视图', exact: true}).click()
    await page.getByRole('button', {name: vm.name, exact: true}).click()
    await page.getByRole('button', {name: '控制台', exact: true}).click()
    await page.getByRole('status').filter({hasText: '已连接'}).waitFor()
    await delay(10000)
    await page.screenshot({path: `${directory}/iso-console.png`})
    report.isoScreenshot = `${directory}/iso-console.png`
    await action(installation, vm.id, 'force-stop')
    let current = await api(`/environments/${installation.id}`)
    current.spec.assets[0].media = []
    await operation((await api(`/environments/${installation.id}/changes`, 'POST', {expectedRevision: current.revision, spec: current.spec, apply: true}, 202)).id)
    current = await api(`/environments/${installation.id}`)
    const state = (await api(`/environments/${installation.id}/state`)).assets[0]
    assert.equal(state.instanceId, before.instanceId)
    const xml = await node(state.nodeId, 'virsh', 'dumpxml', state.instanceId)
    assert.equal((xml.match(/device='cdrom'/g) || []).length, 0)
    current.spec.assets[0].media = ['installer', 'auxiliary']
    await operation((await api(`/environments/${installation.id}/changes`, 'POST', {expectedRevision: current.revision, spec: current.spec, apply: true}, 202)).id)
    assert.equal(((await node(state.nodeId, 'virsh', 'dumpxml', state.instanceId)).match(/device='cdrom'/g) || []).length, 2)
    await context.close(); await destroy(installation)
  })
  await step('真实 Ubuntu 启动、SSH 修改磁盘；运行中固化被拒绝', async () => {
    source = await create(`Template source ${run}`, [modern], [{id: 'data', mountPath: '', sizeGiB: 1}])
    const vm = source.spec.assets[0], connection = await ssh(source, vm.id)
    assert.match(await guest(connection, 'uname', '-s'), /Linux/)
    await guest(connection, 'sh', '-c', quote(`printf '%s' '${run}' > ~/netlab-template-marker`))
    const current = await api(`/environments/${source.id}`)
    await api(`/environments/${source.id}/assets/${vm.id}/templates`, 'POST', {name: 'Running capture rejection', expectedRevision: current.revision}, 409)
    await action(source, vm.id, 'stop')
    const state = (await api(`/environments/${source.id}/state`)).assets[0]
    await node(state.nodeId, 'qemu-io', '-c', 'write -P 0x5a 4096 4096', `/var/lib/netlab-dev/environments/${source.id}/volumes/${vm.id}/data.qcow2`)
  })
  await step('关机固化实际系统盘、NVRAM 和 TPM；环境修订保持不变', async () => {
    const current = await api(`/environments/${source.id}`), vm = current.spec.assets[0]
    await api(`/environments/${source.id}/assets/${vm.id}/templates`, 'POST', {name: 'Stale capture', expectedRevision: current.revision - 1}, 409)
    captured = await api(`/environments/${source.id}/assets/${vm.id}/templates`, 'POST', {name: `Captured Ubuntu ${run}`, expectedRevision: current.revision}, 201)
    await operation(captured.operationId)
    captured = (await api(`/templates?ids=${captured.id}`))[0]
    assert.equal(captured.state, 'ready', captured.error)
    assert.equal(captured.initialization, 'cloud-init')
    assert.equal(captured.disks.length, 2)
    assert.equal(captured.volumes, undefined)
    assert.deepEqual(captured.stateFiles.toSorted(), ['nvram.fd', 'tpm.tar'])
    assert.equal(captured.media, undefined)
    const after = await api(`/environments/${source.id}`)
    assert.equal(after.revision, current.revision); assert.equal(after.status, current.status)
    report.templates.push({id: captured.id, format: captured.format, stateFiles: captured.stateFiles, artifactNodeId: captured.artifactNodeId})
    await action(source, vm.id, 'start')
  })
  await step('固化模板重新部署，真实 SSH 和持久数据保留', async () => {
    const clone = await create(`Captured deployment ${run}`, [captured, captured, captured, captured], [{id: 'runtime-data', mountPath: '', sizeGiB: 1}])
    const states = (await api(`/environments/${clone.id}/state`)).assets
    assert.equal(new Set(states.map(state => state.nodeId)).size, 2)
    for (const vm of clone.spec.assets) {
      const connection = await ssh(clone, vm.id)
      assert.equal(await guest(connection, 'cat', 'netlab-template-marker'), run)
      const state = states.find(state => state.assetId === vm.id)
      assert.match(await node(state.nodeId, 'virsh', 'dumpxml', state.instanceId), /secure='yes'/)
      assert.ok(await node(state.nodeId, 'find', `/var/lib/libvirt/swtpm/${state.instanceId}`, '-type', 'f'))
    }
    await operation((await api(`/environments/${clone.id}/actions`, 'POST', {action: 'stop'}, 202)).id)
    for (const state of states) {
      await node(state.nodeId, 'qemu-io', '-r', '-c', 'read -P 0x5a 4096 4096', `/var/lib/netlab-dev/environments/${clone.id}/instances/${state.instanceId}/disk-1.qcow2`)
    }
    report.cloneNodes = states.map(state => state.nodeId); report.sourceNode = captured.artifactNodeId
  })
  await step('销毁安装与固化环境，容量、实例、固件、TPM 和网络全部清理', async () => {
    const instances = (await Promise.all(envs.map(env => api(`/environments/${env.id}/state`)))).flatMap(state => state.assets)
    report.instances = instances
    await Promise.all(envs.map(destroy))
    for (const env of envs) assert.equal((await api(`/environments/${env.id}/state`)).assets.length, 0)
    for (const env of envs) {
      for (const worker of baseline) {
        assert.equal(await node(worker.id, 'find', '/var/lib/netlab-dev/environments', '-path', `/var/lib/netlab-dev/environments/${env.id}/*`, '-type', 'f'), '')
        assert.ok(!(await node(worker.id, 'virsh', 'list', '--all', '--uuid')).split('\n').some(id => instances.some(asset => asset.instanceId === id)))
        assert.ok(!(await node(worker.id, 'find', '/var/lib/libvirt/swtpm', '-mindepth', '1', '-maxdepth', '1')).split('\n').some(path => instances.some(asset => path.endsWith('/' + asset.instanceId))))
      }
    }
    for (const worker of baseline) {
      const interfaces = await node(worker.id, 'ovs-vsctl', '--columns=external_ids', 'list', 'Interface')
      for (const instance of instances) assert.ok(!interfaces.includes(instance.instanceId))
    }
    const primary = baseline.find(worker => !workers.get(worker.id).host).id
    for (const table of ['Logical_Switch', 'Logical_Switch_Port', 'Logical_Router', 'Logical_Router_Port']) {
      const objects = await node(primary, 'ovn-nbctl', '--columns=external_ids', 'list', table)
      for (const env of envs) assert.ok(!objects.includes(env.id), `${table} 残留`)
    }
    for (const node of await api('/nodes')) assert.deepEqual(node.reserved, baseline.find(item => item.id === node.id).reserved)
  })
} catch (error) {report.error = error.message.replaceAll(cookie || '[session]', '[session]'); process.exitCode = 1}
finally {
  await browser?.close()
  for (const env of envs) try {await destroy(env)} catch (error) {report.cleanupErrors.push({environment: env.id, error: error.message})}
  try {key.remove()} catch (error) {report.cleanupErrors.push({resource: 'guest SSH key', error: error.message})}
  report.finishedAt = new Date().toISOString()
  await writeFile('data/template-workspace-result.json', JSON.stringify(report, null, 2))
  console.log(JSON.stringify({passed: report.steps.filter(step => step.passed).length, total: report.steps.length, error: report.error, cleanupErrors: report.cleanupErrors}))
}
