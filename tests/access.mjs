import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { randomUUID } from 'node:crypto'

const { chromium } = createRequire(new URL('../web/package.json', import.meta.url))('@playwright/test')

export async function verifyAccess(base, environment, otherEnvironment, api) {
  const secondary = process.env.NETLAB_TEST_SECONDARY_URL || 'http://127.0.0.1:8091'
  const asset = environment.spec.assets.find(item => item.name === 'web')
  const principals = []
  const browser = await chromium.launch({ channel: 'msedge', headless: true })
  const issue = async expiresAt => {
    const issued = await api('/service-tokens', 'POST', {
      name: `access-${randomUUID()}`, expiresAt,
      grants: [{ scopeKind: 'asset', scopeId: `${environment.id}/${asset.id}`, permissions: ['read', 'session'] }],
    })
    principals.push(issued.principal.id)
    return issued
  }
  const restricted = (token, path, options) => fetch(`${base}/api/v1${path}`, {
    ...options, headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
  })
  const connect = async token => {
    const context = await browser.newContext()
    await context.addCookies([{ name: 'netlab_session', value: token, url: secondary }])
    const page = await context.newPage()
    await page.goto(`${secondary}/environments/${environment.id}`)
    await page.getByRole('button', { name: '资产视图', exact: true }).click()
    await page.getByRole('button', { name: 'web', exact: true }).click()
    assert.equal(await page.getByRole('button', { name: 'VM', exact: true }).count(), 0)
    assert.equal(await page.getByRole('button', { name: '调整环境', exact: true }).count(), 0)
    const socketReady = page.waitForEvent('websocket', socket => socket.url().includes('kind=terminal'))
    await page.getByRole('button', { name: '终端', exact: true }).click()
    const socket = await socketReady
    await page.getByRole('tabpanel').getByRole('status').filter({ hasText: '已连接' }).waitFor()
    return { page, socket, context }
  }
  try {
    const issued = await issue()
    const own = await restricted(issued.token, `/environments/${environment.id}`)
    assert.equal(own.status, 200)
    const detail = await own.json()
    assert.deepEqual(detail.spec.assets.map(item => item.id), [asset.id])
    const state = await (await restricted(issued.token, `/environments/${environment.id}/state`)).json()
    assert.deepEqual(state.assets.map(item => item.assetId), [asset.id])
    assert.equal((await restricted(issued.token, `/environments/${otherEnvironment.id}`)).status, 403)
    assert.equal((await restricted(issued.token, `/environments/${environment.id}/assets/${environment.spec.assets.find(item => item.name === 'VM').id}/console?kind=vnc`)).status, 403)
    assert.equal((await restricted(issued.token, '/principals')).status, 403)
    assert.equal((await restricted(issued.token, `/environments/${environment.id}/actions`, { method: 'POST', body: JSON.stringify({ action: 'stop' }) })).status, 403)

    let connection = await connect(issued.token)
    let closed = connection.socket.waitForEvent('close', { timeout: 3000 })
    await api(`/service-tokens/${issued.principal.id}`, 'DELETE')
    await closed
    assert.equal((await restricted(issued.token, '/identity')).status, 401)
    await connection.context.close()

    const limited = await issue()
    connection = await connect(limited.token)
    closed = connection.socket.waitForEvent('close', { timeout: 3000 })
    const sharing = await api(`/environments/${environment.id}/grants`)
    await api(`/environments/${environment.id}/grants`, 'PUT', sharing.grants.map(grant => grant.principalId === limited.principal.id ? { ...grant, permissions: ['read'] } : grant))
    await closed
    assert.equal((await restricted(limited.token, `/environments/${environment.id}`)).status, 200)
    await connection.context.close()

    const observer = await api('/service-tokens', 'POST', {
      name: `observer-${randomUUID()}`,
      grants: [{ scopeKind: 'environment', scopeId: environment.id, permissions: ['read'] }],
    })
    principals.push(observer.principal.id)
    const events = await fetch(`${secondary}/api/v1/environments/${environment.id}/events`, {
      headers: { Authorization: `Bearer ${observer.token}` }, signal: AbortSignal.timeout(5000),
    })
    assert.equal(events.status, 200)
    const reader = events.body.getReader()
    assert.equal((await reader.read()).done, false)
    await api(`/service-tokens/${observer.principal.id}`, 'DELETE')
    while (!(await reader.read()).done) {}

    const expiring = await issue(new Date(Date.now() + 6000).toISOString())
    connection = await connect(expiring.token)
    closed = connection.socket.waitForEvent('close', { timeout: 8000 })
    await closed
    assert.equal((await restricted(expiring.token, '/identity')).status, 401)
    await connection.context.close()
  } finally {
    await browser.close()
    await Promise.all(principals.map(id => api(`/service-tokens/${id}`, 'DELETE')))
  }
}
