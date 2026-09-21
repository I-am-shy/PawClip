// 草稿 Markdown 转换器的断言集。
//
// 为什么值得单独一层：md.ts 是**草稿内容的唯一编码器**——它写错了不会报错，
// 只会让用户的正文在某一刻悄悄变样，或者让 Go 侧的孤儿回收算错引用集合
// （那条链路的后果是"草稿里的图过了 24 小时就消失"）。这类错误没有别的
// 地方能拦住。
//
// 直接 import .ts：Node 22.18+ 默认开启类型擦除，所以不需要构建、不需要
// jsdom、不需要测试框架。代价是**不能用枚举 / 命名空间 / 参数属性**
// （擦除器不支持），md.ts 里也就刻意避开了它们。
//
// htmlToMd 走 DOM，这里给它一个很小的假 DOM。这不是"为了测试而抽象"：
// md.ts 里真正用到的只有"读一棵节点树"这一件事，DomLike 就是那件事的形状。
// 另有一个只认"我们自己生成的 HTML"的极小解析器，用来做真正的往返断言——
// 那条断言是这一层的核心，因为它一次性盯住了两个方向的对称性。

import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

// 前置检查：本文件靠 Node 的**类型擦除**直接 import .ts。
// Node 22.18+ 默认开启；更早的版本会以一个语法错误收场，看不出真正原因，
// 所以在这里先给一句人话。CI 的 frontend job 钉的是 node-version: "22"。
const [major, minor] = process.versions.node.split('.').map(Number)
if (major < 22 || (major === 22 && minor < 18)) {
  console.error(
    `check-md: 需要 Node 22.18+（当前 ${process.versions.node}）——` +
      '本检查直接 import frontend/src/md.ts，依赖默认开启的类型擦除。'
  )
  process.exit(1)
}

const here = dirname(fileURLToPath(import.meta.url))
const modPath = join(here, '..', 'frontend', 'src', 'md.ts')

const { mdToHtml, htmlToMd, escapeText, safeHref, safeImgSrc, encodeUrl, canonical, findAutoLinks } =
  await import(modPath)

// ── 假 DOM ───────────────────────────────────────────────────────

const NODE_ELEMENT = 1
const NODE_TEXT = 3

/** tx 造一个文本节点。 */
function tx(s) {
  return { nodeType: NODE_TEXT, nodeName: '#text', childNodes: [], textContent: s }
}

/** el 造一个元素节点。attrs 用普通对象，缺省时 getAttribute 返回 null。 */
function el(name, children = [], attrs = {}) {
  const node = {
    nodeType: NODE_ELEMENT,
    nodeName: name.toUpperCase(),
    childNodes: children,
    getAttribute: (k) => (Object.prototype.hasOwnProperty.call(attrs, k) ? attrs[k] : null),
  }
  Object.defineProperty(node, 'textContent', {
    get() {
      return children.map((c) => c.textContent ?? '').join('')
    },
  })
  return node
}

/** root 造一个容器（相当于 contenteditable 那个 div）。 */
function root(...children) {
  return el('div', children)
}

// ── 极小的 HTML 解析器 ───────────────────────────────────────────
//
// 只认 mdToHtml 自己会产出的那几种标签（p / br / strong / em / u / a / img），
// 不做容错、不处理嵌套错误。这样足够做往返断言，又不必引一个解析库。

const VOID_TAGS = new Set(['br', 'img'])
const TAG_RE = /<(\/?)([a-zA-Z][a-zA-Z0-9]*)((?:\s+[a-zA-Z-]+="[^"]*")*)\s*\/?>/g
const ATTR_RE = /([a-zA-Z-]+)="([^"]*)"/g

function decodeEnt(s) {
  return s
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&amp;/g, '&')
}

function parseHtml(html) {
  const top = el('div', [])
  const stack = [top]
  let last = 0
  TAG_RE.lastIndex = 0
  let m
  while ((m = TAG_RE.exec(html)) !== null) {
    if (m.index > last) {
      const raw = html.slice(last, m.index)
      if (raw) stack[stack.length - 1].childNodes.push(tx(decodeEnt(raw)))
    }
    last = TAG_RE.lastIndex
    const closing = m[1] === '/'
    const name = m[2].toLowerCase()
    const attrs = {}
    for (const a of (m[3] || '').matchAll(ATTR_RE)) attrs[a[1].toLowerCase()] = decodeEnt(a[2])
    if (closing) {
      if (stack.length > 1) stack.pop()
    } else {
      const node = el(name, [], attrs)
      stack[stack.length - 1].childNodes.push(node)
      if (!VOID_TAGS.has(name)) stack.push(node)
    }
  }
  if (html.length > last) {
    const raw = html.slice(last)
    if (raw) stack[stack.length - 1].childNodes.push(tx(decodeEnt(raw)))
  }
  return top
}

