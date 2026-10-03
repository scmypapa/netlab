import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { execFile, spawn } from 'node:child_process'
import http from 'node:http'
import { readFile, writeFile, mkdir, stat } from 'node:fs/promises'
import { promisify } from 'node:util'
import { delay, guestKey, guestSSH, wsl } from './guest-ssh.mjs'

const execute = promisify(execFile), runId = randomUUID()
const base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const directory = `D:/.cache/netlab/artifacts/ingress-${runId}`, linuxDirectory = `/mnt/d/.cache/netlab/artifacts/ingress-${runId}`
const report = { startedAt: new Date().toISOString(), steps: [], templates: [], environments: [], expectedFailures: [] }
const workers = new Map(), envs = [], credentials = { username: 'netlab', password: randomUUID(), plainHttp: true }
const registryId = `netlab-registry-${runId}`, registry = '192.168.122.1:15051'
const reuseImports = process.env.NETLAB_TEST_IMPORTS
let cookie, origin, primary, baseline, ubuntu, container, vm, appliance, privateImage, guestCredentials, registryStarted = false
const quote = value => `'${String(value).replaceAll("'", "'\"'\"'")}'`
const errorText = error => {
  const message = error.message.replaceAll(credentials.password, '[credential]')
  return cookie ? message.replaceAll(cookie, '[session]') : message
}

