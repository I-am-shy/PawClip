# `.clipbak` 备份包格式规范 v1

> 用于 PawClip（喵喵贴）的完整数据导出与跨机迁移。设计目标：**任何工具都能读懂**——ZIP 容器、纯文本清单、无私有二进制编码。

---

## 1. 容器与命名

| 项 | 规定 |
|---|---|
| 扩展名 | `.clipbak` |
| 容器 | ZIP（标准格式，macOS 访达 / Windows 资源管理器双击可直接解开） |
| 压缩 | 清单用 deflate；blob 中文本类用 deflate，已压缩的二进制用 store（见 §6） |
| 文件命名 | `pawclip-backup-<scope>-<yyyyMMdd-HHmmss>.clipbak` |
| `scope` 取值 | `full` / `pinned` / `cat-<分类名>` / `range-<起>-<止>` |
| 编码 | 全部文本 UTF-8（无 BOM）；ZIP 条目名用 UTF-8 并置 UTF-8 flag |

示例：`pawclip-backup-full-20260915-114158.clipbak`

---

## 2. 内部结构

```
pawclip-backup-full-20260915-114158.clipbak
├─ README.txt                       # 格式说明（人可读，第三方工具可按此解析）
├─ blobs/
│  ├─ 9f/2a/9f2a1c...e4.png
│  ├─ 9f/2a/9f2a1c...e4.thumb.png   # 不导出，见 §5
│  └─ 3b/07/3b07d9...11.rtf
└─ manifest.json                    # 或 manifest.yaml，由导出设置决定
```

**草稿的图片与条目共用同一个 `blobs/` 目录**，不做区分：寻址规则只认内容
（sha256），同一份字节被条目和草稿同时引用时只写一次。

### 条目顺序

**`README.txt` → `blobs/**` → `manifest.*`（清单最后写入）。**

原因：导出是流式的，`stats` 里的条目数与总字节数只有全部处理完才知道。把清单放最后，既满足流式写入，也保证读到的清单一定是完整的。ZIP 的中央目录支持随机访问，导入时定位清单没有性能代价。

### 为什么不用 `tar.zst`

`tar.zst` 压缩率更好、写入更快，但需要额外工具才能查看，且 Windows 自带解压不支持。这个包的定位是"用户自己的数据",**可验证性优先于压缩率**。用户能双击打开、能看到图片文件躺在里面，比省下 8% 体积有价值得多。

---

## 3. 清单结构

### 3.1 顶层

```jsonc
{
  "format": "pawclip.backup",
  "formatVersion": 1,
  "appVersion": "0.3.1",
  "exportedAt": "2026-09-15T11:41:58+08:00",
  "platform": "macos",                  // macos | windows
  "scope": "full",
  "stats": {
    "items": 1284,
    "categories": 3,
    "tags": 2,
    "drafts": 12,
    "blobBytes": 90123456               // 未压缩总字节，用于解压炸弹防护
  },
  "settings": { /* 仅作参考，导入时默认忽略 */ },
  "categories": [ /* §3.2 */ ],
  "tags": [ /* §3.3 */ ],
  "items": [ /* §3.4 */ ],
  "drafts": [ /* §3.7 */ ]
}
```

`settings` 一律导出以便审计，但**导入默认不写入本机配置**——迁移数据不应该顺手改掉目标机器的保留策略。需要时由用户在导入向导里勾选。

### 3.2 categories

```jsonc
{
  "id": 1,                              // 源机器 ID，仅供包内交叉引用
  "name": "工作",
  "color": "#E24B4A",
  "icon": "briefcase",
  "sortOrder": 0,
  "ttlSeconds": 604800,                 // null = 跟随全局
  "rule": {                             // 可空
    "match": "any",
    "conditions": [
      { "field": "sourceAppId", "op": "startsWith", "value": "com.apple.Safari" }
    ]
  }
}
```

### 3.3 tags

```jsonc
{ "id": 1, "name": "常用", "color": "#185FA5" }
```

