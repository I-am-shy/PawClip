// 草稿正文的 Markdown ⇄ contenteditable HTML 双向转换。
//
// # 为什么自研而不是引库
//
// docs/DESIGN.md §14 D 要求前端产物 gzip 后 < 150 KB，实测基线已经占掉 74.8 KB。
// 常见的富文本编辑器（TipTap 60–90 KB、Quill 43 KB）会把剩下的预算吃掉大半，
// 而草稿本要的能力只有四样：**粗体 / 斜体 / 下划线 / 超链接**，外加贴图。
// 这恰好是 contenteditable + 自研转换最可靠的那个窄集，所以不引库。
//
// # 存储格式（本模块定义的方言，不是完整的 CommonMark）
//
//   `**粗**`          <strong>
//   `*斜*`            <em>            （只认星号；`_` 一律当字面量，见下）
//   `<u>下划线</u>`   <u>             （Markdown 没有下划线，走内联 HTML）
//   `[文字](url)`     <a>
//   `![alt](url)`     <img>
//   段落              空行分隔
//   换行              段落内的单个 \n（对应 <br>）
//
// 三处刻意的取舍：
//
//  1. **不认 `_斜_`。** 只认 `*`。写入时 `_` 会被转义成 `\_`，读入时又是字面量，
//     于是 `snake_case_name` 这种正文不会被吃掉一半——而"注释里的下划线变成
//     斜体"正是 Markdown 最常见的意外。少一种写法换来的是零歧义。
//  2. **不认 `#` / `-` / `>` 这类块级语法。** 草稿是随手记的，用户打一个
//     `# 标题` 时想要的多半是那个井号本身。整块语法只保留"空行分段"。
//  3. **转义集固定为 `\ * _ [ ] <`。** 前五个是"不转义就会被自己解错"，
//     `<` 是"不转义就会和 `<u>` 撞车"。其余字符（含 `(` `)` `#` `-`）一律
//     字面量——转义它们只会让存储的正文变得难读。
//
// # 与 Go 侧的契约（这条最容易漏）
//
// `store/drafts.go` 的 `rewriteMDRefs` 用**文本扫描**找 `](url)`，不是 Markdown
// 解析器。它的两条前提必须由本模块兑现，否则孤儿回收会误判：
//
//   · 正文里字面出现的 `]` 必须写成 `\]`——否则用户随手打一个 `](` 就能让
//     一段普通文字被当成图片引用（那会让一个本该回收的 blob 永久留下来）；
//   · URL 里不能有空白与 `(` `)`——所以 `encodeUrl` 把它们百分号编码。
//     两边约定成对，缺任何一边都会静默失效，test/check-md.mjs 里有用例盯着。

/** 写入时要转义的字符。见文件头第 3 条。 */
const ESCAPABLE = '\\*_[]<'

/** 允许出现在超链接里的协议。白名单，不是黑名单。 */
const SAFE_SCHEMES = ['http:', 'https:', 'mailto:']

/** 草稿贴图的 URL 前缀，与 Go 侧 store.DraftBlobPrefix 一致。 */
export const BLOB_PREFIX = 'blob/'

// ── 文本 ⇄ 字面量 ────────────────────────────────────────────────

/**
 * escapeText 把一段纯文本变成 md 里的字面量。
 *
 * `\u00A0` 换成普通空格：contenteditable 在行尾、连续空格处会自动插入不换行
 * 空格，原样存下来会让"看起来一样的两段文字"在字节上不同，压缩与查重都会失真。
 */
export function escapeText(s: string): string {
  let out = ''
  for (const ch of s) {
    if (ch === '\u00A0') {
      out += ' '
    } else if (ESCAPABLE.includes(ch)) {
      out += '\\' + ch
    } else {
      out += ch
    }
  }
  return out
}

/** htmlEsc 把文本编成 HTML 字面量（只用于我们生成标签之间的文本）。 */
function htmlEsc(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
}

// ── URL 安全 ────────────────────────────────────────────────────

/**
 * encodeUrl 把 URL 里会破坏文本扫描的字符百分号编码。
 *
 * 空白会让 Go 侧的 `mdURLLen` 提前截断；`(` `)` 更是扫描器的终止符。
 * 不编码的话，一个含括号的维基链接会被截成两半——正文看着没变，
 * 但引用集合算错，孤儿扫描会据此删错文件。
 */
export function encodeUrl(raw: string): string {
  const s = raw.trim()
  let out = ''
  for (const ch of s) {
    if (ch === ' ') out += '%20'
    else if (ch === '\t') out += '%09'
    else if (ch === '\n' || ch === '\r') out += ''
    else if (ch === '(') out += '%28'
    else if (ch === ')') out += '%29'
    else if (ch === '\\') out += '%5C'
    else if (ch < ' ') out += ''
    else out += ch
  }
  return out
}

