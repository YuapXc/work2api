// Headless smoke check: loads each route in real Chrome, captures console
// errors / page errors / failed requests, screenshots, and reports whether the
// view's root content rendered. Requires the work2api server on :8787.
import puppeteer from 'puppeteer-core'
import { mkdirSync } from 'node:fs'

const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe'
const BASE = 'http://127.0.0.1:8787'
const routes = [
  ['overview', '/#/overview', '.overview .view-header'],
  ['accounts', '/#/accounts', '.view-header'],
  ['models', '/#/models', '.view-header'],
  ['usage', '/#/usage', '.view-header'],
  ['records', '/#/records', '.view-header'],
  ['apps', '/#/apps', '.view-header'],
]

mkdirSync('shots', { recursive: true })
const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: 'new',
  args: ['--no-sandbox', '--window-size=1440,900'],
})

let anyFail = false
for (const [name, path, sel] of routes) {
  const page = await browser.newPage()
  await page.setViewport({ width: 1440, height: 900 })
  const errors = []
  page.on('console', (m) => { if (m.type() === 'error') errors.push('console: ' + m.text()) })
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message))
  page.on('requestfailed', (r) => errors.push('reqfail: ' + r.url() + ' ' + (r.failure()?.errorText || '')))
  try {
    await page.goto(BASE + path, { waitUntil: 'networkidle2', timeout: 20000 })
    await new Promise((r) => setTimeout(r, 1500)) // let async data settle
  } catch (e) {
    errors.push('goto: ' + e.message)
  }
  const rootHtmlLen = await page.evaluate(() => document.querySelector('#app')?.innerHTML.length || 0)
  const hasContent = await page.evaluate((s) => !!document.querySelector(s), sel)
  const bodyText = await page.evaluate(() => (document.body.innerText || '').slice(0, 120).replace(/\s+/g, ' '))
  const contentWidth = await page.evaluate(() => {
    const c = document.querySelector('.content')
    const first = c?.firstElementChild
    return first ? { content: c.clientWidth, view: first.getBoundingClientRect().width } : null
  })
  await page.screenshot({ path: `shots/${name}.png`, fullPage: true })
  const ok = hasContent && rootHtmlLen > 200
  if (!ok || errors.length) anyFail = true
  console.log(`\n=== ${name} (${path}) ===`)
  console.log(`  rootHtmlLen=${rootHtmlLen} hasContent(${sel})=${hasContent}`)
  console.log(`  contentWidth=${JSON.stringify(contentWidth)}`)
  console.log(`  bodyText="${bodyText}"`)
  if (errors.length) console.log('  ERRORS:\n' + errors.map((e) => '   - ' + e).join('\n'))
  else console.log('  (no console/page errors)')
  await page.close()
}
await browser.close()
console.log('\n===== ' + (anyFail ? 'SOME PAGES FAILED' : 'ALL PAGES OK') + ' =====')
process.exit(anyFail ? 1 : 0)