### 3.4 items

```jsonc
{
  "id": 1024,
  "kind": "image",                  // text | html | rtf | image | files | mixed
  "preview": "图片 1080 × 1920",
  "fingerprint": "sha256:9f2a1c...e4",
  "byteSize": 2411520,

  "text": null,                     // kind 含 text 时非空
  "html": null,                     // ≤ 64 KB 时内联，否则见 blobs
  "rtfPath": null,                  // 指向包内 blob，null 表示无

  "filePaths": null,                // kind = files 时的原始绝对路径数组

  "imageWidth": 1080,
  "imageHeight": 1920,

  "sourceAppId": "com.figma.Desktop",
  "sourceAppName": "Figma",
  "sourceUrl": null,

  "categoryId": 1,                  // 引用包内 categories[].id，null = 未分类
  "tagIds": [1],

  "pinned": true,
  "ttlSeconds": null,               // 相对的存活时长
  "expiresAt": null,                // 绝对的到期时间（ISO-8601），pinned 时为 null

  "firstSeenAt": "2026-09-15T09:12:03+08:00",
  "createdAt":   "2026-09-15T11:30:11+08:00",
  "lastUsedAt":  "2026-09-15T11:30:11+08:00",
  "useCount": 3,

  "blobs": [
    {
      "role": "image",
      "path": "blobs/9f/2a/9f2a1c...e4.png",
      "mime": "image/png",
      "bytes": 2411520,
      "sha256": "9f2a1c...e4"
    }
  ]
}
```

### 3.5 字段语义要点

| 字段 | 规定 |
|---|---|
| `id` | **仅用于包内交叉引用**（`categoryId` / `tagIds` / `blobs`）。导入方绝不能沿用，必须重映射 |
| `fingerprint` | 格式固定 `sha256:<64位小写十六进制>`，与 blob 的 `sha256` 字段值一致 |
| `pinned` | 为 `true` 时 `expiresAt` 必须为 `null`，`ttlSeconds` 被忽略 |
| 时间字段 | 一律 ISO-8601 **带时区偏移**，导入方转 UTC epoch 存储。不要导出裸 epoch，跨时区迁移会出错 |
| `ttlSeconds` | 相对时长。导入方按"从导入时刻重新起算"解释（仅在用户选择重置 TTL 时使用） |
| `expiresAt` | 绝对时刻。**默认导入语义**——避免迁移后条目凭空续命 |
| `filePaths` | 仅存路径，**不内嵌文件内容**（见 §5） |
| `sourceUrl` | 可选，浏览器来源页 |

### 3.6 YAML 变体

`manifest.yaml` 与 JSON **结构完全等价**，仅序列化格式不同，block style、2 空格缩进、不加 `---` 文档头。便于人工阅读和 git diff。

```yaml
format: pawclip.backup
formatVersion: 1
appVersion: 0.3.1
exportedAt: '2026-09-15T11:41:58+08:00'
platform: macos
scope: full
stats:
  items: 1284
  categories: 3
  tags: 2
  blobBytes: 90123456
categories:
  - id: 1
    name: 工作
    color: '#E24B4A'
    sortOrder: 0
    ttlSeconds: 604800
tags:
  - id: 1
    name: 常用
    color: '#185FA5'
items:
  - id: 1024
    kind: image
    preview: 图片 1080 × 1920
    fingerprint: 'sha256:9f2a1c...e4'
    pinned: true
    categoryId: 1
    tagIds: [1]
    createdAt: '2026-09-15T11:30:11+08:00'
    blobs:
      - role: image
        path: blobs/9f/2a/9f2a1c...e4.png
        mime: image/png
        bytes: 2411520
        sha256: 9f2a1c...e4
drafts:
  - id: 1
    title: 会议记录
    md: "会议记录\n\n- 结论一\n\n![截图](blobs/9f/2a/9f2a1c...e4.png)\n"
    createdAt: '2026-09-15T11:30:11+08:00'
    updatedAt: '2026-09-15T11:41:58+08:00'
```

