// 全局热键：**读键盘**与**写给人看**。
//
// 为什么单独一个模块：这两件事原来都是错的，而且是两类不同的错。
//
//  ① 设置页只是一个普通文本框，用户得自己把 "CmdOrCtrl+Shift+V" 背下来敲进去。
//     键盘就在手边，却要用户去拼字符串——所以这里补上"按一下就记下来"。
//  ② 提示文案是写死的（"搜索历史…（⌘⇧V 呼出）"），既不看设置、也不看有没有
//     热键。所以这里提供 formatCombo，让界面上的每个提示都从 ui.hotkey 现算。
//
// ⚠️ **跨语言契约**：comboFromEvent 产出的字符串会原样写进 ui.hotkey，
// 再由后端 panel.ParseHotkey 解析。两边必须认识同一套记号：
//
//	修饰键  CmdOrCtrl / Cmd / Ctrl / Alt / Shift
//	主键    A-Z · 0-9 · SPACE TAB ENTER ESC BACKSPACE DELETE
//	        UP DOWN LEFT RIGHT HOME END PAGEUP PAGEDOWN · F1-F12
//
// 这张表镜像 panel/panel.go 的 namedKeys，由根包的 TestHotkeyContract_
// FrontendTokensAreParsable 盯着（它读本文件、拿后端解析器验一遍）：
// 前端多写一个后端不认的键名，测试会红，而不是等用户按下去没反应。

/**
 * 主键的两个方向：`code`（物理键，捕获用）↔ 规范记号（存储用）。
 *
 * 用 KeyboardEvent.code 而不是 key 来捕获，是因为 code 不受键盘布局与
 * 输入法影响：中文输入法开着的时候，key 可能给出 "Process"，
 * 而 code 仍然是 "KeyV"。捕获热键必须按**物理键**来。
 *
 * ⚠️ Backspace / Delete 刻意**不在**这张表里：录制中它俩是"清除"。
 * 把清除做成一个要记忆的按钮，不如让用户按他当下最想按的那个键。
 */
const CODE_TO_TOKEN: Record<string, string> = {
  Space: 'SPACE',
  Tab: 'TAB',
  Enter: 'ENTER',
  NumpadEnter: 'ENTER',
  Escape: 'ESC',
  ArrowUp: 'UP',
  ArrowDown: 'DOWN',
  ArrowLeft: 'LEFT',
  ArrowRight: 'RIGHT',
  Home: 'HOME',
  End: 'END',
  PageUp: 'PAGEUP',
  PageDown: 'PAGEDOWN',
}

/**
 * key 的兜底映射：极老的 WebKit 上 code 可能为空。
 * 只覆盖具名键里"key 与 code 不同名"的那些，其余走 code 或单字符规则。
 */
const KEY_TO_TOKEN: Record<string, string> = {
  ' ': 'SPACE',
  Enter: 'ENTER',
  Tab: 'TAB',
  Escape: 'ESC',
  ArrowUp: 'UP',
  ArrowDown: 'DOWN',
  ArrowLeft: 'LEFT',
  ArrowRight: 'RIGHT',
  Home: 'HOME',
  End: 'END',
  PageUp: 'PAGEUP',
  PageDown: 'PAGEDOWN',
}

/** keyToken 把一个按键事件翻成规范主键名；翻不出来（修饰键、不支持的键）返回 null。 */
function keyToken(e: KeyboardEvent): string | null {
  const code = e.code ?? ''

  // 字母 / 数字 / 功能键：这三类是可以算出来的，不必进表。
  const letter = /^Key([A-Z])$/.exec(code)
  if (letter) return letter[1]
  const digit = /^Digit([0-9])$/.exec(code)
  if (digit) return digit[1]
  const fn = /^F([1-9]|1[0-2])$/.exec(code)
  if (fn) return 'F' + fn[1]
  if (CODE_TO_TOKEN[code]) return CODE_TO_TOKEN[code]
  // 小键盘数字落成同一个数字（后端只有 0-9 这一种数字键）。
  const numpad = /^Numpad([0-9])$/.exec(code)
  if (numpad) return numpad[1]

  if (KEY_TO_TOKEN[e.key]) return KEY_TO_TOKEN[e.key]
  if (/^[a-zA-Z]$/.test(e.key)) return e.key.toUpperCase()
  if (/^[0-9]$/.test(e.key)) return e.key
  return null
}

/**
 * comboFromEvent 把一次 Keydown 翻成规范组合串；还不构成一个热键时返回 null。
 *
 * 三种情况返回 null，调用方应当**继续等待**而不是报错：
 *
 *   · 只按了修饰键（⌘ 本身不是热键）；
 *   · 按了不支持的键（标点、小键盘运算符……后端在全局热键上不认识它们）；
 *   · 一个修饰键都没有 —— 后端同样拒绝（裸键的全局热键等于把那个键
 *     从整个系统吃掉，见 panel.ParseHotkey 的长注释）。
 *
 * 平台差异只体现在**修饰键的落法**上：macOS 按 ⌘ 记成 CmdOrCtrl、
 * 按 ⌃ 记成 Ctrl；Windows 按 Ctrl 记成 CmdOrCtrl、按 Win 记成 Cmd。
 * 这样一份设置跨平台都是同一个意思（DESIGN §9 的默认值就是这么写的）。
 */