// ── 断言器（不引框架）────────────────────────────────────────────

let failed = 0
let passed = 0

function eq(label, got, want) {
  if (got === want) {
    passed++
    return
  }
  failed++
  console.error(`  ✗ ${label}`)
  console.error(`      期望 ${JSON.stringify(want)}`)
  console.error(`      实得 ${JSON.stringify(got)}`)
}

function ok(label, cond, extra = '') {
  if (cond) {
    passed++
    return
  }
  failed++
  console.error(`  ✗ ${label}${extra ? '：' + extra : ''}`)
}

// ── md → HTML ───────────────────────────────────────────────────

console.log('md → HTML')
eq('单个段落', mdToHtml('你好'), '<p>你好</p>')
eq('段内换行变 br', mdToHtml('a\nb'), '<p>a<br>b</p>')
eq('空行分段', mdToHtml('a\n\nb'), '<p>a</p><p>b</p>')
eq('粗体', mdToHtml('**粗**'), '<p><strong>粗</strong></p>')
eq('斜体', mdToHtml('*斜*'), '<p><em>斜</em></p>')
eq('下划线走内联 HTML', mdToHtml('<u>线</u>'), '<p><u>线</u></p>')
eq('链接', mdToHtml('[t](https://a.com/b)'), '<p><a href="https://a.com/b">t</a></p>')
eq('图片', mdToHtml('![a](blob/9f/2a/x.png)'), '<p><img src="blob/9f/2a/x.png" alt="a"></p>')
eq(
  '嵌套：粗体里带链接',
  mdToHtml('**[t](https://a.com)**'),
  '<p><strong><a href="https://a.com">t</a></strong></p>'
)
eq('转义的星号是字面量', mdToHtml('\\*不斜\\*'), '<p>*不斜*</p>')
eq('HTML 元字符被转义', mdToHtml('a<b>&"c'), '<p>a&lt;b&gt;&amp;&quot;c</p>')
eq('下划线不触发斜体', mdToHtml('snake_case_name'), '<p>snake_case_name</p>')
eq('未闭合的 ** 保持字面量', mdToHtml('a ** b'), '<p>a ** b</p>')

// 安全：这几条是"用户内容 → 应用能力"的那条通道，必须挡住。
//
// 两条不同的挡法都留着，因为它们由不同的机制负责，将来改任一处都会露馅：
//  · `javascript:alert` —— URL 完整解析出来了，靠 safeHref 白名单拒绝；
//  · `javascript:alert(1)` —— URL 在 `(` 处按 Go 侧同样的规则截断，
//    于是根本不构成链接，原样当字面量输出（同样安全，但成因不同）。
eq('javascript: 链接被去链接、留文字', mdToHtml('[点我](javascript:alert)'), '<p>点我</p>')
eq(
  '含括号的 javascript: 干脆不成链接（当字面量）',
  mdToHtml('[点我](javascript:alert(1))'),
  '<p>[点我](javascript:alert(1))</p>'
)
eq('data: 链接被拒', mdToHtml('[x](data:text/html,abc)'), '<p>x</p>')
eq('远程图片整段丢掉（不留破图）', mdToHtml('![a](https://evil.com/x.png)'), '')
eq('整段被丢时不留空 <p>', mdToHtml('a\n\n![x](https://evil.com/x.png)\n\nb'), '<p>a</p><p>b</p>')

ok('safeHref 放行 blob', safeHref('blob/9f/2a/x.png') === 'blob/9f/2a/x.png')
ok('safeHref 放行 https', safeHref('https://a.com') === 'https://a.com')
ok('safeHref 放行 mailto', safeHref('mailto:a@b.com') === 'mailto:a@b.com')
ok('safeHref 大小写混写的 javascript 也拒', safeHref('JaVaScRiPt:alert(1)') === '')
ok('safeHref 前导空白的 javascript 也拒', safeHref('  javascript:alert(1)') === '')
ok('safeHref 拒绝纯锚点（草稿里没有去处）', safeHref('#sec') === '')
ok('safeImgSrc 只认 blob', safeImgSrc('http://a/x.png') === '')
ok('encodeUrl 编码括号', encodeUrl('a(b)c') === 'a%28b%29c')
ok('encodeUrl 编码空格', encodeUrl('a b') === 'a%20b')

