import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { readFile, writeFile } from 'node:fs/promises'
import { delay } from './guest-ssh.mjs'
import { qga as guestAgent, ready as guestReady, powershell } from './windows-qga.mjs'

const base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const report = { startedAt: new Date().toISOString(), steps: [], cleanupErrors: [] }
let cookie, environment, point, actual

async function api(path, method = 'GET', body) {
  const response = await fetch(base + '/api/v1' + path, { method, headers: { Cookie: cookie || '', 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(30_000) })
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie')?.split(';')[0]
  const text = await response.text()
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${text}`)
  return text ? JSON.parse(text) : undefined
}
async function complete(id) {
  for (const deadline = Date.now() + 180_000; Date.now() < deadline;) {
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
const qga = request => guestAgent(actual, request)
const ready = (probe = () => qga({ execute: 'guest-get-osinfo' })) => guestReady(probe)
const shell = script => powershell(actual, script)
try {
  await api('/sessions/login', 'POST', JSON.parse(await readFile('data/dev-login.json', 'utf8')))
  await step('从已有 Windows 模板启动真实来宾并确认 QGA', async () => {
    const saved = JSON.parse(await readFile('data/windows-guest-state.json', 'utf8'))
    const template = (await api('/templates?ids=' + saved.template.id))[0]
    assert.equal(template.state, 'ready')
    const network = { id: randomUUID(), name: 'VSS LAN', cidr: '10.98.0.0/24' }
    environment = await api('/environments', 'POST', { name: 'Windows VSS verification', run: true, spec: { networks: [network], assets: [{ id: randomUUID(), name: 'Windows VSS', templateId: template.id, resources: template.resources, guest: { hostname: 'netlab-vss', username: 'netlab' }, interfaces: [{ id: randomUUID(), networkId: network.id, address: '', mac: '', primary: true }] }] } })
    await complete(environment.operationId)
    actual = (await api(`/environments/${environment.id}/state`)).assets[0]
    report.guest = await ready()
    assert.equal(report.guest.id, 'mswindows')
    report.environmentId = environment.id
    await ready(async () => {
      report.setup = JSON.parse(await shell("Get-ItemProperty HKLM:\\SYSTEM\\Setup | Select-Object SystemSetupInProgress,OOBEInProgress | ConvertTo-Json"))
      assert.equal(report.setup.SystemSetupInProgress, 0, 'Windows first boot specialization')
      assert.equal(report.setup.OOBEInProgress, 0, 'Windows first boot OOBE')
      assert.equal(await shell("(Get-Content -LiteralPath 'C:\\Program Files\\Cloudbase Solutions\\Cloudbase-Init\\log\\cloudbase-init.log' -Tail 200 | Select-String -SimpleMatch 'Plugins execution done').Count -gt 0"), 'True', 'Windows guest initialization')
    })
    await shell("New-Item -ItemType Directory -Path C:\\NetlabVSS -Force | Out-Null; Set-Content C:\\NetlabVSS\\proof.txt 'before-snapshot'")
  })
  await step('产品恢复点执行 Windows VSS，记录应用一致性且完成解冻', async () => {
    environment = await api(`/environments/${environment.id}`)
    point = await api(`/environments/${environment.id}/recovery-points`, 'POST', { name: 'VSS verification', expectedRevision: environment.revision })
    await complete(point.operationId)
    report.point = (await api(`/environments/${environment.id}/recovery-points`)).find(item => item.id === point.id)
    assert.equal(report.point.consistency, 'application')
    assert.equal(await qga({ execute: 'guest-fsfreeze-status' }), 'thawed')
  })
  await step('修改来宾文件，再通过产品恢复原始内容', async () => {
    await shell("Set-Content C:\\NetlabVSS\\proof.txt 'after-snapshot'")
    environment = await api(`/environments/${environment.id}`)
    await complete((await api(`/environments/${environment.id}/recovery-points/${point.id}/restore`, 'POST', { expectedRevision: environment.revision })).id)
    actual = (await api(`/environments/${environment.id}/state`)).assets[0]
    await ready()
    assert.equal(await shell('Get-Content C:\\NetlabVSS\\proof.txt'), 'before-snapshot')
  })
} catch (error) {
  report.error = error.message; process.exitCode = 1
  if (actual) try {
    report.guestDiagnostics = await shell("Get-Service VSS,swprv,EventSystem,COMSysApp,qemu-ga | Select-Object Name,Status,StartType | ConvertTo-Json; Get-WinEvent -FilterHashtable @{LogName='Application'; StartTime=(Get-Date).AddMinutes(-10)} -ErrorAction SilentlyContinue | Where-Object { $_.ProviderName -in @('VSS','EventSystem') } | Select-Object -First 12 Id,ProviderName,Message | ConvertTo-Json; & vssadmin.exe list writers")
  } catch (failure) { report.diagnosticsError = failure.message }
} finally {
  if (point) try { await complete((await api(`/environments/${environment.id}/recovery-points/${point.id}`, 'DELETE')).id) } catch (error) { report.cleanupErrors.push(error.message) }
  if (environment) try { await complete((await api(`/environments/${environment.id}/actions`, 'POST', { action: 'destroy' })).id); assert.equal((await api(`/environments/${environment.id}/state`)).assets.length, 0) } catch (error) { report.cleanupErrors.push(error.message) }
  report.finishedAt = new Date().toISOString(); report.passed = !report.error && !report.cleanupErrors.length
  if (!report.passed) process.exitCode = 1
  await writeFile('data/windows-vss-result.json', JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ passed: report.passed, error: report.error, cleanupErrors: report.cleanupErrors }))
}