async function api(path, method = 'GET', body, expected = 200) {
  const response = await fetch(`${base}/api/v1${path}`, { method, headers: { Cookie: cookie || '', 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(20000) })
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie')?.split(';')[0]
  const result = response.status === 204 ? undefined : await response.json()
  assert.equal(response.status, expected, `${method} ${path}: ${JSON.stringify(result)}`)
  if (path === '/templates' && method === 'POST') result.operationId = response.headers.get('Operation-Location')?.split('/').at(-1)
  return result
}
async function step(name, run) {
  const began = performance.now(), result = { name, passed: false }
  try { await run(); result.passed = true } catch (error) { result.error = errorText(error); throw error } finally { result.durationMs = Math.round(performance.now() - began); report.steps.push(result); console.log(JSON.stringify(result)) }
}
async function node(id, ...args) {
  const worker = workers.get(id)
  const command = worker?.host ? ['ssh', '-i', worker.keyPath, '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', `UserKnownHostsFile=${worker.keyPath}.hosts`, `root@${worker.host}`, args.map(quote).join(' ')] : args
  return (await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', ...command], { encoding: 'utf8', timeout: 120000, maxBuffer: 2 ** 20 })).stdout.trim()
}
async function operation(id, expected = 'succeeded') {
  assert.ok(id)
  const deadline = Date.now() + 180000
  while (Date.now() < deadline) {
    const result = await api(`/operations/${id}`)
    if (['succeeded', 'failed', 'partially_applied'].includes(result.state)) { assert.equal(result.state, expected, `${result.phase}: ${result.error}`); return result }
    await delay(200)
  }
  throw new Error(`operation timeout ${id}`)
}
async function upload(input, files, expected = 201) {
  const metadata = `${directory}/${input.id}.json`
  await writeFile(metadata, JSON.stringify(input))
  const args = ['--silent', '--show-error', '--max-time', '180', '-H', `Cookie: ${cookie}`, '--form', `template=@${metadata};type=application/json`]
  for (const file of files) args.push('--form', `files=@${file.path};filename=${file.name}`)
  args.push('-D', `${directory}/${input.id}.headers`, '-w', '\n%{http_code}', `${base}/api/v1/templates`)
  const raw = (await execute('curl.exe', args, { encoding: 'utf8', timeout: 190000, maxBuffer: 2 ** 20 })).stdout
  const boundary = raw.lastIndexOf('\n'), status = Number(raw.slice(boundary + 1)), result = JSON.parse(raw.slice(0, boundary))
  assert.equal(status, expected, `upload: ${status} ${JSON.stringify(result)}`)
  if (status === 201) {
    const headers = await readFile(`${directory}/${input.id}.headers`, 'utf8')
    result.operationId = headers.match(/Operation-Location: \/api\/v1\/operations\/([^\r\n]+)/i)?.[1]
    await operation(result.operationId)
    const [prepared] = await api(`/templates?ids=${result.id}`)
    assert.equal(prepared.state, 'ready', prepared.error)
    assert.equal(prepared.artifactNodeId, origin)
    assert.equal(await node(origin, 'find', `/var/lib/netlab-dev/imports/${result.id}/1`, '-type', 'f'), '')
    report.templates.push({ id: prepared.id, kind: prepared.kind, format: prepared.format, operationId: result.operationId })
    return prepared
  }
  return result
}
async function registryHash() {
  return new Promise((resolve, reject) => {
    const child = spawn('go', ['run', './tests/fixtures/registry-auth.go'], { env: { ...process.env, GOCACHE: 'D:/.cache/netlab/go-build', GOMODCACHE: 'D:/.cache/netlab/go-mod', GOPATH: 'D:/.cache/netlab/go', TEMP: 'D:/.cache/netlab/tmp', TMP: 'D:/.cache/netlab/tmp' } })
    let output = '', error = ''
    child.stdout.on('data', value => output += value)
    child.stderr.on('data', value => error += value)
    child.once('error', reject)
    child.once('close', code => code === 0 ? resolve(output) : reject(new Error(error)))
    child.stdin.end(credentials.password)
  })
}
async function destroy(env) {
  if ((await api(`/environments/${env.id}`)).status === 'destroyed') return
  await operation((await api(`/environments/${env.id}/actions`, 'POST', { action: 'destroy', clientRequestId: randomUUID() }, 202)).id)
}

try {
  await mkdir(directory, { recursive: true })
  await step(reuseImports ? '复用已验收的上传模板' : '流式上传 OCI 包和 Ubuntu 系统盘，源文件由节点接收', async () => {
    await api('/sessions/login', 'POST', JSON.parse(await readFile('data/dev-login.json', 'utf8')))
    baseline = await api('/nodes'); assert.equal(baseline.length, 2); assert.ok(baseline.every(node => node.state === 'ready'))
    primary = wsl('cat', '/var/lib/netlab-dev/node-id').trim()
    origin = baseline.toSorted((a, b) => a.id.localeCompare(b.id))[0].id
    for (const worker of JSON.parse(await readFile('D:/.cache/netlab/artifacts/multi-node-workers.json', 'utf8'))) workers.set(worker.nodeId, worker)
    const templates = await api('/templates?limit=100')
    ubuntu = templates.find(item => item.kind === 'vm' && item.state === 'ready' && item.source === '/var/lib/netlab-dev/templates/ubuntu-24.04.qcow2')
    assert.ok(ubuntu)
    if (reuseImports) {
      const previous = JSON.parse(await readFile(reuseImports, 'utf8'))
      const items = await api(`/templates?ids=${previous.templates.slice(0, 3).map(item => item.id).join(',')}`)
      ;[container, vm, appliance] = previous.templates.slice(0, 3).map(item => items.find(template => template.id === item.id))
      assert.ok([container, vm, appliance].every(template => template?.state === 'ready'))
      report.reusedFrom = reuseImports
      report.templates = previous.templates.slice(0, 3)
      return
    }
    const packageFile = 'D:/newgz/netlab/data/nginx.tar', vmFile = '//wsl.localhost/Ubuntu/var/lib/netlab-dev/templates/ubuntu-24.04.qcow2'
    report.uploadBytes = { container: (await stat(packageFile)).size, ubuntu: (await stat(vmFile)).size }
    container = await upload({ id: randomUUID(), name: `上传容器 ${runId}`, kind: 'container', os: 'Linux', version: 1, source: 'image.tar', format: 'docker', resources: { cpu: 1, memoryMiB: 128, diskGiB: 1 } }, [{ path: packageFile, name: 'image.tar' }])
    vm = await upload({ ...ubuntu, id: randomUUID(), artifactNodeId: undefined, name: `上传 Ubuntu ${runId}`, source: 'system.qcow2', disks: undefined }, [{ path: vmFile, name: 'system.qcow2' }])
  })
  if (!reuseImports) await step('OVF 与子目录中的系统盘一并上传并生成可启动模板', async () => {
    const descriptor = `${directory}/machine.ovf`
    const diskBytes = JSON.parse(await node(primary, 'qemu-img', 'info', '--output=json', ubuntu.source))['virtual-size']
    await writeFile(descriptor, `<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1" xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData" xmlns:vssd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_VirtualSystemSettingData"><References><File ovf:id="system" ovf:href="disks/system.qcow2"/></References><DiskSection><Disk ovf:diskId="boot" ovf:fileRef="system" ovf:capacity="${diskBytes}" ovf:capacityAllocationUnits="byte"/></DiskSection><VirtualSystem ovf:id="ubuntu"><VirtualHardwareSection><System><vssd:VirtualSystemType>${ubuntu.hardware.machine}</vssd:VirtualSystemType></System><Item><rasd:InstanceID>cpu</rasd:InstanceID><rasd:ResourceType>3</rasd:ResourceType><rasd:VirtualQuantity>${ubuntu.resources.cpu}</rasd:VirtualQuantity></Item><Item><rasd:InstanceID>mem</rasd:InstanceID><rasd:ResourceType>4</rasd:ResourceType><rasd:VirtualQuantity>${ubuntu.resources.memoryMiB}</rasd:VirtualQuantity></Item></VirtualHardwareSection></VirtualSystem></Envelope>`)
    appliance = await upload({ ...ubuntu, id: randomUUID(), artifactNodeId: undefined, name: `上传 OVF ${runId}`, source: 'vm/machine.ovf', format: 'ovf', disks: undefined }, [{ path: descriptor, name: 'vm/machine.ovf' }, { path: '//wsl.localhost/Ubuntu/var/lib/netlab-dev/templates/ubuntu-24.04.qcow2', name: 'vm/disks/system.qcow2' }])
    assert.equal(appliance.disks.length, 1)
  })
  await step('上传中断及数据库拒绝不留下模板、任务或节点文件', async () => {
    const definition = id => ({ id, name: '上传失败边界验收', kind: 'container', os: 'Linux', version: 1, source: 'image.tar', format: 'docker', resources: container.resources })
    const interrupted = randomUUID(), boundary = `netlab-${runId}`
    await new Promise((resolve, reject) => {
      const request = http.request(`${base}/api/v1/templates`, { method: 'POST', headers: { Cookie: cookie, 'Content-Type': `multipart/form-data; boundary=${boundary}` } })
      request.on('error', error => error.code === 'ECONNRESET' ? resolve() : reject(error))
      request.on('response', response => reject(new Error(`incomplete upload returned ${response.statusCode}`)))
      request.write(`--${boundary}\r\nContent-Disposition: form-data; name="template"\r\nContent-Type: application/json\r\n\r\n${JSON.stringify(definition(interrupted))}\r\n--${boundary}\r\nContent-Disposition: form-data; name="files"; filename="image.tar"\r\nContent-Type: application/octet-stream\r\n\r\n`)
      request.write(Buffer.alloc(512 * 1024))
      setTimeout(() => request.destroy(), 500)
    })
    await delay(500)
    assert.equal((await api(`/templates?ids=${interrupted}`)).length, 0)
    assert.equal(await node(origin, 'find', `/var/lib/netlab-dev/imports/${interrupted}`, '-type', 'f'), '')
    const rejected = randomUUID(), functionName = `netlab_ingress_${runId.replaceAll('-', '')}`
    const sql = query => execute('docker', ['exec', 'netlab-postgres-1', 'psql', '-v', 'ON_ERROR_STOP=1', '-U', 'netlab', '-d', 'netlab', '-Atc', query])
    await sql(`CREATE FUNCTION ${functionName}() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='${rejected}' THEN RAISE EXCEPTION 'injected template insert failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER ${functionName} BEFORE INSERT ON templates FOR EACH ROW EXECUTE FUNCTION ${functionName}();`)
    try {
      await upload(definition(rejected), [{ path: 'D:/newgz/netlab/data/nginx.tar', name: 'image.tar' }], 500)
      assert.equal((await api(`/templates?ids=${rejected}`)).length, 0)
      assert.equal(await node(origin, 'find', `/var/lib/netlab-dev/imports/${rejected}/1`, '-type', 'f'), '')
      assert.equal((await sql(`SELECT count(*) FROM operations WHERE scope_id IN ('${interrupted}','${rejected}')`)).stdout.trim(), '0')
      report.expectedFailures.push({ kind: 'upload-interrupted', templateId: interrupted }, { kind: 'database-insert', templateId: rejected })
    } finally { await sql(`DROP TRIGGER ${functionName} ON templates; DROP FUNCTION ${functionName}();`) }
  })
  await step('真实私有 Registry 拒绝错误认证，加密凭据支持原任务重试', async () => {
    await writeFile(`${directory}/htpasswd`, await registryHash())
    await node(primary, 'ctr', '-n', 'netlab', 'run', '-d', '--net-host', '--mount', `type=bind,src=${linuxDirectory},dst=/auth,options=rbind:ro`, '--env', 'REGISTRY_AUTH=htpasswd', '--env', 'REGISTRY_AUTH_HTPASSWD_REALM=Netlab', '--env', 'REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd', '--env', 'REGISTRY_HTTP_ADDR=192.168.122.1:15051', 'docker.io/library/registry:2', registryId)
    registryStarted = true
    const source = `${registry}/netlab/nginx:${runId}`
    const local = 'docker.io/library/nginx:alpine'
    const image = (await node(primary, 'ctr', '-n', 'netlab', 'images', 'list')).split('\n').find(line => line.startsWith(`${local} `))
    const index = JSON.parse(await node(primary, 'ctr', '-n', 'netlab', 'content', 'get', image.trim().split(/\s+/)[2]))
    const manifest = index.manifests.find(item => item.platform?.architecture === 'amd64' && item.platform?.os === 'linux')
    assert.ok(manifest)
    await node(primary, 'ctr', '-n', 'netlab', 'images', 'push', '--local', '--manifest', manifest.digest, '--manifest-type', manifest.mediaType, '--plain-http', '--user', `netlab:${credentials.password}`, source, local)
    await writeFile(`${directory}/manifest.json`, await node(primary, 'ctr', '-n', 'netlab', 'content', 'get', manifest.digest))
    await node(primary, 'curl', '--silent', '--show-error', '--fail', '--user', `netlab:${credentials.password}`, '--header', `Content-Type: ${manifest.mediaType}`, '--upload-file', `${linuxDirectory}/manifest.json`, `http://${registry}/v2/netlab/nginx/manifests/${runId}`)
    const tags = JSON.parse(await node(primary, 'curl', '--silent', '--show-error', '--fail', '--user', `netlab:${credentials.password}`, `http://${registry}/v2/netlab/nginx/tags/list`))
    assert.ok(tags.tags.includes(runId)); report.registryTags = tags.tags
    const input = { id: randomUUID(), name: `私有镜像 ${runId}`, kind: 'container', os: 'Linux', version: 1, source, format: 'oci', resources: container.resources, registry: credentials }
    const wrong = await api('/templates', 'POST', { ...input, id: randomUUID(), registry: { ...credentials, password: randomUUID() } }, 201)
    const failed = await operation(wrong.operationId, 'failed')
    assert.match(failed.error, /401|authorization|unauthorized|denied/i)
    report.expectedFailures.push({ kind: 'registry-auth', operationId: wrong.operationId, error: failed.error })
    // Wrong credentials remain wrong on retry; no fallback to anonymous or cached source tags.
    await operation((await api(`/operations/${wrong.operationId}/retry`, 'POST', undefined, 202)).id, 'failed')
    const created = await api('/templates', 'POST', input, 201)
    await operation(created.operationId)
    ;[privateImage] = await api(`/templates?ids=${created.id}`)
    assert.equal(privateImage.state, 'ready', privateImage.error)
    assert.ok(!('registry' in privateImage))
    report.templates.push({ id: privateImage.id, kind: privateImage.kind, format: privateImage.format, operationId: created.operationId })
    const definition = await execute('docker', ['exec', 'netlab-postgres-1', 'psql', '-U', 'netlab', '-d', 'netlab', '-Atc', `SELECT definition::text FROM templates WHERE id='${privateImage.id}'`])
    const payload = await execute('docker', ['exec', 'netlab-postgres-1', 'psql', '-U', 'netlab', '-d', 'netlab', '-Atc', `SELECT payload::text FROM operations WHERE id='${created.operationId}'`])
    assert.ok(!definition.stdout.includes(credentials.password)); assert.ok(!payload.stdout.includes(credentials.password))
    assert.match(payload.stdout, /templateCredentials/)
    report.credentialsEncrypted = true
  })
  await step('新模板自动跨节点运行 4 容器和 2 台 Ubuntu，验证真实 SSH', async () => {
    const networkId = randomUUID(), key = guestCredentials = guestKey(), templates = [container, container, privateImage, privateImage, vm, appliance]
    const spec = { networks: [{ id: networkId, name: '业务网', cidr: '10.87.0.0/24', mtu: 1400 }], assets: templates.map((template, index) => ({ id: randomUUID(), name: `${template.kind}-${index}`, templateId: template.id, resources: template.resources, interfaces: [{ id: randomUUID(), networkId, address: '', mac: '', primary: true }], ...(template.kind === 'vm' ? { guest: { username: 'netlab', sshAuthorizedKeys: [key.publicKey] } } : {}) })) }
    const env = await api('/environments', 'POST', { name: '镜像上传与认证验收', spec, run: true, clientRequestId: randomUUID() }, 201)
    envs.push(env); report.environments.push(env.id)
    await operation(env.operationId)
    const current = await api(`/environments/${env.id}`), actual = await api(`/environments/${env.id}/state`)
    assert.equal(new Set(actual.assets.map(asset => asset.nodeId)).size, 2)
    assert.ok(actual.assets.every(asset => asset.state === 'running'))
    report.placement = actual.assets.map(({ assetId, instanceId, nodeId }) => ({ assetId, instanceId, nodeId }))
    const client = actual.assets.find(asset => asset.nodeId === primary && current.spec.assets.find(item => item.id === asset.assetId).templateId === container.id)
    assert.ok(client)
    for (const target of actual.assets.filter(asset => [vm.id, appliance.id].includes(current.spec.assets.find(item => item.id === asset.assetId).templateId))) {
      const asset = current.spec.assets.find(item => item.id === target.assetId), ssh = await guestSSH(client.instanceId, target.instanceId, () => asset.interfaces[0].address, key)
      assert.match(ssh.run('cat', '/etc/os-release'), /Ubuntu/)
      assert.ok(ssh.run('cat', '/proc/sys/kernel/random/boot_id').trim())
    }
  })
  await step('销毁环境并核对容量、实例、网络和磁盘清理', async () => {
    for (const env of envs) {
      await destroy(env)
      assert.equal((await api(`/environments/${env.id}/state`)).assets.length, 0)
      for (const worker of baseline) {
        assert.equal(await node(worker.id, 'find', '/var/lib/netlab-dev/environments', '-path', `*/${env.id}/*`, '-type', 'f'), '')
        const outputs = await Promise.all([
          node(worker.id, 'ctr', '-n', 'netlab', 'containers', 'list'),
          node(worker.id, 'virsh', 'list', '--all', '--uuid'),
          node(worker.id, 'ovs-vsctl', '--columns=external_ids', 'list', 'Interface'),
        ])
        for (const target of report.placement.filter(item => item.nodeId === worker.id)) {
          assert.ok(outputs.every(output => !output.includes(target.instanceId)), `现场实例残留 ${target.instanceId}`)
        }
      }
      for (const table of ['Logical_Switch', 'Logical_Switch_Port', 'Logical_Router', 'Logical_Router_Port']) {
        assert.ok(!(await node(primary, 'ovn-nbctl', '--columns=external_ids', 'list', table)).includes(env.id), `${table} 残留`)
      }
    }
    const nodes = await api('/nodes')
    for (const before of baseline) assert.deepEqual(nodes.find(node => node.id === before.id).reserved, before.reserved)
  })
  report.passed = true
} catch (error) { report.passed = false; report.error = errorText(error) }
finally {
  report.cleanupErrors = []
  for (const env of envs) { try { await destroy(env) } catch (error) { report.cleanupErrors.push(errorText(error)) } }
  if (registryStarted) {
    try { await node(primary, 'ctr', '-n', 'netlab', 'tasks', 'kill', '--signal', 'SIGTERM', registryId); await delay(200); await node(primary, 'ctr', '-n', 'netlab', 'tasks', 'delete', registryId); await node(primary, 'ctr', '-n', 'netlab', 'containers', 'delete', registryId) } catch (error) { report.cleanupErrors.push(errorText(error)) }
  }
  if (guestCredentials) try { guestCredentials.remove() } catch (error) { report.cleanupErrors.push(errorText(error)) }
  if (report.cleanupErrors.length) report.passed = false
  report.finishedAt = new Date().toISOString()
  await writeFile('data/template-ingress-result.json', JSON.stringify(report, null, 2))
}
console.log(JSON.stringify({ passed: report.passed, error: report.error, cleanupErrors: report.cleanupErrors }))
if (!report.passed) process.exitCode = 1