/**
 * safeHref 筛掉危险协议的链接，返回空串表示"这条链接不可信"。
 *
 * 为什么必须有这道：草稿的 md 会被本模块重新渲染成 `<a href>`，而渲染结果
 * 是在 WebView 里解析的——WebView 能调 Wails 绑定。于是一段粘进来的
 * `[点我](javascript:…)` 就成了一条从"用户内容"到"应用能力"的通道。
 * 用白名单而不是黑名单：`JaVaScRiPt:`、`java\tscript:` 这类变体在黑名单下
 * 总能绕过，而在白名单下"不匹配任何已知安全协议"就是拒绝。
 *
 * `blob/` 单独放行：它是草稿自己的贴图，相对 URL，没有协议。
 */
export function safeHref(raw: string): string {
  const s = raw.trim()
  if (s === '') return ''
  if (s.startsWith(BLOB_PREFIX)) return s
  // 纯锚点（`#section`）在草稿里没有去处，一并拒绝：
  // 放行它只会得到一个点了没反应的链接。
  const lower = s.toLowerCase()
  for (const sc of SAFE_SCHEMES) {
    if (lower.startsWith(sc)) return s
  }
  return ''
}

/**
 * safeImgSrc 只放行草稿自己的 blob 图片。
 *
 * 不放行 http(s)：那会让"打开草稿"变成"向某个服务器发出请求"，
 * 而草稿是本地笔记，用户不会预期打开它就联网。粘进来的网图由贴图
 * 通路落成本地 blob 之后再引用。
 */
export function safeImgSrc(raw: string): string {
  const s = raw.trim()
  return s.startsWith(BLOB_PREFIX) ? s : ''
}

// ── md → HTML ───────────────────────────────────────────────────

/** LinkMatch 是 `[text](url)` 或 `![alt](url)` 的解析结果。 */
type LinkMatch = { text: string; url: string; end: number }

/**
 * matchLink 从 `[`（或 `!` 后的 `[`）开始解析一个链接。
 *
 * URL 的终止条件与 Go 侧 mdURLLen **逐字对应**：到第一个空白或 `(` `)` 为止。
 * 两边不一致时，"前端存进去的"和"后端能认出来的"会分叉。
 */
function matchLink(src: string, open: number): LinkMatch | null {
  if (src[open] !== '[') return null
  let i = open + 1
  let depth = 0
  // 找配对的 `]`，尊重 `\[` `\]` 转义。
  for (; i < src.length; i++) {
    const c = src[i]
    if (c === '\\') {
      i++
      continue
    }
    if (c === '[') depth++
    else if (c === ']') {
      if (depth === 0) break
      depth--
    }
  }
  if (i >= src.length) return null
  const text = src.slice(open + 1, i)
  if (src[i + 1] !== '(') return null
  const urlStart = i + 2
  let j = urlStart
  for (; j < src.length; j++) {
    const c = src[j]
    if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === '(' || c === ')') break
  }
  if (src[j] !== ')') return null
  return { text, url: src.slice(urlStart, j), end: j + 1 }
}

/** findUnescaped 找 open 之后第一个未被转义的 `needle`，找不到返回 -1。 */
function findUnescaped(src: string, from: number, needle: string): number {
  for (let i = from; i <= src.length - needle.length; i++) {
    if (src[i] === '\\') {
      i++
      continue
    }
    if (src.startsWith(needle, i)) return i
  }
  return -1
}

/**
 * mdInline 把一行 md 渲染成 HTML 片段。
 *
 * 顺序即优先级：转义 → 粗体 → 斜体 → 下划线 → 图片 → 链接。
 * 转义排在最前，所以 `\*` 永远是一个星号，不会被当成粗体的开头。
 */