export function comboFromEvent(e: KeyboardEvent, platform: string): string | null {
  const key = keyToken(e)
  if (!key) return null

  const isMac = platform === 'darwin'
  const cmdOrCtrl = isMac ? e.metaKey : e.ctrlKey
  const cmd = !isMac && e.metaKey
  const ctrl = isMac && e.ctrlKey
  const alt = e.altKey
  const shift = e.shiftKey

  if (!cmdOrCtrl && !cmd && !ctrl && !alt && !shift) return null

  // 顺序与后端 Hotkey.String() 一致（CmdOrCtrl → Cmd → Ctrl → Alt → Shift → 键），
  // 免得"同一个热键、两次录制、两种写法"。
  const parts: string[] = []
  if (cmdOrCtrl) parts.push('CmdOrCtrl')
  if (cmd) parts.push('Cmd')
  if (ctrl) parts.push('Ctrl')
  if (alt) parts.push('Alt')
  if (shift) parts.push('Shift')
  parts.push(key)
  return parts.join('+')
}

/** 录制中的"按键是否是清除/取消"判据（两个键都算，符合各家的习惯）。 */
export function isClearKey(e: KeyboardEvent): boolean {
  return e.code === 'Backspace' || e.code === 'Delete' || e.key === 'Backspace' || e.key === 'Delete'
}

/** 主键的显示名。macOS 用符号，其余平台用词。 */
const MAC_KEY_LABEL: Record<string, string> = {
  SPACE: 'Space',
  TAB: '⇥',
  ENTER: '↩',
  ESC: '⎋',
  BACKSPACE: '⌫',
  DELETE: '⌦',
  UP: '↑',
  DOWN: '↓',
  LEFT: '←',
  RIGHT: '→',
  HOME: '↖',
  END: '↘',
  PAGEUP: '⇞',
  PAGEDOWN: '⇟',
}

const OTHER_KEY_LABEL: Record<string, string> = {
  SPACE: 'Space',
  TAB: 'Tab',
  ENTER: 'Enter',
  ESC: 'Esc',
  BACKSPACE: 'Backspace',
  DELETE: 'Delete',
  UP: 'Up',
  DOWN: 'Down',
  LEFT: 'Left',
  RIGHT: 'Right',
  HOME: 'Home',
  END: 'End',
  PAGEUP: 'PgUp',
  PAGEDOWN: 'PgDn',
}

/**
 * formatCombo 把规范组合串渲染成界面上的样子。
 *
 * macOS：⌘⇧V · ⌃⌥P · ⌘⇧↩（符号连写，系统菜单里就是这么显示的）
 * 其它：  Ctrl+Shift+V · Alt+P · Ctrl+Shift+Enter
 *
 * 空值/空白返回空串——这是"没有热键"（用户清掉了它），
 * 调用方据此换成另一句提示，而不是显示一对光秃秃的括号。
 * 认不出的记号**原样保留**：宁可让用户看到一个陌生的词，
 * 也不要因为前端不认识就把设置值显示成"没有热键"。
 */
export function formatCombo(combo: string, platform: string): string {
  const s = (combo ?? '').trim()
  if (s === '') return ''

  const isMac = platform === 'darwin'
  const keyLabels = isMac ? MAC_KEY_LABEL : OTHER_KEY_LABEL

  let out = ''
  for (const raw of s.split('+')) {
    const tok = raw.trim().toUpperCase()
    if (tok === '') continue

    switch (tok) {
      case 'CMDORCTRL':
      case 'COMMANDORCONTROL':
        out += isMac ? '⌘' : 'Ctrl+'
        continue
      case 'CMD':
      case 'COMMAND':
      case 'SUPER':
      case 'META':
        out += isMac ? '⌘' : 'Win+'
        continue
      case 'CTRL':
      case 'CONTROL':
        out += isMac ? '⌃' : 'Ctrl+'
        continue
      case 'ALT':
      case 'OPTION':
      case 'OPT':
        out += isMac ? '⌥' : 'Alt+'
        continue
      case 'SHIFT':
        out += isMac ? '⇧' : 'Shift+'
        continue
    }

    const label = keyLabels[tok]
    if (label) {
      out += label
      continue
    }
    if (tok === 'RETURN') {
      out += isMac ? '↩' : 'Enter'
      continue
    }
    if (tok === 'ESCAPE') {
      out += isMac ? '⎋' : 'Esc'
      continue
    }
    out += tok
  }
  return out
}
