// 前端 i18n 的两条硬约束检查。
//
// 和后端那条 TestEveryKeyIsActuallyUsed 是同一个思路的镜像：**只检查"字典
// 自己"是不够的**。字典里有一堆没人用的键、或者两份字典的键集不一致，
// 都不会让 `tsc --noEmit` 变红——类型系统看不见字符串键。
//
// 检查两件事：
//
//   1. 两份字典（zh-CN / en）的键**完全一致**。少一条就会在切到那门语言时
//      静默回退到英文（makeT 的行为），表现为"界面中英混杂"。
//   2. 每个键都至少被 `t('key')` / `t("key")` 引用过一次。
//      留着不用的键会让人以为那个位置已经做了 i18n。
//
// 用 Node 直接跑（不引测试框架）：它既是 `npm run build` 的第一道闸
// （键不一致就别出包），也是 test/run.sh 的一层，失败信息要能直接指出"哪个键"。
//
// 位置说明：本文件住在 test/，但它读的是 `../frontend/src`——因为检查对象是
// 前端源码。所以改目录结构时这两处要一起动：这里的 srcDir，以及
// frontend/package.json 的 `build` 脚本里对 ../test/check-i18n.mjs 的调用。

import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const srcDir = join(here, '..', 'frontend', 'src')

/** walk 收集 src 下全部 .ts/.tsx 文件。 */
function walk(dir) {
  const out = []
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) out.push(...walk(p))
    else if (/\.tsx?$/.test(name)) out.push(p)
  }
  return out
}

const files = walk(srcDir)
const dictPath = join(srcDir, 'i18n.ts')
const dictSrc = readFileSync(dictPath, 'utf8')

// 从两个 `const zh: Dict = {...}` / `const en: Dict = {...}` 里把键抠出来。
// 用"键名后跟冒号"的形状匹配，值里的冒号不会命中（键名不含引号）。
function keysOf(varName) {
  const re = new RegExp(`const ${varName}: Dict = \\{([\\s\\S]*?)\\n\\}`, 'm')
  const m = dictSrc.match(re)
  if (!m) {
    console.error(`check-i18n: 找不到 ${varName} 字典（结构改了？）`)
    process.exit(1)
  }
  const keys = new Set()
  for (const line of m[1].split('\n')) {
    const km = line.match(/^\s*'([^']+)':/)
    if (km) keys.add(km[1])
  }
  return keys
}

const zh = keysOf('zh')
const en = keysOf('en')
let failed = false

for (const k of zh) {
  if (!en.has(k)) {
    console.error(`check-i18n: en 缺少键 ${k}（会静默回退成英文）`)
    failed = true
  }
}
for (const k of en) {
  if (!zh.has(k)) {
    console.error(`check-i18n: zh 缺少键 ${k}`)
    failed = true
  }
}

// 引用扫描：把 i18n.ts 自己排除（那里只有字典）。
const src = files
  .filter((f) => f !== dictPath)
  .map((f) => readFileSync(f, 'utf8'))
  .join('\n')

const referenced = new Set()
for (const m of src.matchAll(/\bt\(\s*'([^']+)'/g)) referenced.add(m[1])
for (const m of src.matchAll(/\bt\(\s*"([^"]+)"/g)) referenced.add(m[1])
// 模板拼接的键（例如 `conv.err.${kind}`）无法静态解析，这里显式列出前缀，
// 让"动态键"这部分的检查范围可见，而不是悄悄跳过。
const dynamicPrefixes = ['conv.err.', 'conv.', 'kind.', 'ttl.']
const isCoveredByDynamic = (k) => dynamicPrefixes.some((p) => k.startsWith(p))

if (referenced.size === 0) {
  console.error('check-i18n: 一个 t(...) 调用都没扫到 —— 检查逻辑失效了')
  process.exit(1)
}
for (const k of zh) {
  if (referenced.has(k)) continue
  if (isCoveredByDynamic(k)) continue
  console.error(`check-i18n: 键 ${k} 没有任何 t('...') 引用（要么接上，要么删掉）`)
  failed = true
}

if (failed) process.exit(1)
console.log(`check-i18n: ${zh.size} 个键，两份字典一致、全部有引用`)