export function mdInline(src: string): string {
  let out = ''
  let i = 0
  while (i < src.length) {
    const c = src[i]

    if (c === '\\') {
      // 转义的下一字符一律字面量。末尾光杆反斜杠按字面量处理（用户真的打了它）。
      if (i + 1 < src.length) {
        out += htmlEsc(src[i + 1])
        i += 2
      } else {
        out += '\\'
        i++
      }
      continue
    }

    if (src.startsWith('**', i)) {
      const end = findUnescaped(src, i + 2, '**')
      if (end > i + 2) {
        out += '<strong>' + mdInline(src.slice(i + 2, end)) + '</strong>'
        i = end + 2
        continue
      }
    }

    if (c === '*') {
      const end = findUnescaped(src, i + 1, '*')
      if (end > i + 1) {
        out += '<em>' + mdInline(src.slice(i + 1, end)) + '</em>'
        i = end + 1
        continue
      }
    }

    if (c === '<' && src.slice(i, i + 3).toLowerCase() === '<u>') {
      const close = src.toLowerCase().indexOf('</u>', i + 3)
      if (close > i + 3) {
        out += '<u>' + mdInline(src.slice(i + 3, close)) + '</u>'
        i = close + 4
        continue
      }
    }

    if (c === '!') {
      const m = matchLink(src, i + 1)
      if (m) {
        const src2 = safeImgSrc(m.url)
        // 源不安全时整段丢掉而不是渲染成破图：正文里留一个必然加载失败的
        // <img> 只会显示成一个裂图标，比"这里什么都没有"更让人困惑。
        if (src2) out += `<img src="${htmlEsc(src2)}" alt="${htmlEsc(m.text)}">`
        i = m.end
        continue
      }
    }

    if (c === '[') {
      const m = matchLink(src, i)
      if (m) {
        const href = safeHref(m.url)
        if (href) {
          out += `<a href="${htmlEsc(href)}">${mdInline(m.text)}</a>`
        } else {
          // 不安全的目标：**保留文字、去掉链接**。整段丢掉会连用户写的
          // 字一起消失，那是内容损失；去掉链接只损失那个跳转。
          out += mdInline(m.text)
        }
        i = m.end
        continue
      }
    }

    out += htmlEsc(c)
    i++
  }
  return out
}

/**
 * mdToHtml 把整篇 md 渲染成可以塞进 contenteditable 的 HTML。
 *
 * 段落模型：连续的非空行合成一个 `<p>`，之间用 `<br>`；**空行分段**。
 * 这个模型是与 htmlToMd 严格对称的——那边把每个块级元素翻译成一个空行，
 * 这边把空行翻译成新的 `<p>`。对称性不是审美问题：不对称会让"用户写好的
 * 段落间距"在第一次编辑后就消失（存的是 `\n\n`、读回来却只剩 `\n`）。
 *
 * 于是编辑行为也就定了：**回车 = 新段落**（与任何所见即所得编辑器一致），
 * 而"把一段多行文本粘进来"由粘贴处理转成段内 `<br>`，不会被拆成十几个
 * 带间距的段落。
 */
export function mdToHtml(md: string): string {
  const lines = md.replace(/\r\n?/g, '\n').split('\n')
  const blocks: string[] = []
  let buf: string[] = []
  const flush = () => {
    if (buf.length === 0) return
    const html = buf.map(mdInline).join('<br>')
    buf = []
    // 整段都被丢掉的（例如只有一条不安全图片引用）不留空的 `<p>`：
    // 空的块元素在界面上会占出一行高度，看起来像"这里有个空行"。
    if (html !== '') blocks.push('<p>' + html + '</p>')
  }
  for (const line of lines) {
    if (line.trim() === '') flush()
    else buf.push(line)
  }
  flush()
  return blocks.join('')
}

// ── HTML → md ───────────────────────────────────────────────────

/**
 * DomLike 是本模块真正用到的那部分 DOM 接口。
 *
 * 只声明用得到的四个成员，为的是让转换逻辑能被 Node 直接测
 * （test/check-md.mjs 用一个六十行的假 DOM 驱动它）——而不用为了几条断言
 * 引入 jsdom。这不是"为了测试而抽象"：这里要的确实只是"读节点树"，
 * 换成任何树的表示都不影响转换规则本身。
 */
export type DomLike = {
  nodeType: number
  nodeName: string
  childNodes: ArrayLike<DomLike>
  textContent: string | null
  getAttribute?: (name: string) => string | null
}

/** 直接被当成"一段"的块级元素。 */
const BLOCKS = new Set([
  'P',
  'DIV',
  'LI',
  'UL',
  'OL',
  'H1',
  'H2',
  'H3',
  'H4',
  'H5',
  'H6',
  'BLOCKQUOTE',
  'SECTION',
  'ARTICLE',
  'FIGURE',
  'FIGCAPTION',
  'TABLE',
  'TR',
])

const NODE_ELEMENT = 1
const NODE_TEXT = 3