导入方**自动识别**：先尝试 JSON 解析，失败则按 YAML 解析，两者都失败则报 `manifest_unreadable`。

YAML 仅建议在条目数较少时导出（导出向导在 > 5000 条时提示改用 JSON），因为 YAML 的流式发射器对超大数组不如 JSON 直接。

空集合必须写成**同一行**的 `items: []` / `drafts: []`。写成
`items:` 换行再缩进一个 `[]` 是**非法 YAML**（独立一行的 `[]` 不是合法的
节点续行），本工具自己的解析器也会拒绝它。

### 3.7 drafts

草稿本（`docs/DESIGN.md` §4.4）的笔记。与 `items` 是**两个独立的集合**，
不是条目的一种 `kind`。

```jsonc
{
  "id": 1,                              // 源机器 ID，仅供包内交叉引用
  "title": "会议记录",                   // 标题可以是空串（用户清空过输入框）
  "md": "会议记录\n\n- 结论一\n\n![截图](blobs/9f/2a/9f2a1c...e4.png)\n",
  "createdAt": "2026-09-15T11:30:11+08:00",
  "updatedAt": "2026-09-15T11:41:58+08:00"
}
```

四条规定：

| 规定 | 理由 |
|---|---|
| `md` 是**唯一真源**，Markdown 格式 | 富文本编辑器（contenteditable）只是编辑态。包里不存 HTML / JSON 副本，否则两边迟早不一致 |
| 图片引用**写在 `md` 里**，形如 `](blobs/<aa>/<bb>/<sha256>.<ext>)`，**没有** item 那样的 `blobs[]` 数组 | 库里同样只有一份真源。导出时把引用抽出来另列一份，导入时就有了两个真源。第三方工具按 `](…)` 扫一遍即可 |
| 前两个 `id` / `title` 之外只有时间字段 | 草稿没有指纹、没有标签、没有分类、没有 TTL：它们既不参与去重也不参与淘汰 |
| **已归档（软删除）的草稿不在包内** | 与回收站条目同一条规则（§5）：已经是待删数据 |

导入语义（见 §8.4）：草稿**没有指纹**，因此不存在"重复"这一说——
每一次导入都是**新增**，`conflictPolicy` 对它一律不生效。导入方还必须：

- 把正文里的 `blobs/<rel>` 改写回自己那一侧的引用写法；
- 引用到的图片**不在包里或 sha256 校验不通过**时，把那一处引用从正文中
  移除（而不是留一个必然破图的 URL）；
- 给导入的草稿**不分配源机器的编号**（源机器的"草稿 3"对本机没有意义，
  留着会与本机编号撞名）。

---

## 4. 内容形态决策表

| kind | 文本内联 | blob | 说明 |
|---|---|---|---|
| `text` | `text` 字段，**永不外置** | — | 纯文本内联最好读 |
| `html` | ≤ 64 KB 内联到 `html` 字段 | > 64 KB 走 `blobs/*.html` | |
| `rtf` | — | 总是 `blobs/*.rtf`，`role: "rtf"` | RTF 含控制码，不宜内联 |
| `image` | — | 总是 `blobs/*.png`，`role: "image"` | **一律转 PNG 存储**，跨平台无歧义 |
| `files` | `filePaths` 数组 | 默认无 | 见 §5 |
| `mixed` | 按上述各自规则 | 多个 | 例如同时有 text + html + image |

常量 `INLINE_MAX_BYTES = 65536`。

---

## 5. 三类数据不导出（或需显式开启）

| 数据 | 默认行为 | 原因 |
|---|---|---|
| **缩略图** `*.thumb.png` | **不导出** | 派生数据，导入后自动重新生成。导出会让包体积白涨约 15% |
| **文件类条目内容** | 只导出 `filePaths` 路径字符串 | 用户复制的是"文件的引用"而不是文件本身。内嵌可能瞬间产生几十 GB 的包；且路径迁移后本来就会失效 |
| **回收站内容** `deleted_at IS NOT NULL` | **不导出** | 已经是待删数据 |
| **已归档的草稿** `archived_at IS NOT NULL` | **不导出** | 同上：草稿的软删除就是"待删" |

