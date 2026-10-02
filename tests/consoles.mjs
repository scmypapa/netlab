import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { mkdir } from 'node:fs/promises'

const { chromium } = createRequire(new URL('../web/package.json', import.meta.url))('@playwright/test')

export async function verifyConsoles(base, cookie, environment) {
  const browser = await chromium.launch({ channel: 'msedge', headless: true })
  try {
    const context = await browser.newContext({ viewport: { width: 1366, height: 900 } })
    const separator = cookie.indexOf('=')
    await context.addCookies([{ name: cookie.slice(0, separator), value: cookie.slice(separator + 1), url: base }])
    const page = await context.newPage()
    const failures = []
    page.on('pageerror', error => failures.push(error.message))
    await page.goto(`${base}/environments/${environment.id}`)
    await page.getByRole('button', { name: '资产视图', exact: true }).click()
    await page.getByRole('button', { name: 'web', exact: true }).click()
    let output = ''
    page.on('websocket', socket => {
      if (socket.url().includes('kind=terminal')) socket.on('framereceived', frame => { output += frame.payload.toString() })
    })
    await page.getByRole('button', { name: '终端', exact: true }).click()
    await page.getByRole('tabpanel').getByRole('status').filter({ hasText: '已连接' }).waitFor()
    await page.locator('.xterm-helper-textarea').focus()
    await page.keyboard.type("printf '%s%s\\n' 'NETLAB_' 'EXECUTED'; stty size")
    await page.keyboard.press('Enter')
    const deadline = Date.now() + 10_000
    while (!output.includes('NETLAB_EXECUTED') && Date.now() < deadline) await page.waitForTimeout(100)
    assert.ok(output.includes('NETLAB_EXECUTED'), `终端命令未执行：${output}`)
    const before = output.length
    await page.setViewportSize({ width: 1920, height: 1080 })
    await page.keyboard.type('stty size')
    await page.keyboard.press('Enter')
    while (!/\r?\n\d+ \d+/.test(output.slice(before)) && Date.now() < deadline) await page.waitForTimeout(100)
    assert.match(output.slice(before), /\r?\n\d+ \d+/, '终端尺寸没有传至真实TTY')

    await page.getByRole('button', { name: 'VM', exact: true }).click()
    await page.getByRole('button', { name: '控制台', exact: true }).click()
    const vnc = page.getByRole('tabpanel')
    await vnc.getByRole('status').filter({ hasText: '已连接' }).waitFor()
    assert.equal(await vnc.locator('canvas').count(), 1, 'VNC未显示真实Framebuffer')
    await mkdir('data', { recursive: true })
    for (const width of [390, 1366, 1920, 2560]) {
      await page.setViewportSize({ width, height: width === 390 ? 844 : 1000 })
      const size = await page.evaluate(() => ({ viewport: innerWidth, content: document.documentElement.scrollWidth }))
      assert.ok(size.content <= size.viewport, `${width}宽度出现页面横向溢出`)
      if (width === 390 || width === 1366) await page.screenshot({ path: `data/console-${width}.png` })
    }
    await page.getByRole('button', { name: '结束 VM 连接', exact: true }).click()
    await page.getByRole('button', { name: '结束 web 连接', exact: true }).click()
    assert.equal(await page.getByRole('region', { name: '资产连接' }).count(), 0)
    assert.deepEqual(failures, [], '控制台页面出现运行错误')
  } finally {
    await browser.close()
  }
}