// ── HTML → md ───────────────────────────────────────────────────

console.log('HTML → md')
eq('块级元素 = 段落', htmlToMd(root(el('div', [tx('a')]), el('div', [tx('b')]))), 'a\n\nb')
eq('br 是段内换行', htmlToMd(root(el('div', [tx('a'), el('br'), tx('b')]))), 'a\nb')
eq(
  '多推的空行由 canonical 收敛',
  htmlToMd(root(el('div', [tx('a')]), el('div', []), el('div', [tx('b')]))),
  'a\n\nb'
)
eq('strong', htmlToMd(root(el('strong', [tx('x')]))), '**x**')
eq('b 也认', htmlToMd(root(el('b', [tx('x')]))), '**x**')
eq('em', htmlToMd(root(el('em', [tx('x')]))), '*x*')
eq('i 也认', htmlToMd(root(el('i', [tx('x')]))), '*x*')
eq('u', htmlToMd(root(el('u', [tx('x')]))), '<u>x</u>')
eq('a', htmlToMd(root(el('a', [tx('t')], { href: 'https://a.com/b' }))), '[t](https://a.com/b)')
eq(
  'img',
  htmlToMd(root(el('img', [], { src: 'blob/9f/2a/x.png', alt: 'a' }))),
  '![a](blob/9f/2a/x.png)'
)
eq('未知内联元素穿透（span）', htmlToMd(root(el('span', [tx('x')]))), 'x')
eq(
  'script 整段丢掉',
  htmlToMd(root(el('div', [tx('a')]), el('script', [tx('alert(1)')]), el('div', [tx('b')]))),
  'a\n\nb'
)
eq('ul/li 各成段', htmlToMd(root(el('ul', [el('li', [tx('a')]), el('li', [tx('b')])]))), 'a\n\nb')
eq('尾部空行不写进正文', htmlToMd(root(el('div', [tx('a')]), el('div', []))), 'a')
eq('nbsp 收敛成普通空格', htmlToMd(root(el('div', [tx('a\u00A0b')]))), 'a b')

// 文字里出现 `]` 必须转义——这是与 Go 侧 rewriteMDRefs 的成对约定。
// 不成立的话，用户在正文里随手打一个 `](` 就能让一段普通文字被当成图片引用，
// 于是一个本该被回收的 blob 永久留下（或反过来：正文里的真引用被算错）。
eq(
  '字面 ] 被转义（防伪造图片引用）',
  htmlToMd(root(el('div', [tx('](blob/9f/2a/x.png)')]))),
  '\\](blob/9f/2a/x.png)'
)
eq('星号被转义', htmlToMd(root(el('div', [tx('2*3*4')]))), '2\\*3\\*4')
eq('尖括号被转义', htmlToMd(root(el('div', [tx('<u>假的</u>')]))), '\\<u>假的\\</u>')
eq('反斜杠被转义', htmlToMd(root(el('div', [tx('C:\\tmp')]))), 'C:\\\\tmp')

// 不安全的目标：链接去壳、图片丢掉，但文字不能少。
eq(
  '不安全 href 只留文字',
  htmlToMd(root(el('a', [tx('点我')], { href: 'javascript:alert(1)' }))),
  '点我'
)
eq('不安全的 img 整段不写', htmlToMd(root(el('div', [el('img', [], { src: 'http://a/x.png' })]))), '')
eq(
  '含括号的 href 被编码（否则 Go 侧会截断）',
  htmlToMd(root(el('a', [tx('t')], { href: 'https://a.com/x(y)' }))),
  '[t](https://a.com/x%28y%29)'
)

// 内联元素里含换行：宁可丢格式，不能丢文字，也不能漏出可见的标记。
eq(
  'strong 内部换行时只保留文字',
  htmlToMd(root(el('div', [el('strong', [tx('a'), el('br'), tx('b')])]))),
  'a\nb'
)