范围（`scope`）与草稿的关系：**只有 `scope = full` 才带草稿**。
`pinned` / `category` / `range` 选的是**条目**的子集，而草稿不可置顶、
不属于分类、也没有 TTL，跟这三个条件正交——硬塞进去等于无视用户在导出
向导里选的范围。此时清单里写出的是 `"drafts": []`（键存在、数组为空），
让读取方分得清"这次导出没带草稿"和"这个字段不存在"。

文件类条目的可选开关：`backup.embedFiles`（默认 `false`）。开启后仅内嵌单个文件 ≤ `backup.embedFileMaxBytes`（默认 50 MB）的内容，且向导必须明确提示"包体积可能显著增大"。

导入时 `filePaths` 指向的文件在目标机器上不存在时，条目正常恢复，但标记为"引用已失效"，UI 上以灰色路径展示，允许用户重新指定。

---

## 6. 压缩策略

| 内容 | 方法 | 理由 |
|---|---|---|
| `manifest.json` / `.yaml` | deflate level 6 | 纯文本，压缩率高 |
| `README.txt` | deflate level 6 | 同上 |
| `.rtf` / `.html` blob | deflate level 6 | 文本类，且常含大量重复标签 |
| `.png` / `.jpg` / `.webp` blob | **store** | 已压缩格式，deflate 只能省下 0–2%，纯浪费 CPU |

判断依据用 blob 的 `mime` 前缀：`image/*` 且非 `image/bmp`/`image/tiff`（未压缩位图）走 store，其余走 deflate。

---

## 7. 导出流程

```
1. 置位 export_in_progress 运行时标志 → GC 线程本轮跳过，防止 blob 被并发删除
2. BEGIN DEFERRED 只读事务 → 拿到稳定的数据快照
3. 打开目标文件，创建 ZipWriter
4. 写 README.txt
5. 按 §6 规则写 blobs/**（分块读写，块大小 64 KB）——
   条目引用的与草稿正文里引用的走同一份去重表，同一份字节只写一次
6. 写 manifest.*（流式：先写头部与 categories/tags，再逐条追加 items，最后追加 drafts）
7. 写 ZIP 中央目录，fsync，关闭
8. 提交事务，清除 export_in_progress
9. 若统计到「引用的 blob 文件缺失」，在结果里报告警告条数
```

**内存占用与条目数无关（O(1)）**：任一时刻内存中只有一条 item / draft 记录
+ 一个 64 KB 缓冲块。`drafts` 也必须流式写——草稿的正文是用户手写的、
单条就可能很大，攒成切片是真的会吃掉内存。

### 流式 JSON 写法（Go）

