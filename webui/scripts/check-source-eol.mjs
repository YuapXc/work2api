import { readFile, readdir } from 'node:fs/promises'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = fileURLToPath(new URL('../', import.meta.url))
const nonLF = []

async function check(file) {
  if (/\r/.test(await readFile(join(root, file), 'utf8'))) nonLF.push(file)
}

async function checkTree(dir) {
  for (const entry of await readdir(join(root, dir), { withFileTypes: true })) {
    const file = `${dir}${entry.name}`
    if (entry.isDirectory()) await checkTree(`${file}/`)
    else if (entry.isFile() && /\.(vue|[cm]?[jt]sx?|html|css|json|svg)$/i.test(entry.name)) await check(file)
  }
}

for (const dir of ['src/', 'src-portal/', 'public/']) await checkTree(dir)
for (const file of ['index.html', 'portal.html', 'vite.config.ts', 'postcss.config.js', 'tailwind.config.js']) await check(file)

if (nonLF.length) {
  console.error(`请将以下前端源码保存为 LF 后再构建：\n${nonLF.sort().join('\n')}`)
  process.exitCode = 1
}
