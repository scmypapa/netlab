import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { execFile } from 'node:child_process'
import { copyFile, readFile, writeFile } from 'node:fs/promises'
import { createRequire } from 'node:module'
import { promisify, stripVTControlCharacters } from 'node:util'
import { delay } from './guest-ssh.mjs'

const require = createRequire(new URL('../web/package.json', import.meta.url))
const { chromium } = require('@playwright/test')
const execute = promisify(execFile), base = process.env.NETLAB_TEST_URL || 'http://127.0.0.1:8090'
const cache = 'D:/.cache/netlab/artifacts', media = cache + '/windows-media'
const stateFile = 'data/windows-guest-state.json', resume = process.env.NETLAB_TEST_WINDOWS_RESUME
const report = { startedAt: new Date().toISOString(), steps: [], cleanupErrors: [] }
const saved = resume ? JSON.parse(await readFile(stateFile, 'utf8')) : { password: randomUUID() + '!', environments: [] }
for (const run of saved.previousRuns ?? []) delete run.previousRuns
report.installationRecovery = saved.installationRecovery
report.previousRuns = saved.previousRuns
report.clockCorrection = saved.clockCorrection
report.templatePreparationRepair = saved.templatePreparationRepair
let cookie, browser, page
const desktopSizes = new Map()
let sentKeys = []
const clean = error => error.message.replaceAll(saved.password, '[redacted]').replaceAll(cookie || '[session]', '[session]')
async function checkpoint() { await writeFile(stateFile, JSON.stringify(saved, null, 2)) }
async function raw(path, method = 'GET', body, timeout = 180_000) {
  return fetch(base + '/api/v1' + path, { method, headers: { Cookie: cookie || '', ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(timeout) })
}
async function api(path, method = 'GET', body) {
  const response = await raw(path, method, body)
  if (path === '/sessions/login') cookie = response.headers.get('set-cookie').split(';')[0]
  const result = response.status === 204 ? undefined : await response.json()
  assert.ok(response.ok, method + ' ' + path + ': ' + response.status + ' ' + JSON.stringify(result))
  return result
}
async function complete(id) {
  const deadline = Date.now() + 180_000
  while (Date.now() < deadline) { const item = await api('/operations/' + id); if (['succeeded', 'failed', 'partially_applied'].includes(item.state)) { assert.equal(item.state, 'succeeded', item.error); return item }; await delay(250) }
  throw new Error('operation timed out: ' + id)
}
async function step(name, action) {
  const item = { name, passed: false }, start = performance.now()
  try { await action(); item.passed = true } catch (error) { item.error = clean(error); throw error } finally { item.durationMs = Math.round(performance.now() - start); report.steps.push(item); console.log(JSON.stringify(item)); await writeFile('data/windows-guest-result.json', JSON.stringify(report, null, 2)) }
}
const assetPath = environment => '/environments/' + environment.id + '/assets/' + environment.asset.id
async function shell(environment, command) {
  const script = "try { $ErrorActionPreference='Stop'; & { " + command + " } | Out-String -Width 200 | Write-Output; Write-Output 'NETLAB_COMMAND_DONE:0' } catch { Write-Output ($_ | Out-String); Write-Output 'NETLAB_COMMAND_DONE:1' }; exit"
  let output = ''
  const watch = socket => { if (socket.url().includes(assetPath(environment) + '/console?kind=ssh')) socket.on('framereceived', ({ payload }) => { output += payload.toString() }) }
  page.on('websocket', watch)
  try {
    await open(environment, 'ssh')
    const deadline = Date.now() + 60_000
    while (!/PS [^>\r\n]*>/.test(stripVTControlCharacters(output)) && Date.now() < deadline) await delay(100)
    assert.match(stripVTControlCharacters(output), /PS [^>\r\n]*>/, 'Windows terminal prompt did not open')
    await page.locator('.xterm-helper-textarea').focus()
    await page.keyboard.insertText('powershell.exe -NoProfile -NonInteractive -EncodedCommand ' + Buffer.from(script, 'utf16le').toString('base64'))
    await page.keyboard.press('Enter')
    let done
    while (Date.now() < deadline) { done = stripVTControlCharacters(output).match(/NETLAB_COMMAND_DONE:(\d+)\r?\n/); if (done) break; await delay(100) }
    assert.ok(done, 'Windows SSH command timed out: ' + stripVTControlCharacters(output).slice(-2000))
    assert.equal(done[1], '0', stripVTControlCharacters(output).slice(-4000))
    return stripVTControlCharacters(output)
  } finally { page.off('websocket', watch) }
}
async function content(environment, path, bytes) {
  const response = await fetch(base + '/api/v1' + assetPath(environment) + '/files/content?path=' + encodeURIComponent(path), { method: bytes === undefined ? 'GET' : 'PUT', headers: { Cookie: cookie, ...(bytes === undefined ? {} : { 'Content-Type': 'application/octet-stream' }) }, body: bytes, signal: AbortSignal.timeout(60_000) })
  assert.ok(response.ok, 'file ' + path + ': ' + response.status + ' ' + (response.ok ? '' : await response.text()))
  return response.arrayBuffer()
}
async function localGuestPowerShell(instanceId, script) {
  const qga = async request => JSON.parse((await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', 'virsh', 'qemu-agent-command', instanceId, JSON.stringify(request)], { timeout: 15_000 })).stdout).return
  const started = await qga({ execute: 'guest-exec', arguments: { path: 'C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe', arg: ['-NoProfile', '-NonInteractive', '-EncodedCommand', Buffer.from(script, 'utf16le').toString('base64')], 'capture-output': true } })
  const deadline = Date.now() + 60_000
  while (Date.now() < deadline) {
    const result = await qga({ execute: 'guest-exec-status', arguments: { pid: started.pid } })
    if (result.exited) { assert.equal(result.exitcode, 0, Buffer.from(result['err-data'] || '', 'base64').toString()); return Buffer.from(result['out-data'] || '', 'base64').toString().trim() }
    await delay(250)
  }
  throw new Error('Windows guest command timed out')
}
async function connectSSH(environment, seconds = 120) {
  const deadline = Date.now() + seconds * 1000
  let peer, failure
  while (Date.now() < deadline) { const response = await raw(assetPath(environment) + '/ssh/host-key', 'POST', { port: 22 }); if (response.ok) { peer = await response.json(); break }; failure = await response.text(); await delay(1000) }
  assert.ok(peer, 'Windows SSH was not ready: ' + failure)
  await api(assetPath(environment) + '/ssh', 'PUT', { username: 'netlab', port: 22, authKind: 'password', password: saved.password, hostKey: peer.fingerprint })
  environment.hostKey = peer.fingerprint
  if (environment.label) {
    let initialized = false
    while (Date.now() < deadline) {
      try {
        const response = await raw(assetPath(environment) + '/files/content?path=' + encodeURIComponent('/C:/Program Files/Cloudbase Solutions/Cloudbase-Init/log/cloudbase-init.log'), 'GET', undefined, Math.min(15_000, deadline - Date.now()))
        failure = await response.text()
        if (response.ok && failure.includes('Plugins execution done')) { initialized = true; break }
      } catch (error) {
        if (!['TimeoutError', 'AbortError'].includes(error.name)) throw error
        failure = error.message
      }
      await delay(1000)
    }
    assert.ok(initialized, 'Windows ' + environment.label + ' initialization did not complete: ' + failure.slice(-2000))
  }
  return shell(environment, "Get-Content C:\\ProgramData\\NetlabTests\\setup.done")
}
async function open(environment, kind) {
  await page.goto(base + '/environments/' + environment.id)
  await page.getByRole('button', { name: '资产视图', exact: true }).click()
  await page.getByRole('button', { name: environment.asset.name, exact: true }).click()
  if (kind === 'vnc') await page.getByRole('button', { name: '控制台', exact: true }).click()
  else { await page.getByRole('button', { name: '对象操作', exact: true }).click(); await page.getByRole('menuitem', { name: kind === 'ssh' ? 'SSH' : '远程桌面', exact: true }).click() }
}
async function create(name, template, guest) {
  const network = { id: randomUUID(), name: 'LAN', cidr: '10.94.0.0/24' }
  const asset = { id: randomUUID(), name, templateId: template.id, resources: template.resources, guest, interfaces: [{ id: randomUUID(), networkId: network.id, address: '', mac: '', primary: true }] }
  const env = await api('/environments', 'POST', { name, run: true, spec: { networks: [network], assets: [asset] } })
  const record = { id: env.id, asset, operationId: env.operationId }
  saved.environments.push(record); await checkpoint(); await complete(env.operationId)
  return record
}
try {
  await api('/sessions/login', 'POST', JSON.parse(await readFile('data/dev-login.json', 'utf8')))
  browser = await chromium.launch({ channel: 'msedge', headless: true })
  const context = await browser.newContext({ viewport: { width: 1366, height: 900 }, permissions: ['clipboard-read', 'clipboard-write'] })
  const [name, value] = cookie.split('='); await context.addCookies([{ name, value, url: base }]); page = await context.newPage()
  page.on('websocket', socket => {
    if (!socket.url().includes('kind=rdp')) return
    const assetId = new URL(socket.url()).pathname.split('/').at(-2)
    socket.on('framesent', ({ payload }) => { const value = payload.toString(); if (value.startsWith('3.key,')) sentKeys.push(value) })
    socket.on('framereceived', ({ payload }) => {
      for (const match of payload.toString().matchAll(/4\.size,1\.0,\d+\.(\d+),\d+\.(\d+);/g)) desktopSizes.set(assetId, { width: Number(match[1]), height: Number(match[2]) })
    })
  })
  await step('官方 Windows Server 2025 安装介质和 VirtIO / Guest Agent 准备', async () => {
    if (saved.installer) { await api('/templates?ids=' + saved.installer.id); return }
    const nodes = await api('/nodes'), machine = nodes.find(node => node.name === 'Local node').vmHardware.machines.find(machine => machine.aliases.includes('q35') && machine.secureBoot && machine.tpm2)
    assert.ok(machine)
    await copyFile(cache + '/CloudbaseInitSetup_1_1_8_x64.msi', media + '/CloudbaseInitSetup_1_1_8_x64.msi')
    await copyFile(new URL('./fixtures/windows.ps1', import.meta.url), media + '/windows.ps1')
    const xml = (await readFile(new URL('./fixtures/windows-unattend.xml', import.meta.url), 'utf8')).replaceAll('@@PASSWORD@@', saved.password)
    await writeFile(media + '/Autounattend.xml', xml)
    await execute('wsl.exe', ['-d', 'Ubuntu', '-u', 'root', '--exec', 'genisoimage', '-quiet', '-joliet', '-rock', '-V', 'NETLAB_SETUP', '-o', '/mnt/d/.cache/netlab/artifacts/windows-setup.iso', '/mnt/d/.cache/netlab/artifacts/windows-media'])
    const template = { id: randomUUID(), name: 'Windows Server 2025 installer', kind: 'vm', os: 'Windows Server 2025', version: 1, format: 'iso', source: 'windows.iso', resources: { cpu: 2, memoryMiB: 3072, diskGiB: 40 }, hardware: { firmware: 'uefi', secureBoot: true, tpm: true, guestAgent: true, cpuModel: 'host-passthrough', machine: machine.name, diskBus: 'virtio', nicModel: 'virtio' }, media: [{ id: 'setup', source: 'setup.iso' }] }
    await writeFile(cache + '/windows-upload.json', JSON.stringify(template))
    const upload = await execute('curl.exe', ['-fsS', '--max-time', '600', '-H', 'Cookie: ' + cookie, '--form', 'template=@' + cache + '/windows-upload.json;type=application/json', '--form', 'files=@' + cache + '/windows-server2025-eval.iso;filename=windows.iso', '--form', 'files=@' + cache + '/windows-setup.iso;filename=setup.iso', base + '/api/v1/templates'], { timeout: 610_000, maxBuffer: 2 ** 20 })
    saved.installer = JSON.parse(upload.stdout); await checkpoint(); await complete(saved.installer.operationId)
    saved.installer = (await api('/templates?ids=' + saved.installer.id))[0]; await checkpoint()
  })
  await step('真实 ISO 自动安装、Secure Boot / TPM、VirtIO 磁盘和网卡', async () => {
    if (saved.installationVerified) { report.reusedInstallation = true; return }
    saved.source ??= saved.environments.find(env => env.asset.name === 'Windows ISO installation')
    if (!saved.source) { saved.source = await create('Windows ISO installation', saved.installer); await checkpoint(); await open(saved.source, 'vnc'); const canvas = page.getByRole('tabpanel').locator('canvas'); await canvas.click(); for (let i = 0; i < 6; i++) { await page.keyboard.press('Enter'); await delay(500) } }
    else await open(saved.source, 'vnc')
    const setup = await connectSSH(saved.source, 1800)
    assert.match(setup, /Windows Server 2025/); assert.match(setup, /"secureBoot"\s*:\s*true/i); assert.match(setup, /"tpm"\s*:\s*true/i)
    report.installed = (await shell(saved.source, '(Get-CimInstance Win32_OperatingSystem).Version; Get-NetAdapter | Format-Table Name, InterfaceDescription, Status; Get-Service qemu-ga, sshd')).slice(-3000)
    saved.installationVerified = true; await checkpoint(); await page.screenshot({ path: 'data/windows-installation.png' })
  })
  await step('Sysprep 封装、正常关机并固化独立 Windows 模板', async () => {
    if (saved.template) { await complete(saved.template.operationId); saved.template = (await api('/templates?ids=' + saved.template.id))[0]; assert.equal(saved.template.state, 'ready'); return }
    const xml = (await readFile(new URL('./fixtures/windows-unattend.xml', import.meta.url), 'utf8')).replaceAll('@@PASSWORD@@', saved.password).replace(/<settings pass="windowsPE">[\s\S]*?<\/settings>/, '').replace(/<LocalAccounts>[\s\S]*?<\/LocalAccounts>/, '').replace(/<AutoLogon>[\s\S]*?<\/AutoLogon>/, '').replace(/<FirstLogonCommands>[\s\S]*?<\/FirstLogonCommands>/, '')
    if ((await api('/environments/' + saved.source.id + '/state')).assets[0].state !== 'stopped') {
      await content(saved.source, '/C:/ProgramData/NetlabTests/seal.xml', Buffer.from(xml))
      await shell(saved.source, "Stop-Service cloudbase-init; Get-ChildItem -LiteralPath 'C:\\Program Files\\Cloudbase Solutions\\Cloudbase-Init\\log' -Filter cloudbase-init.log | Remove-Item; Remove-Item -Path C:\\ProgramData\\ssh\\ssh_host_*; $p=Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine='C:\\Windows\\System32\\Sysprep\\Sysprep.exe /quiet /generalize /oobe /shutdown /unattend:C:\\ProgramData\\NetlabTests\\seal.xml'}; if ($p.ReturnValue -ne 0) { throw ('Sysprep launch failed: '+$p.ReturnValue) }")
    }
    const deadline = Date.now() + 300_000
    while (Date.now() < deadline) { if ((await api('/environments/' + saved.source.id + '/state')).assets[0].state === 'stopped') break; await delay(1000) }
    assert.equal((await api('/environments/' + saved.source.id + '/state')).assets[0].state, 'stopped', 'Sysprep did not finish shutdown')
    const source = await api('/environments/' + saved.source.id)
    saved.template = await api(assetPath(saved.source) + '/templates', 'POST', { name: 'Windows Server 2025 guest verification', expectedRevision: source.revision, initialization: 'cloudbase-init' }); await checkpoint(); await complete(saved.template.operationId)
    saved.template = (await api('/templates?ids=' + saved.template.id))[0]; assert.equal(saved.template.state, 'ready'); await checkpoint()
  })
  await step('两个同 CIDR 独立 Windows 环境，来宾初始化与 SFTP 文件操作', async () => {
    if (saved.guestsVerified) { report.reusedGuests = true; report.guests = saved.environments.filter(env => env.label).map(env => ({ label: env.label, facts: env.facts })); return }
    for (const label of ['A', 'B']) {
      let env = saved.environments.find(env => env.label === label)
      if (!env) env = await create('Windows ' + label, saved.template, { hostname: 'win-' + label.toLowerCase(), username: 'netlab' })
      env.label = label; await checkpoint()
      await connectSSH(env, 240)
      const facts = await shell(env, "$env:COMPUTERNAME; Get-NetIPAddress -AddressFamily IPv4 | Where-Object { $_.IPAddress -like '10.94.0.*' } | Format-Table IPAddress | Out-String; (Get-CimInstance Win32_UserAccount -Filter \"LocalAccount=True AND Name='netlab'\").SID; 'NETLAB_UTC:'+([DateTimeOffset]::UtcNow.ToUnixTimeSeconds())")
      assert.match(facts, new RegExp('win-' + label, 'i')); assert.match(facts, /10\.94\.0\./)
      env.sid = facts.match(/S-1-5-21-\d+-\d+-\d+-\d+/)?.[0]; assert.ok(env.sid, 'Windows local user SID was not returned')
      env.clockSkewSeconds = Math.abs(Number(facts.match(/NETLAB_UTC:(\d+)/)?.[1]) - Math.floor(Date.now()/1000)); assert.ok(env.clockSkewSeconds < 10, 'Windows clock depends on the host timezone')
      env.facts = facts.slice(-2500); await content(env, '/C:/Users/netlab/netlab-file.txt', Buffer.from(label))
      assert.equal(Buffer.from(await content(env, '/C:/Users/netlab/netlab-file.txt')).toString(), label)
      await content(env, '/C:/Users/netlab/netlab-file.txt', Buffer.from(label + '-updated'))
      assert.equal(Buffer.from(await content(env, '/C:/Users/netlab/netlab-file.txt')).toString(), label + '-updated')
      const folder = '/C:/Users/netlab/test-files-' + randomUUID()
      await api(assetPath(env) + '/files?path=' + folder, 'POST', { action: 'mkdir' })
      await api(assetPath(env) + '/files?path=/C:/Users/netlab/netlab-file.txt', 'POST', { action: 'rename', destination: folder + '/中文.txt' })
      assert.equal((await api(assetPath(env) + '/files?path=' + folder))[0].name, '中文.txt')
      await api(assetPath(env) + '/files?path=' + folder, 'POST', { action: 'remove' })
    }
    assert.notEqual(saved.environments.find(env => env.label === 'A').hostKey, saved.environments.find(env => env.label === 'B').hostKey)
    assert.notEqual(saved.environments.find(env => env.label === 'A').sid, saved.environments.find(env => env.label === 'B').sid)
    report.guests = saved.environments.filter(env => env.label).map(env => ({ label: env.label, facts: env.facts }))
    saved.guestsVerified = true; await checkpoint()
  })
  await step('首次连接设置、真实 Windows RDP 输入与桌面尺寸收敛', async () => {
    for (const env of saved.environments.filter(env => env.label)) {
      if (env.rdpVerified) { (report.reusedDesktops ??= []).push(env.label); (report.dimensions ??= []).push(...env.desktopDimensions); continue }
      await page.setViewportSize({ width: 1366, height: 900 })
      sentKeys = []
      const instanceId = (await api('/environments/' + env.id + '/state')).assets.find(asset => asset.assetId === env.asset.id).instanceId
      const eventLog = 'Microsoft-Windows-TerminalServices-LocalSessionManager/Operational'
      const previousEvent = Number(await localGuestPowerShell(instanceId, "(Get-WinEvent -LogName '" + eventLog + "' -MaxEvents 1).RecordId"))
      await api(assetPath(env) + '/rdp', 'DELETE')
      await open(env, 'rdp')
      const dialog = page.getByRole('dialog')
      await dialog.getByRole('textbox', { name: /^用户名/ }).fill('netlab'); await dialog.getByLabel('密码', { exact: true }).fill(saved.password)
      await dialog.getByRole('button', { name: '读取服务器证书', exact: true }).click()
      await dialog.getByRole('button', { name: '信任并保存', exact: true }).click()
      await page.getByRole('tabpanel').getByRole('status').filter({ hasText: '已连接' }).waitFor({ timeout: 45_000 })
      ;(report.nativeSessions ??= []).push({ environment: env.label, readyAt: await localGuestPowerShell(instanceId, "$deadline=(Get-Date).AddSeconds(45); do { $event=Get-WinEvent -LogName '" + eventLog + "' -MaxEvents 20 | Where-Object { $_.RecordId -gt " + previousEvent + " -and $_.Id -in @(22,25) } | Select-Object -First 1; if ($event) { $shell=Get-Process explorer | Where-Object { $_.SessionId -gt 0 } | Select-Object -First 1; if (!$shell.WaitForInputIdle(30000)) { throw 'Windows desktop shell did not become idle' }; $event.TimeCreated.ToUniversalTime().ToString('o'); exit }; Start-Sleep -Milliseconds 250 } while ((Get-Date) -lt $deadline); throw 'Windows RDP session did not complete login or reconnect'") })
      const screen = page.getByLabel('远程桌面画面', { exact: true })
      await screen.evaluate(element => new Promise((resolve, reject) => {
        const deadline = Date.now() + 60_000
        const inspect = () => {
          const canvas = element.querySelector('canvas'), width = canvas.parentElement.clientWidth, height = canvas.parentElement.clientHeight
          if (width && height) {
            const pixels = canvas.getContext('2d').getImageData(0, 0, width, height).data
            for (let y = 0; y < 10; y++) for (let x = 0; x < 10; x++) {
              const offset = (Math.floor(height*(y+.5)/10)*width + Math.floor(width*(x+.5)/10))*4
              if (pixels[offset+3] && Math.max(pixels[offset],pixels[offset+1],pixels[offset+2]) > 80) { resolve(); return }
            }
          }
          if (Date.now() >= deadline) reject(new Error('Windows RDP desktop remained blank'))
          else setTimeout(inspect, 250)
        }
        inspect()
      }))
      await screen.click()
      const beforeRun = await screen.locator('canvas').first().evaluate(canvas => canvas.toDataURL())
      await page.keyboard.press('Meta+r', { delay: 100 })
      await page.waitForFunction(before => document.querySelector('[aria-label="远程桌面画面"] canvas').toDataURL() !== before, beforeRun, { timeout: 15_000 })
      report.inputFocus = await page.evaluate(() => ({ tag: document.activeElement.tagName, label: document.activeElement.getAttribute('aria-label') }))
      await page.keyboard.type('cmd /c echo ' + env.label + ' > C:\\Users\\netlab\\rdp-proof.txt', { delay: 50 }); await page.keyboard.press('Enter', { delay: 100 })
      const deadline = Date.now() + 15_000
      let proof, fileError
      while (Date.now() < deadline) { const response = await fetch(base + '/api/v1' + assetPath(env) + '/files/content?path=/C:/Users/netlab/rdp-proof.txt', { headers: { Cookie: cookie } }); if (response.ok) { proof = (await response.text()).trim(); break }; fileError = response.status + ' ' + await response.text(); await delay(500) }
      report.keyboard = { first: sentKeys.slice(0, 16), last: sentKeys.slice(-16), count: sentKeys.length }
      assert.equal(proof, env.label, 'Windows RDP keyboard did not execute in the selected guest: ' + fileError)
      for (const width of [390, 1366, 1920, 2560]) {
        await page.setViewportSize({ width, height: width === 390 ? 844 : 900 })
        const expected = await screen.evaluate(element => ({ width: Math.max(320, element.parentElement.clientWidth), height: Math.max(200, element.parentElement.clientHeight) }))
        const deadline = Date.now() + 20_000
        while (Date.now() < deadline && JSON.stringify(desktopSizes.get(env.asset.id)) !== JSON.stringify(expected)) await delay(200)
        assert.deepEqual(desktopSizes.get(env.asset.id), expected, 'remote Windows desktop did not resize at ' + width)
        ;(report.dimensions ??= []).push({ environment: env.label, viewport: width, desktop: desktopSizes.get(env.asset.id) })
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'page overflow at ' + width)
      }
      env.rdpVerified = true; env.desktopDimensions = report.dimensions.filter(item => item.environment === env.label); await checkpoint()
    }
    await page.setViewportSize({ width: 1366, height: 900 }); await page.screenshot({ path: 'data/windows-desktop.png' })
  })
  await step('销毁测试实例，释放容量与移除安装制品', async () => {
    for (const env of saved.environments) { const item = await api('/environments/' + env.id); if (item.status !== 'destroyed') await complete((await api('/environments/' + env.id + '/actions', 'POST', { action: 'destroy' })).id); assert.equal((await api('/environments/' + env.id + '/state')).assets.length, 0) }
    await complete((await api('/templates/' + saved.installer.id, 'DELETE')).id)
    report.nodes = (await api('/nodes')).map(node => ({ name: node.name, reserved: node.reserved })); for (const node of report.nodes) assert.deepEqual(node.reserved, { cpu: 0, memoryMiB: 0, diskGiB: 0 })
    report.reusableTemplate = saved.template.id
  })
} catch (error) {
  report.error = clean(error); process.exitCode = 1
  if (page) try { await page.screenshot({ path: 'data/windows-failure.png' }) } catch (error) { report.cleanupErrors.push(clean(error)) }
  report.retainedEnvironments = saved.environments.map(env => ({ id: env.id, assetId: env.asset.id, label: env.label }))
} finally {
  await browser?.close(); await checkpoint(); report.finishedAt = new Date().toISOString()
  await writeFile('data/windows-guest-result.json', JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ passed: report.steps.filter(item => item.passed).length, total: report.steps.length, error: report.error, cleanupErrors: report.cleanupErrors }))
}