```go
w.WriteString(`{"format":"pawclip.backup","formatVersion":1,`)
w.WriteString(`"categories":[…],"tags":[…],"items":[`)
for i := 0; rows.Next(); i++ {   // rows 逐行扫描，不 collect 进切片
    if i > 0 {
        w.WriteString(",")        // 先写分隔符再写内容
    }
    b, _ := json.Marshal(row)
    w.Write(b)
}
w.WriteString(`],"drafts":[`)    // 第二个集合：先收 items 的方括号，不闭合对象
for i := drafts.Next(); i++ {   // 同样是流式
    if i > 0 {
        w.WriteString(",")
    }
    b, _ := json.Marshal(draft)
    w.Write(b)
}
w.WriteString("]}")              // 到这一行清单才完整
```

**先写分隔符、再写内容**（第一条不加逗号），这样不会产生尾随逗号——不要写成"每条写完追加逗号"，那样最后一条会多一个逗号导致 JSON 非法。

YAML 出口同理，但要注意 YAML 的序列缩进与块标量转义，发射器需自己处理（不引第三方 YAML 库，见 §6）。YAML 里两个集合是顶层的两个键，且**键名要惰性写出**：空集合必须写成同一行的 `items: []`，而"先写 `items:`、发现是空再补一个缩进的 `[]`"会产出非法 YAML（见 §3.6）。

### 导出选项

| 选项 | 默认 | 说明 |
|---|---|---|
| `scope` | `full` | `full` / `pinned` / 指定分类 / 时间范围 / 指定类型 |
| `manifestFormat` | `json` | `json` / `yaml` |
| `includeExpired` | `false` | 是否包含已过期未清理的条目 |
| `embedFiles` | `false` | 是否内嵌文件类条目的内容 |
| `excludeKinds` | `[]` | 例如排除 `image` 得到纯文本包（体积极小，适合日常备份） |

---

## 8. 导入流程

### 8.1 阶段一：预检（不写库）

```
1. 打开 ZIP，定位 manifest.*；缺失 → 中止，报 manifest_missing
2. 解析（JSON → YAML 回退）；两者都失败 → 中止，报 manifest_unreadable
3. 校验 format == "pawclip.backup"；不符 → 中止，报 not_a_pawclip_backup
4. 校验 formatVersion：
     == 本机支持版本      → 正常导入
     <  本机支持版本      → 按旧版本规则导入，输出升级提示
     >  本机支持版本      → 中止，报 backup_too_new（提示升级应用）
5. 安全校验（见 §9）：条目名合法性、解压总大小、单条目大小
6. 统计预检结果，展示确认页：
     将导入 N 条 / 预计跳过 M 条（重复）/ 跳过 K 条（已过期）
     将新建分类 X 个、复用同名分类 Y 个
     解压后占用约 Z MB
7. 草稿段单独统计：**将新增 P 条草稿**（§8.4：草稿没有指纹，
   不存在"重复"这一说，所以措辞必须是"新增"而不是"导入"）
```

**第 6 步不可省。** 用户点"导入"之前必须看到会发生什么，尤其是"将新建 3 个分类""将跳过 412 条已过期"这类会改变预期的信息。

### 8.2 阶段二：写入

```
1. 创建 imports 行，status = running
2. 分类映射：按 name 精确匹配 → 复用已有 id；无同名 → 建新行
   构建 old_id → new_id 映射表
   （按名称而非 ID 匹配，是因为两个机器上的 ID 体系完全无关）
3. 标签映射：同上
4. 逐条处理 items（流式，不全量载入内存）：
   a. 校验 fingerprint 格式；不合法 → 计入 failed，继续
   b. 计算最终 expires_at：
        - 用户选「按绝对时间」（默认）→ 用 expiresAt 转 epoch
        - 用户选「重置 TTL」         → 用 importedAt + ttlSeconds
        - expiresAt 已是过去时间且未勾选"导入已过期条目" → 计入 skipped，继续
   c. 查重：指纹已存在于存活条目
        - 策略 merge（默认） → 累加 use_count，取较新的 lastUsedAt，保留本机分类
        - 策略 skip          → 计入 skipped，继续
        - 策略 overwrite     → 软删旧条目后插入新的
        - 策略 duplicate     → 插入为新条目（不改指纹唯一索引即可，见下）
   d. 重映射 categoryId / tagIds（用步骤 2、3 的映射表）
   e. 对每个 blob：
        - 从 ZIP 读出 → 计算 sha256 → 与清单比对
        - 不符 → 该条目计入 failed，跳过（不写入半截 blob）
        - 相符 → 原子写入 blobs/<a>/<b>/<sha256>.<ext>
   f. INSERT items 行，写 import_id
   g. 每 500 条 COMMIT 一次并开新事务（长事务会让 WAL 无限增长）
4'. 逐条处理 drafts（§3.7），在 items 全部提交之后单独一条事务里：
   a. 从正文里扫出 `](blobs/<rel>)` 引用（**没有** blobs[] 数组可读）
   b. 逐个从 ZIP 读出 → 核对 sha256（期望值由**包内路径自己反推**，
      路径就是 `blobs/<sha前2>/<sha次2>/<sha>.<ext>`）→ 按内容重新寻址落盘
      - 路径不是合法分片、或校验不符、或写盘失败 → **该处引用作废**
   c. 据步骤 b 的结果改写正文：作废的引用换成 `]()`（空 URL），
      其余改写成导入方自己那一侧的写法
      （**必须先落盘再改写**：反过来的话，某个 blob 被弃用时正文里
      那条引用已经进库了，结果是一条指向不存在文件的引用）
   d. INSERT drafts 行：写 import_id、保留源机器的 created_at / updated_at、
      **seq 一律 NULL**、sort_order 追加到当前末尾
5. 更新 imports 行：finished_at、计数、status
   status = ok（failed == 0）/ partial（0 < failed < 总数）/ failed
6. 触发 FTS 重建校验（触发器应已处理，此处仅做行数比对）
```

