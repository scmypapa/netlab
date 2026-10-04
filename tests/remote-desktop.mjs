import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { createReadStream } from 'node:fs'
import { readFile, writeFile } from 'node:fs/promises'
import { createRequire } from 'node:module'
import { guestKey, delay, wsl } from './guest-ssh.mjs'

const { chromium } = createRequire(new URL('../web/package.json', import.meta.url))('@playwright/test')
const { WebSocket } = createRequire(new URL('../web/package.json', import.meta.url))('ws')
const base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const report = { startedAt: new Date().toISOString(), steps: [], cleanupErrors: [] }
const environments = []
const resume = process.env.NETLAB_TEST_RDP_RESUME
let cookie, key, template, createdTemplate = false, browser, page, operator
const password = randomUUID() + '!'
const safeError = error => error.message.replaceAll(password, '[redacted]')
async function raw(path, method = 'GET', body, token) {
  return fetch(base + '/api/v1' + path, { method, headers: { ...(token ? { Authorization: 'Bearer ' + token } : { Cookie: cookie || '' }), ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(180_000) })
}
async function api(path, method = 'GET', body, token) {
  const response = await raw(path, method, body, token)
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie').split(';')[0]
  const value = response.status === 204 ? undefined : await response.json()
  assert.ok(response.ok, method + ' ' + path + ': ' + response.status + ' ' + JSON.stringify(value))
  return value
}
async function completed(id) {
  const deadline = Date.now() + 180_000
  while (Date.now() < deadline) { const operation = await api('/operations/' + id); if (['succeeded', 'failed', 'partially_applied'].includes(operation.state)) { assert.equal(operation.state, 'succeeded', operation.error); return }; await delay(200) }
  throw new Error('operation timed out')
}
async function step(name, run) {
  const record = { name, passed: false }, start = performance.now()
  try { await run(); record.passed = true } catch (error) { record.error = safeError(error); throw error } finally { record.durationMs = Math.round(performance.now() - start); report.steps.push(record); console.log(JSON.stringify(record)) }
}
const assetPath = environment => '/environments/' + environment.id + '/assets/' + environment.asset.id
async function upload(environment, path, body) {
  const response = await fetch(base + '/api/v1' + assetPath(environment) + '/files/content?path=' + encodeURIComponent(path), { method: 'PUT', headers: { Cookie: cookie, 'Content-Type': 'application/octet-stream' }, body, duplex: 'half', signal: AbortSignal.timeout(180_000) })
  assert.equal(response.status, 204, await response.text())
}
async function shell(environment, command) {
  return page.evaluate(({ base, path, command }) => new Promise((resolve, reject) => {
    const url = new URL('/api/v1' + path + '/console?kind=ssh', base)
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
    const socket = new WebSocket(url, 'binary'), decoder = new TextDecoder()
    let output = ''
    socket.binaryType = 'arraybuffer'
    const timer = setTimeout(() => { socket.close(); reject(new Error('guest command timed out')) }, 180_000)
    socket.onopen = () => socket.send(new TextEncoder().encode('stty -echo\n' + command + '; result=$?; printf "\\nNETLAB_COMMAND_DONE:%s\\n" "$result"; exit\n'))
    socket.onmessage = event => {
      output += decoder.decode(event.data, { stream: true })
      const done = output.match(/NETLAB_COMMAND_DONE:(\d+)\r?\n/)
      if (done) { clearTimeout(timer); socket.close(); Number(done[1]) === 0 ? resolve(output.slice(-8000)) : reject(new Error('guest command failed: ' + output.slice(-8000))) }
    }
    socket.onerror = () => { clearTimeout(timer); reject(new Error('SSH connection failed')) }
    socket.onclose = () => { clearTimeout(timer); reject(new Error('SSH ended before command completion: ' + output.slice(-1200))) }
  }), { base, path: assetPath(environment), command })
}
async function openDesktop(environment) {
  await page.goto(base + '/environments/' + environment.id)
  await page.getByRole('button', { name: '资产视图', exact: true }).click()
  await page.getByRole('button', { name: environment.asset.name, exact: true }).click()
  await page.getByRole('button', { name: '对象操作', exact: true }).click()
  await page.getByRole('menuitem', { name: '远程桌面', exact: true }).click()
}
try {
  await step(resume ? '复用两个已准备的真实 KVM 来宾' : '两个重叠网络环境和真实 Ubuntu KVM 来宾', async () => {
    assert.ok(process.env.NETLAB_TEST_PASSWORD, 'set NETLAB_TEST_PASSWORD')
    await api('/sessions/login', 'POST', { name: 'admin', password: process.env.NETLAB_TEST_PASSWORD })
    key = guestKey()
    if (resume) {
      report.resumedFrom = resume
      const previous = JSON.parse(await readFile(resume, 'utf8'))
      for (const retained of previous.retainedEnvironments) {
        const current = await api('/environments/' + retained.id)
        const asset = current.spec.assets.find(asset => asset.id === retained.assetId)
        assert.ok(asset, 'retained desktop asset is missing')
        environments.push({ id: retained.id, asset, label: retained.label })
      }
    } else {
    const source = process.env.NETLAB_TEST_LINUX_SOURCE || '/var/lib/netlab-dev/templates/ubuntu-24.04-guest.qcow2'
    const templates = await api('/templates?limit=100')
    template = templates.find(item => item.kind === 'vm' && item.state === 'ready' && item.source === source)
    if (!template) {
      createdTemplate = true
      template = await api('/templates', 'POST', { id: randomUUID(), name: 'Remote desktop verification', kind: 'vm', os: 'Ubuntu 24.04', version: 1, source, initialization: 'cloud-init', resources: { cpu: 2, memoryMiB: 2048, diskGiB: 8 }, hardware: { firmware: 'bios', machine: 'pc', diskBus: 'virtio', nicModel: 'virtio' } })
      const deadline = Date.now() + 120_000
      while (Date.now() < deadline) { template = (await api('/templates?ids=' + template.id))[0]; assert.notEqual(template.state, 'failed', template.error); if (template.state === 'ready') break; await delay(200) }
      assert.equal(template.state, 'ready')
    }
    for (const label of ['A', 'B']) {
      const network = { id: randomUUID(), name: 'LAN', cidr: '10.92.0.0/24' }
      const asset = { id: randomUUID(), name: 'Desktop ' + label, templateId: template.id, resources: template.resources, interfaces: [{ id: randomUUID(), networkId: network.id, address: '', mac: '', primary: true }], guest: { hostname: 'desktop-' + label.toLowerCase(), sshAuthorizedKeys: [key.publicKey] } }
      const result = await api('/environments', 'POST', { name: 'Desktop ' + label, run: true, clientRequestId: randomUUID(), spec: { networks: [network], assets: [asset] } })
      environments.push({ id: result.id, asset, label }); await completed(result.operationId)
    }
    }
    browser = await chromium.launch({ channel: 'msedge', headless: true })
    const context = await browser.newContext({ viewport: { width: 1366, height: 900 }, permissions: ['clipboard-read', 'clipboard-write'] })
    const [name, value] = cookie.split('='); await context.addCookies([{ name, value, url: base }])
    page = await context.newPage()
    page.on('websocket', socket => {
      if (!socket.url().includes('kind=rdp')) return
      const desktop = { frames: {}, details: [] }
      ;(report.desktops ??= []).push(desktop)
      socket.on('framereceived', ({ payload }) => {
        const text = payload.toString(), opcode = text.match(/^\d+\.([^,;]*)/)?.[1] || 'internal'
        desktop.frames[opcode] = (desktop.frames[opcode] ?? 0) + 1
        if (opcode === 'size' || opcode === 'error') desktop.details.push(text.replaceAll(password, '[redacted]'))
      })
    })
    await page.goto(base)
  })
  await step('通过平台 SSH 和流式 SFTP 准备真实桌面服务', async () => {
    for (const environment of environments) {
      if (resume) {
        environment.ssh = { ...await api(assetPath(environment) + '/ssh'), authKind: 'key', privateKey: wsl('cat', key.path) }
        await upload(environment, '/home/ubuntu/.ssh/authorized_keys', key.publicKey + '\n')
      } else {
      const deadline = Date.now() + 120_000
      let peer, failure
      while (Date.now() < deadline) { const response = await raw(assetPath(environment) + '/ssh/host-key', 'POST', { port: 22 }); if (response.ok) { peer = await response.json(); break }; failure = await response.text(); await delay(500) }
      assert.ok(peer, failure)
      environment.ssh = { username: 'ubuntu', port: 22, authKind: 'key', hostKey: peer.fingerprint, privateKey: wsl('cat', key.path) }
      }
      await api(assetPath(environment) + '/ssh', 'PUT', environment.ssh)
      if (!resume) await upload(environment, '/home/ubuntu/rdp-packages.tar.gz', createReadStream(process.env.NETLAB_TEST_RDP_PACKAGES || 'D:/.cache/netlab/artifacts/rdp-guest-packages.tar.gz'))
      await upload(environment, '/home/ubuntu/.netlab-rdp-password', password)
      await upload(environment, '/home/ubuntu/netlab-rdp-setup.sh', await readFile(new URL('./fixtures/desktop.sh', import.meta.url)))
      await shell(environment, 'bash /home/ubuntu/netlab-rdp-setup.sh ' + environment.label + (resume ? ' reuse' : ' install'))
      assert.equal((await api(assetPath(environment) + '/ssh')).authKind, 'key')
      let certificate, certificateError
      const desktopDeadline = Date.now() + 15_000
      while (Date.now() < desktopDeadline) {
        const response = await raw(assetPath(environment) + '/rdp/certificate', 'POST', { port: 3389 })
        if (response.ok) { certificate = await response.json(); break }
        certificateError = await response.text()
        await delay(200)
      }
      if (!certificate) report.desktopService = await shell(environment, 'sudo -n systemctl status xrdp --no-pager; sudo -n tail -n 60 /var/log/xrdp.log; sudo -n journalctl -u xrdp -n 20 --no-pager').catch(safeError)
      assert.ok(certificate, certificateError)
      environment.rdp = { username: 'ubuntu', port: 3389, certificate: certificate.fingerprint, password }
      await api(assetPath(environment) + '/rdp', 'PUT', environment.rdp)
      const settings = await api(assetPath(environment) + '/rdp')
      assert.equal(settings.password, undefined); assert.equal(settings.certificate, certificate.fingerprint)
    }
    assert.notEqual(environments[0].rdp.certificate, environments[1].rdp.certificate)
  })
  await step('真实 SSH 密码、加密私钥认证和凭据保留', async () => {
    const environment = environments[0]
    await api(assetPath(environment) + '/ssh', 'PUT', { username: 'ubuntu', port: 22, authKind: 'password', hostKey: environment.ssh.hostKey, password })
    await shell(environment, 'test "$(id -un)" = ubuntu')
    const encrypted = key.path + '.encrypted'
    wsl('cp', key.path, encrypted); wsl('ssh-keygen', '-p', '-q', '-P', '', '-N', password, '-f', encrypted)
    try {
      await api(assetPath(environment) + '/ssh', 'PUT', { ...environment.ssh, privateKey: wsl('cat', encrypted), passphrase: password })
      await api(assetPath(environment) + '/ssh', 'PUT', await api(assetPath(environment) + '/ssh'))
      await shell(environment, 'test "$(id -un)" = ubuntu')
    } finally { wsl('rm', '-f', '--', encrypted) }
  })
  await step('浏览器真实桌面、键鼠输入和环境隔离', async () => {
    for (const environment of environments) {
      await openDesktop(environment)
      const panel = page.getByRole('tabpanel')
      await panel.getByRole('status').filter({ hasText: '已连接' }).waitFor({ timeout: 30_000 })
      const screen = page.getByLabel('远程桌面画面', { exact: true })
      const inputDeadline = Date.now() + 15_000
      let inputReady = false
      while (Date.now() < inputDeadline) {
        const response = await fetch(base + '/api/v1' + assetPath(environment) + '/files/content?path=/home/ubuntu/.netlab-desktop-ready', { headers: { Cookie: cookie } })
        if (response.ok) { inputReady = true; break }
        await delay(200)
      }
      assert.ok(inputReady, 'guest desktop terminal did not open')
      await screen.click()
      await page.keyboard.type('printf ' + environment.label + ' > ~/rdp-proof.txt', { delay: 10 }); await page.keyboard.press('Enter')
      const deadline = Date.now() + 10_000
      let content
      while (Date.now() < deadline) { const response = await fetch(base + '/api/v1' + assetPath(environment) + '/files/content?path=/home/ubuntu/rdp-proof.txt', { headers: { Cookie: cookie } }); if (response.ok) { content = await response.text(); break }; await delay(200) }
      assert.equal(content, environment.label, 'RDP keyboard command must execute in the selected guest')
      if (environment.label === 'A') {
        for (const width of [390, 1366, 1920, 2560]) { await page.setViewportSize({ width, height: width === 390 ? 844 : 900 }); assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'horizontal overflow at ' + width) }
        await page.setViewportSize({ width: 1366, height: 900 }); await page.screenshot({ path: 'data/rdp-desktop.png' })
      }
      await page.getByRole('button', { name: '结束 ' + environment.asset.name + ' 连接', exact: true }).click()
    }
  })
  await step('错误证书拒绝连接和密码字段保留', async () => {
    const environment = environments[0], saved = await api(assetPath(environment) + '/rdp')
    await api(assetPath(environment) + '/rdp', 'PUT', { ...saved, certificate: 'sha256:' + '00'.repeat(32) })
    await openDesktop(environment)
    await page.getByRole('tabpanel').getByRole('status').filter({ hasNotText: /已连接|连接中/ }).waitFor({ timeout: 30_000 })
    await page.getByRole('button', { name: '结束 ' + environment.asset.name + ' 连接', exact: true }).click()
    await api(assetPath(environment) + '/rdp', 'PUT', saved)
  })
  await step('主体隔离、会话权限和进行中撤权', async () => {
    const environment = environments[0]
    operator = await api('/service-tokens', 'POST', { name: 'RDP operator', grants: [{ scopeKind: 'asset', scopeId: environment.id + '/' + environment.asset.id, permissions: ['read', 'session'] }] })
    assert.equal((await raw(assetPath(environment) + '/rdp', 'GET', undefined, operator.token)).status, 404)
    await api(assetPath(environment) + '/rdp', 'PUT', environment.rdp, operator.token)
    assert.equal((await raw(assetPath(environment) + '/files?path=/', 'GET', undefined, operator.token)).status, 403)
    assert.equal((await raw(assetPath(environments[1]) + '/rdp', 'GET', undefined, operator.token)).status, 403)
    const url = new URL('/api/v1' + assetPath(environment) + '/console?kind=rdp', base); url.protocol = 'ws:'
    const socket = new WebSocket(url, 'guacamole', { headers: { Authorization: 'Bearer ' + operator.token } })
    await new Promise((resolve, reject) => {
      const timer = setTimeout(() => { socket.terminate(); reject(new Error('restricted RDP desktop did not render')) }, 20_000)
      socket.on('message', frame => {
        const text = frame.toString()
        if (text.startsWith('4.sync,')) socket.send(text)
        if (text.startsWith('4.size,1.0,')) { clearTimeout(timer); resolve() }
      })
      socket.on('error', error => { clearTimeout(timer); reject(error) })
      socket.on('close', code => { clearTimeout(timer); reject(new Error('restricted RDP closed: ' + code)) })
    })
    const closed = new Promise(resolve => socket.once('close', resolve))
    await api('/service-tokens/' + operator.principal.id, 'DELETE')
    assert.equal(await Promise.race([closed, delay(10_000).then(() => { throw new Error('revoked desktop did not close') })]), 1008)
    assert.equal((await raw(assetPath(environment) + '/rdp', 'GET', undefined, operator.token)).status, 401)
    operator = undefined
  })
  report.passed = true
} catch (error) {
  report.passed = false; report.error = safeError(error); report.stack = error.stack.replaceAll(password, '[redacted]'); process.exitCode = 1
  if (page) { await page.screenshot({ path: 'data/rdp-error.png' }).catch(() => {}); report.browserText = (await page.locator('body').innerText()).slice(-5000) }
  if (page && environments[0]?.ssh) report.desktopService = await shell(environments[0], 'sudo -n tail -n 60 /var/log/xrdp.log; sudo -n tail -n 40 /var/log/xrdp-sesman.log').catch(safeError)
} finally {
  await browser?.close()
  if (operator) await api('/service-tokens/' + operator.principal.id, 'DELETE').catch(error => report.cleanupErrors.push(error.message))
  if (report.passed) {
    for (const environment of environments) await api('/environments/' + environment.id + '/actions', 'POST', { action: 'destroy', clientRequestId: randomUUID() }).then(result => completed(result.id)).catch(error => report.cleanupErrors.push(error.message))
    if (createdTemplate && template) await api('/templates/' + template.id, 'DELETE').then(result => completed(result.id)).catch(error => report.cleanupErrors.push(error.message))
  } else report.retainedEnvironments = environments.map(({ id, asset, label }) => ({ id, assetId: asset.id, label }))
  key?.remove()
  if (report.cleanupErrors.length) { report.passed = false; process.exitCode = 1 }
  report.finishedAt = new Date().toISOString(); await writeFile('data/rdp-result.json', JSON.stringify(report, null, 2)); console.log(JSON.stringify({ passed: report.passed, steps: report.steps.length, cleanupErrors: report.cleanupErrors, error: report.error }))
}
