# test/ —— 测试与验收

一条命令跑全部：

```bash
test/run.sh              # 格式 → 静态检查 → 单测 → 前端检查（约 45 秒）
test/run.sh --short      # 同上，跳过规模测试（约 10 秒）
test/run.sh --build --accept   # 再加：重新出包 + 对产物做真机验收（发布前的完整口径）
```

`test/run.sh --help` 是权威用法。下面讲的是"为什么这么分"。

---

## 一、这个目录里有什么

| 文件 | 是什么 | 单独怎么跑 |
|---|---|---|
| `run.sh` | 测试总入口。分层跑，最后给总表 | `test/run.sh` |
| `accept.sh` | **产物验收**：对打包好的 `.app` 做真机端到端取证（21 项） | `scripts/build.sh && test/accept.sh` |
| `demo-m1.sh` | **端到端演示**：起真 App → 真复制几样东西 → 把落库结果摊开给你看 | `scripts/build.sh && test/demo-m1.sh` |
| `check-i18n.mjs` | 前端 i18n 一致性（两份字典键集相同、没有死键） | `npm --prefix frontend run check:i18n` |

`run.sh` 里除 `--build` 外**都不需要先构建**：前三层是纯 Go、第四层是纯前端。

## 二、为什么 Go 单测不在这个目录里（重要）

**这是 Go 工具链的硬约束，不是没整理。**

Go 要求 `*_test.go` 与被测包**同目录**——连 `package foo_test` 这种"外部测试包"
也必须在同目录才能编译。把 `store/fts_test.go` 搬进 `test/` 的结果不是"换个地方
跑"，而是**编译失败**：本仓库的测试大量访问包内未导出符号（`listSQL`、
`chooseFTSPlan`、`newApp`、`msgErr`…），出了包一个都够不着。

所以 32 个测试文件仍分布在各包目录里：

```
.                       6 个    app / boot / config / i18n / messages / resources
store/                  8 个    schema / items / writer / fts / plan / pinyin / latency / testutil
clipboard/              7 个    normalize（含 html/dib/dropfiles）/ filter / filter_apply / guard
capture/                5 个    capture / acceptance / acceptance_real_darwin / latency / testutil
backup/                 2 个    backup / yaml
panel/ pinyin/ retention/ transform/   各 1 个
```

能集中到 `test/` 的只有**独立可执行的测试资产**——也就是上表那四个。
`*_test.go` 的"索引"作用改由 `run.sh` 与本文件承担：你不用记住它们在哪个目录，
跑 `test/run.sh` 就全跑了。

## 三、各层分别证明什么

| 层 | 证明什么 | 出处 |
|---|---|---|
| ① 格式 `gofmt -l` | 代码走没走 gofmt。只列不改，只读 | 全仓 `*.go` |
| ② 静态检查 `go vet -tags sqlite_fts5` | 类型/格式串/锁拷贝一类静态问题 | 全仓 |
| ③ 单元测试 | **逻辑正确**：注入假剪贴板后端，确定性、可重复 | 上表 32 个文件 |
| ③′ 规模测试（`--short` 跳过） | **指标达标**：10 万条下检索延迟、5 MB 图片落库 | `store/latency_test.go`、`capture/latency_test.go` |
| ④ 前端检查 | 两份 i18n 字典键集一致、无死键；TS 类型正确 | `test/check-i18n.mjs`、`tsc --noEmit` |
| ⑤ 产物验收（`--accept`） | **这个 `.app` 真能跑**：签名、`LSUIElement`、真剪贴板捕获、断电安全 | `test/accept.sh` |
| ⑤′ 演示（`--demo`） | 链路真的走通——给人看的，不判对错 | `test/demo-m1.sh` |

**③ 和 ⑤ 缺一不可。** 单测全绿而产物发不出去（签名坏了、`Info.plist` 丢了
`LSUIElement`、构建漏了标签）是最常见的发布事故，而它**一条单测都不会红**。
反过来，`accept.sh` 也抓不到边界条件——它只走一遍正常路径。
验收的实际结果与已知差距见 `docs/ACCEPTANCE.md`。

## 四、三条硬口径（照抄就对了）

```bash
go test -tags sqlite_fts5 -p 1 -count=1 ./...
```

1. **`-tags sqlite_fts5` 不能省。** 不带它时 `store.Open` 会**优雅降级**——
   打一行 WARN、退回 LIKE 检索、进程照常启动，一批全文检索用例变成"跳过"而不是
   变红。也就是说 FTS5 静默失效时测试仍然全绿。同一个纪律在 `scripts/build.sh`
   （构建）与 `scripts/dev.sh`（开发）里各钉了一次。
2. **`-p 1` 不能省。** 延迟类用例是墙钟测的，并行跑包会让 `capture` 的图片项
   从 110 ms 飙到 300 ms——那是"CPU 被别的包抢满"，不是产品退化。串行跑才对得上
   预算。
3. **`-count=1` 与 CI 一致。** 默认走测试缓存很快，但那意味着"绿"可能是上次的
   结论。CI 用 `-count=1`，本机也用，两边结论才可互推。

参数顺序也有讲究：构建标签必须写在**包路径之前**。`go build ./... -tags x` 不是
"传标签"，而是把 `-tags` 和 `x` 当成两个包路径解析（`malformed import path
"-tags"`）。`go test` 对包后面参数有兼容处理，所以同样的写法在测试那一步能过——
这个不对称正是最容易抄错的地方，带 `compile-check` 的 job 就是为它设的。

## 五、`accept.sh` 的三种结论

| 标记 | 含义 | 影响退出码 |
|---|---|---|
| ✅ `ok` | 通过 | 否 |
| ⚠️ `warn` | **已知差距**：有归因、已记录在 `docs/ACCEPTANCE.md` | **否** |
| ❌ `bad` | 有东西坏了 | 是 |

`warn` 目前只有一条：空闲常驻内存未达 §12 的 30 MB（实测稳态 54 MB，
`wails.Run` 之前的基线是 13 MB —— 超出的部分在 Wails/WebKit 侧，归因见
`docs/ACCEPTANCE.md`）。它不翻红是刻意的：**一条永远红的判据会让人学会无视整张表**，
那它就再也不报警了。取而代之的是一条回归线——同一项超过 80 MB 就是 `bad`
（稳态 54 MB 留约 50% 余量）。

所以 `--accept` 的退出码只跟 `bad` 走。

## 六、注意

- `accept.sh` 会**覆盖你当前的系统剪贴板**，耗时约 40 秒，其中 26 秒是静置采样
  ——那段时间别动剪贴板，否则空闲 CPU 会被量成"我们正在折腾它"。
- 所有脚本都在仓库内的临时目录里跑（`.workbuddy/tmp/pawclip-accept`），
  **不碰** `~/Library/Application Support/PawClip` 里的真实数据。
- `test/` 不参与构建，也不进发布产物。
- Windows / Linux 目前只有编译烟测，**没有真机测试**——原因见 `docs/ACCEPTANCE.md`
  的「已知遗留」。