### 8.3 导入选项

| 选项 | 默认 | 说明 |
|---|---|---|
| `conflictPolicy` | `merge` | `merge` / `skip` / `overwrite` / `duplicate` |
| `expiryPolicy` | `absolute` | `absolute` / `reset` |
| `importExpired` | `false` | 是否导入已过期条目 |
| `categoryPolicy` | `mergeByName` | `mergeByName` / `createAll` / `manual` |
| `importSettings` | `false` | 是否用包内 settings 覆盖本机配置（默认不动本机） |
| `errorTolerance` | `100` | 累计失败超过此数则中止（避免一个坏包跑满磁盘） |

### 8.4 幂等性

靠 §4.1 的 `uq_items_fp_alive ON items(fingerprint) WHERE deleted_at IS NULL` 偏唯一索引 + 默认 merge 策略实现。**同一包导入两次，第二次全部计入 skipped，库中不产生任何重复行。** 这比维护"已导入包哈希"更可靠——用户可能重装应用、可能手动改过库。

**草稿不适用这条**（§3.7）：它没有指纹，"两条一模一样的草稿"是完全正常
的状态，没有任何字段能声明"这条和那条是同一份"。所以重导一次，
包里的草稿就**再新增一批**。这是刻意的选择：去重只能靠标题或正文哈希，
而标题会重复、正文会微改，任何一种都会让用户"改了两个字就多出一份"
或者"两份不同的草稿被当成一份删掉"。确认页必须如实写"将新增 N 条草稿"。

例外：`duplicate` 策略下需要临时绕过唯一索引。实现方式是把插入行的 `fingerprint` 存为 `sha256:<hex>:<序号>` 变体，并在 `canonical_fingerprint` 新列保存原值。**建议直接不做 `duplicate` 策略**，它带来的复杂度不划算。

### 8.5 回滚

`imports` 表记录了批次。回滚 = `DELETE FROM items WHERE import_id = ?`，
**再 `DELETE FROM drafts WHERE import_id = ?`**，两次删除在同一个事务里
（分两次提交时，第二次失败会留下"条目没了、草稿还在"的半截状态，
而批次已经被标成 `rolled_back`——用户再也找不到入口去撤掉那些草稿）。
最后跑一次孤儿 blob 扫描。两个集合都不删 blob 文件：`blobs/` 是内容寻址的，
同一份字节可能被别的条目或别的草稿引用。

⚠️ 不带 import_id 的行一律不动（老草稿升级上来时那一列是 NULL，
不会被任何一次回滚误删）。

UI 上提供"撤销上次导入"，并明确标注可回滚的时间窗口（超过 30 天的批次记录自动清理，回滚入口随之消失）。

---

## 9. 安全要求

这是本格式唯一的高风险面：`.clipbak` 是可以从任何地方下载到的文件。