// ── 往返 ────────────────────────────────────────────────────────
//
// 这一节是最要紧的：它一次性盯住两个方向的**对称性**。
// 不对称的表现是"用户写的段落间距在第一次编辑后消失"、"粗体存一次就没了"，
// 而两个方向各自的断言都看不出来——只有来回一趟才暴露。

console.log('往返（md → HTML → DomLike → md）')
for (const md of [
  '你好',
  'a\nb',
  'a\n\nb',
  '**粗** 与 *斜* 混排',
  '<u>下划线</u>',
  '[链接](https://a.com/b)',
  '![图](blob/9f/2a/x.png)',
  '多段\n\n第二段\n\n第三段',
  '**粗**里带 [链接](https://a.com)',
  '转义：\\*literal\\* 与 \\]brace',
  'C:\\\\tmp 路径',
  '2\\*3 与 a\\<b',
]) {
  eq(`往返 ${JSON.stringify(md)}`, htmlToMd(parseHtml(mdToHtml(md))), canonical(md))
}

// ── 收敛与转义的地基 ────────────────────────────────────────────

console.log('canonical / escapeText')
eq('canonical 压缩多余空行', canonical('a\n\n\n\nb'), 'a\n\nb')
eq('canonical 去掉首尾空行', canonical('\n\na\n\n'), 'a')
eq('canonical 保留单个换行', canonical('a\nb'), 'a\nb')
eq('canonical 幂等', canonical(canonical('a\n\n\nb\n')), canonical('a\n\n\nb\n'))
eq('escapeText 转义六个字符', escapeText('\\*_[]<'), '\\\\\\*\\_\\[\\]\\<')
eq('escapeText 不转义其他标点', escapeText('()#-`>'), '()#-`>')

// ── 裸 URL 自动识别 ──────────────────────────────────────────────

console.log('findAutoLinks')
{
  const one = (label, text, want) => {
    const got = findAutoLinks(text)
    eq(
      label,
      JSON.stringify(got),
      JSON.stringify(want),
    )
  }
  one('识别 http 与 https', '看 https://a.com 和 http://b.cn/x', [
    { start: 2, end: 15, url: 'https://a.com', text: 'https://a.com' },
    { start: 18, end: 31, url: 'http://b.cn/x', text: 'http://b.cn/x' },
  ])
  one('www 补 https 前缀', '见 www.foo.bar 页', [
    { start: 2, end: 13, url: 'https://www.foo.bar', text: 'www.foo.bar' },
  ])
  one('www 后没有点不算', 'www 就是 world wide web 的缩写', [])
  one('前一个字符是字母数字不算', 'axwww.foo.bar', [])
  one('句尾标点剥掉', '链接是 https://a.com。', [
    { start: 4, end: 17, url: 'https://a.com', text: 'https://a.com' },
  ])
  one('右括号剥掉', '(https://a.com)', [
    { start: 1, end: 14, url: 'https://a.com', text: 'https://a.com' },
  ])
  one('普通文本无链接', '没有任何链接的句子。', [])
  one('空文本', '', [])
  one('多个连着', 'https://a.com https://b.com', [
    { start: 0, end: 13, url: 'https://a.com', text: 'https://a.com' },
    { start: 14, end: 27, url: 'https://b.com', text: 'https://b.com' },
  ])
  // 中文引号是常见的"从聊天工具里复制出来"的包裹符，得能剥掉。
  one('中文引号剥掉', '“https://a.com”', [
    { start: 1, end: 14, url: 'https://a.com', text: 'https://a.com' },
  ])
  // 识别出来的 URL 必须能过 safeHref（http/https 白名单）——
  // 过不了的话，编辑器里包出来的 <a> 会在保存时被剥成纯文本，
  // "自动识别"就成了只在屏幕上闪一下的假动作。
  for (const m of findAutoLinks('https://a.com/x?y=1 与 www.b.io')) {
    ok(`识别结果可作 href：${m.url}`, safeHref(m.url) !== '')
  }
  // 别把已识别的文本再喂回去时认出别的东西：text 与片段一一对应。
  {
    const m = findAutoLinks('https://a.com')[0]
    eq('片段与原文一致', 'https://a.com'.slice(m.start, m.end), m.text)
  }
}

// ── 总表 ────────────────────────────────────────────────────────

console.log('')
if (failed > 0) {
  console.error(`check-md: ${passed} 条通过，${failed} 条失败`)
  process.exit(1)
}
console.log(`check-md: ${passed} 条断言全部通过`)