export function htmlToMd(root: DomLike): string {
  const lines: string[] = []
  let cur = ''
  // inInline > 0 表示"正在为一个内联标记收集内部字符串"（粗体/斜体/下划线/链接）。
  // 这个深度是必须的：collect() 会把收集到的片段包进 `**` 之类，而那个包装
  // 只能作用在**同一行**上。所以处于内联内部时，遇到换行不能真的推一行
  // （推了就跑到包装外面去，内容会丢），而要把它记成一个 `\n` 字面量。
  let inInline = 0

  const flush = () => {
    if (inInline > 0) {
      cur += '\n'
      return
    }
    // 只 push 非空行：contenteditable 的收尾空行不该变成 md 里的空行
    // （否则每次保存都会给正文尾部多加一个换行，正文会越长越长）。
    if (cur !== '') lines.push(cur)
    cur = ''
  }

  /**
   * paragraph 收尾一个块级元素：**多推一个空行**。
   *
   * 那个空行就是段落间距。少了它，`<p>a</p><p>b</p>` 会读成 `a\nb`，
   * 再渲染回来是一个 `<p>` 里的两个 `<br>`——用户按回车分出的段落，
   * 存一次之后就并成一段了。多推的空行由 canonical 统一收敛，
   * 所以这里可以不管"连续几次收尾"会不会堆出三重换行。
   */
  const paragraph = () => {
    flush()
    if (inInline === 0) lines.push('')
  }

  const walk = (node: DomLike): void => {
    if (node.nodeType === NODE_TEXT) {
      cur += escapeText(node.textContent ?? '')
      return
    }
    if (node.nodeType !== NODE_ELEMENT) return

    const name = node.nodeName.toUpperCase()
    switch (name) {
      case 'BR':
        // 段内换行：写成 `\n`，读回时又是 `<br>`（见 mdToHtml 的段落模型）。
        flush()
        return
      case 'SCRIPT':
      case 'STYLE':
        return
      // 三个内联标记：**x** / *x* / <u>x</u>。
      case 'STRONG':
      case 'B':
        cur += wrap('**', node)
        return
      case 'EM':
      case 'I':
        cur += wrap('*', node)
        return
      case 'U':
        cur += wrap('<u>', node, '</u>')
        return
      case 'A': {
        const href = safeHref(node.getAttribute?.('href') ?? '')
        const inner = collect(node)
        if (href === '') {
          // 目标不可信 → 只留文字。与 mdToHtml 里那条对称。
          cur += inner
        } else if (inner === '') {
          // 空文字的链接在 HTML 里点不到，读回后就彻底消失了。
          // 写成裸 URL，至少用户还能看见它。
          cur += '[' + escapeText(href) + '](' + encodeUrl(href) + ')'
        } else {
          cur += '[' + inner + '](' + encodeUrl(href) + ')'
        }
        return
      }
      case 'IMG': {
        const src = safeImgSrc(node.getAttribute?.('src') ?? '')
        if (src === '') return
        const alt = node.getAttribute?.('alt') ?? ''
        cur += '![' + escapeText(alt) + '](' + encodeUrl(src) + ')'
        return
      }
      case 'CODE':
      case 'PRE':
        // 不承诺代码块语法（见文件头第 2 条），但内容一个字符都不能丢。
        cur += escapeText(node.textContent ?? '')
        return
    }

    if (BLOCKS.has(name)) {
      // 块级元素 = 一个段落：进入前收尾上一段，出来后空一行。
      paragraph()
      for (const child of Array.from(node.childNodes)) walk(child)
      paragraph()
      return
    }

    // 未知的内联元素（SPAN / FONT / 浏览器自造标签）当透明容器穿透，
    // 而不是丢掉——contenteditable 会随手造出各种 span。
    for (const child of Array.from(node.childNodes)) walk(child)
  }

  /** collect 收集一个元素内部的 md 片段（不推行，全部并进返回值）。 */
  const collect = (node: DomLike): string => {
    const saved = cur
    const savedDepth = inInline
    cur = ''
    inInline = savedDepth + 1
    for (const child of Array.from(node.childNodes)) walk(child)
    const inner = cur
    cur = saved
    inInline = savedDepth
    return inner
  }

  /**
   * wrap 给一个内联元素套上标记，**内部含换行时不套**。
   *
   * 那一条看着像特例，其实是"内容优先"：`<strong>a<br>b</strong>` 在 md 里
   * 没有无损写法（标记跨行会让本模块自己的解析器认不出来，反过来变成正文里
   * 多出字面 `**`）。所以宁可丢掉"粗体"这一层格式，也不能丢掉文字，
   * 更不能在用户正文里留下可见的星号。实际编辑中 contenteditable 也几乎
   * 不会产出这种结构。
   */
  const wrap = (open: string, node: DomLike, close?: string): string => {
    const inner = collect(node)
    if (inner === '') return ''
    if (inner.includes('\n')) return inner
    return open + inner + (close ?? open)
  }

  for (const child of Array.from(root.childNodes)) walk(child)
  flush()

  return canonical(lines.join('\n'))
}

/**
 * canonical 把正文收敛到规范形式。
 *
 * 两件事：**连续 3 个以上换行压成 2 个**（否则"多按了几下回车"会把空白
 * 无限累积进库），以及**去掉首尾空行**。之所以要在写入前做而不是读取时做：
 * 库里存的应当是规范形式，不然"同一份内容的两种写法"会让 diff 与查重失真。
 */
export function canonical(md: string): string {
  return md.replace(/\n{3,}/g, '\n\n').replace(/^\n+/, '').replace(/\n+$/, '')
}