| 威胁 | 防护 |
|---|---|
| **ZIP Slip**（条目名含 `../` 或绝对路径，解压时写到包外） | 拒绝一切非法条目名：含 `..` 段、以 `/` 或盘符开头、含 NUL、指向符号链接。且**永远不要用"按条目名拼路径直接落盘"的写法**，blob 落盘路径必须由我们根据其 sha256 **重新计算**得出，条目名只用于定位读取，不参与目标路径构造 |
| **解压炸弹**（声明小、解压后极大） | 预检阶段累加所有条目的 `uncompressed_size`，超过 `stats.blobBytes × 1.2` 或绝对上限 20 GB 则中止 |
| **单条目炸弹** | 单条 blob 解压后超过 `capture.imageMaxBytes`（10 MB）跳过并计数 |
| **压缩比异常** | 单条目 `uncompressed/compressed > 1000` 且未压缩 > 10 MB → 拒绝 |
| **哈希不匹配** | 每个 blob 落盘前核对 sha256，不符即弃 |
| **草稿正文里的引用** | 同样只认分片路径：`blobs/<aa>/<bb>/<sha256>.<ext>` 的路径**自证内容**，期望的 sha256 从路径反推（清单里没有 `blobs[]` 可读）。反推不出来的路径一律当作不可信，宁可丢这一处引用，也不落一个来源不明的文件。落盘路径同样由 sha256 重算，绝不采用正文里写的路径 |
| **路径穿越到符号链接** | blob 目录权限收紧（macOS `0700`，Windows 默认用户目录 ACL），落盘用 `O_CREAT \| O_EXCL \| O_NOFOLLOW` |
| **磁盘写满** | 预检时检查可用空间 ≥ 解压总大小 × 1.3，不足则拒绝 |
| **清单中存在敏感字段** | 导出时不生成任何凭据类字段；本工具也不存凭据 |

---

## 10. 版本演进规则

| 变更 | 版本动作 |
|---|---|
| 新增可选字段 | `formatVersion` **不变**，读取方忽略未知字段 |
| 字段语义变更、字段重命名、移除字段 | `formatVersion` **+1** |
| 容器从 ZIP 换成别的东西 | `formatVersion` **+1**（并且大概率要引入 `minReaderVersion`） |

`drafts`（§3.7）就是"新增可选字段"这一类的实例：它在 v1 之后加入，
**没有动 `formatVersion`**。只认 `items` 的老读取方拿到带草稿的包时，
既不会崩、也不会丢它认识的那部分数据；`stats.drafts` 同属这一类。
反过来，读取方**不能**假定 `drafts` 一定存在——老包没有这个键。

这条规则只有在"新字段确实可选"时才成立：一旦某个字段成为**语义必需**
（不读它就会误解整包数据），那就属于"字段语义变更"，必须 `+1`。

读取方规则：**未知字段一律忽略，未知的 `kind` 值降级为文本处理**（保留 `preview` 作为文本内容），保证旧版本读新包时至少不丢数据、不崩溃。

---

## 11. 第三方消费指南

`README.txt` 的内容需足以让外部脚本独立解析本包，至少包含：

1. 包格式版本与生成工具版本、导出时间
2. 清单文件位置与字段说明表（对应 §3，含 §3.7 的 `drafts`）
3. 时间字段格式说明（ISO-8601 带时区）
4. blob 的寻址规则（`blobs/<sha256前2位>/<第3-4位>/<sha256>.<ext>`）
5. 一句话示例：如何用 Python 三行读出全部文本

```python
import zipfile, json
z = zipfile.ZipFile("pawclip-backup-full-20260915-114158.clipbak")
m = json.loads(z.read("manifest.json"))
texts = [i["text"] for i in m["items"] if i.get("text")]
notes = [d["md"] for d in m.get("drafts", [])]   # 老包没有这个键，所以用 .get
```

这条"能被三行脚本读出来"的要求，是本格式所有设计取舍的最终裁判。

---

## 12. 支持性声明

`.clipbak` 是**归档格式**，不是同步格式：它没有增量、没有冲突解决、没有删除传播。用它做日常备份完全够用，但不要指望靠频繁互导来维持两台机器一致。本工具明确不做多设备同步。
