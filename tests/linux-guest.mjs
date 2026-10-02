import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { delay, guestKey, guestSSH } from './guest-ssh.mjs'

export async function verifyLinuxGuest(api, completed, containerTemplate) {
  const source = process.env.NETLAB_TEST_LINUX_SOURCE || '/var/lib/netlab-dev/templates/ubuntu-24.04.qcow2'
  const key = guestKey()
  let environment
  let failure
  let result
  try {
    const template = await api('/templates', 'POST', {
      id: randomUUID(), name: 'Ubuntu 24.04 guest verification', kind: 'vm', os: 'Ubuntu 24.04', version: 1,
      source, initialization: 'cloud-init', resources: { cpu: 2, memoryMiB: 1024, diskGiB: 8 },
      hardware: { firmware: 'bios', machine: 'pc', diskBus: 'virtio', nicModel: 'virtio' },
    })
    let prepared
    const deadline = Date.now() + 120_000
    do {
      prepared = (await api(`/templates?ids=${template.id}`)).find(item => item.id === template.id)
      assert.notEqual(prepared?.state, 'failed', prepared?.error)
      if (prepared?.state === 'ready') break
      await delay(200)
    } while (Date.now() < deadline)
    assert.equal(prepared?.state, 'ready', 'Ubuntu镜像准备超时')
    const networks = [
      { id: randomUUID(), name: 'Guest LAN', cidr: '10.89.0.0/24', dnsServers: ['10.89.0.5'] },
      { id: randomUUID(), name: 'Guest Backend', cidr: '10.90.0.0/24', dnsServers: ['10.90.0.5'] },
    ]
    const interfaces = () => networks.map((network, index) => ({ id: randomUUID(), networkId: network.id, mac: '', address: '', primary: index === 0 }))
    const vm = { id: randomUUID(), name: 'Ubuntu', templateId: template.id, resources: prepared.resources,
      interfaces: interfaces(), guest: { username: 'netlab', hostname: 'netlab-guest', sshAuthorizedKeys: [key.publicKey] } }
    const client = { id: randomUUID(), name: 'guest-client', templateId: containerTemplate.id, resources: containerTemplate.resources, interfaces: interfaces() }
    const started = performance.now()
    environment = await api('/environments', 'POST', { name: 'Linux guest verification', spec: { networks, assets: [vm, client] }, run: true, clientRequestId: randomUUID() })
    await completed(environment.operationId)
    const hypervisorReadyMs = Math.round(performance.now() - started)
    environment = await api(`/environments/${environment.id}`)
    let current = environment.spec.assets.find(item => item.id === vm.id)
    const state = await api(`/environments/${environment.id}/state`)
    const instanceId = state.assets.find(item => item.assetId === vm.id).instanceId
    const clientInstance = state.assets.find(item => item.assetId === client.id).instanceId
    let address = current.interfaces[0].address
    const connection = await guestSSH(clientInstance, instanceId, () => address, key)
    const ssh = connection.run
    assert.match(ssh('cat', '/etc/os-release'), /VERSION_ID="24\.04"/)
    assert.equal(ssh('hostname').trim(), 'netlab-guest')
    const guestReadyMs = Math.round(performance.now() - started)
    const beforeBoot = ssh('cat', '/proc/sys/kernel/random/boot_id').trim()
    const interfacesBefore = JSON.parse(ssh('ip', '-j', 'address'))
    for (const expected of current.interfaces) {
      const actual = interfacesBefore.find(item => item.address === expected.mac)
      assert.ok(actual?.addr_info.some(item => item.local === expected.address), `来宾接口 ${expected.mac} 未配置 ${expected.address}`)
    }
    let defaults = JSON.parse(ssh('ip', '-j', 'route')).filter(item => item.dst === 'default')
    assert.equal(defaults.length, 1)
    assert.equal(defaults[0].gateway, '10.89.0.1')
    assert.match(ssh('resolvectl', 'dns'), /10\.89\.0\.5/)
    ssh('touch', '/home/netlab/data-kept')

    const next = structuredClone(environment.spec)
    const changed = next.assets.find(item => item.id === vm.id)
    changed.interfaces[0].address = '10.89.0.50'
    changed.interfaces[0].primary = false
    changed.interfaces[1].primary = true
    const preview = await api(`/environments/${environment.id}/changes`, 'POST', { expectedRevision: environment.revision, spec: next, apply: false })
    assert.equal(preview.changes.find(item => item.id === vm.id).effect, 'update')
    const updateStarted = performance.now()
    const operation = await api(`/environments/${environment.id}/changes`, 'POST', { expectedRevision: environment.revision, spec: next, apply: true, clientRequestId: randomUUID() })
    await completed(operation.id)
    address = '10.89.0.50'
    let boot = beforeBoot
    const updatedDeadline = Date.now() + 120_000
    while (boot === beforeBoot && Date.now() < updatedDeadline) {
      try { boot = ssh('cat', '/proc/sys/kernel/random/boot_id').trim() } catch { await delay(500) }
    }
    assert.notEqual(boot, beforeBoot, '初始化网络变更没有重启并生效')
    const afterState = await api(`/environments/${environment.id}/state`)
    assert.equal(afterState.assets.find(item => item.assetId === vm.id).instanceId, instanceId)
    defaults = JSON.parse(ssh('ip', '-j', 'route')).filter(item => item.dst === 'default')
    assert.equal(defaults.length, 1)
    assert.equal(defaults[0].gateway, '10.90.0.1')
    ssh('test', '-f', '/home/netlab/data-kept')
    assert.equal(ssh('hostname').trim(), 'netlab-guest')
    const networkUpdateMs = Math.round(performance.now() - updateStarted)
    const control = async action => {
      const operation = await api(`/environments/${environment.id}/assets/${vm.id}/actions`, 'POST', { action, clientRequestId: randomUUID() })
      await completed(operation.id)
    }
    await control('suspend')
    await control('stop')
    assert.equal((await api(`/environments/${environment.id}/state`)).assets.find(item => item.assetId === vm.id).state, 'stopped')
    await control('start')
    await connection.ready()
    ssh('test', '-f', '/home/netlab/data-kept')
    result = { hypervisorReadyMs, guestReadyMs, networkUpdateMs, system: 'Ubuntu 24.04', interfaces: 2, suspendedShutdown: true }
  } catch (error) {
    failure = error
  } finally {
    try {
      if (environment && !failure) {
        const operation = await api(`/environments/${environment.id}/actions`, 'POST', { action: 'destroy', clientRequestId: randomUUID() })
        await completed(operation.id)
      }
      if (!failure) key.remove()
    } catch (error) {
      failure = failure ? new AggregateError([failure, error], `${failure.message}; 清理失败：${error.message}`) : error
    }
  }
  if (failure) throw new Error(`Ubuntu环境 ${environment?.id ?? '尚未创建'}：${failure.message}`, { cause: failure })
  return result
}
