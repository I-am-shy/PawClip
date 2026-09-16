package backup

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

// readmeText 生成包内的 README.txt。
//
// 这个文件是**格式规范的一部分**，不是装饰。BACKUP-FORMAT.md §11 明确
// 要求它的内容足以让外部脚本独立解析本包：包格式版本与生成工具版本、
// 清单位置与字段说明、时间字段格式、blob 寻址规则、以及一段可直接跑的
// Python 示例。用户拿这个包去问 AI、去写脚本、去迁移到别的工具时，
// 全靠它。
//
// 所以它是 i18n 的**易漏位置**之一（DESIGN §14 第 24 条列了 6 处）。
// 这里的处理是**分两段**：
//
//   - 开头那段给人看的提示（"这是什么包 / 怎么恢复 / 里面有敏感内容"）
//     由调用方按当前语言生成，通过 opt.Preamble 传进来 —— 它就是
//     第 24 条要的那一处 i18n；
//   - 下面的格式规范用中英双语写死，**刻意不跟随语言**：它是给"别人的
//     工具"看的（外部脚本按它解析本包），同一个格式的说明不该有两个版本。
//
// 两段分开是刻意的：跟着语言走的是"要人看的话"，不跟着走的是"格式契约"。
func readmeText(opt ExportOptions, exportedAt time.Time) string {
	appVer := opt.AppVersion
	if appVer == "" {
		appVer = "unknown"
	}
	var b strings.Builder
	if p := strings.TrimSpace(opt.Preamble); p != "" {
		b.WriteString(p)
		b.WriteString("\n\n")
	}

	fmt.Fprintf(&b, `PawClip 剪贴板备份包 / PawClip clipboard backup
================================================================

格式 Format        : %s
格式版本 Version   : %d
生成工具 Tool      : PawClip %s (%s)
导出时间 Exported  : %s

这是一个标准 ZIP 文件。用任何解压工具都能打开，也能用三行 Python 读出来。
This is a plain ZIP file. Open it with any unzip tool, or read it with 3 lines of Python.

----------------------------------------------------------------
包内结构 / Layout
----------------------------------------------------------------

  README.txt              本说明 / this file
  manifest.json           清单 / manifest (or manifest.yaml)
  blobs/<aa>/<bb>/<sha256>.<ext>
                          内容寻址的二进制内容 / content-addressed blobs

blob 寻址规则 / blob addressing
  blobs/ 下再按 sha256 的**前两位**与**第 3-4 位**分两级，
  文件名就是完整的 sha256 十六进制串加扩展名：

    sha256 = 9f2a1c...e4
    path   = blobs/9f/2a/9f2a1c...e4.png

  两级分片的意义是避免单个目录堆到万级文件（Windows 上目录枚举会明显变慢）。
  缩略图（*.thumb.png）不导出，导入后由原始图片重新生成。

----------------------------------------------------------------
清单字段 / manifest fields
----------------------------------------------------------------

顶层 / top level
  format          固定 "pawclip.backup" / always "pawclip.backup"
  formatVersion   整型，当前为 %d / integer, currently %d
  appVersion      生成工具版本 / producing app version
  exportedAt      ISO-8601 **带时区偏移** / ISO-8601 **with timezone offset**
  platform        macos | windows
  scope           full | pinned | category | range
  stats.items            条目数 / item count
  stats.categories       分类数 / category count
  stats.tags             标签数 / tag count
  stats.blobBytes        全部 blob 的未压缩字节数 / total uncompressed blob bytes
  settings        **仅供参考**，导入默认不写入本机配置
                  informational only; importers should NOT apply it by default
  categories[]    { id, name, color, icon, sortOrder, ttlSeconds, rule }
  tags[]          { id, name, color }
  items[]         见下 / see below

items[]
  id              仅用于**包内交叉引用**（categoryId / tagIds）。
                  NOT a global identity — importers must remap it.
  kind            text | html | rtf | image | files | mixed
  preview         列表摘要 / short preview text
  fingerprint     "sha256:<64 位小写十六进制>" / "sha256:<64 lowercase hex>"
  byteSize        原始内容字节数 / original content size in bytes
  text            纯文本（kind 含文本时非空）/ plain text, may be null
  html            <= 64 KB 时内联，更大则走 blob / inlined when <= 64 KiB
  rtfPath         指向包内 rtf blob 的路径 / in-package path, may be null
  filePaths       kind = files 时的原始**绝对路径**数组。
                  只存路径，不内嵌文件内容 / paths only, file contents
                  are NOT embedded（除非导出时显式开启 embedFiles）.
  imageWidth      图片像素宽 / image width in pixels（非图片为 0）
  imageHeight     图片像素高 / image height in pixels
  sourceAppId     平台原生应用标识 / platform-native app id
  sourceAppName   应用显示名 / app display name
  sourceUrl       浏览器来源页 / source page, may be null
  categoryId      引用包内 categories[].id / references categories[].id
  tagIds          引用包内 tags[].id / references tags[].id
  pinned          为 true 时 expiresAt 必为 null / expiresAt is null when true
  ttlSeconds      相对存活时长（秒）/ relative lifetime in seconds
  expiresAt       绝对到期时刻，ISO-8601 带时区 / absolute expiry, ISO-8601
  firstSeenAt     首次复制时刻 / first copy time
  createdAt       排序键；重复复制时被更新 / sort key, refreshed on re-copy
  lastUsedAt      最近一次取用时刻 / last use time, may be null
  useCount        累计复制次数 / cumulative copy count
  blobs[]         { role, path, mime, bytes, sha256 }
                  role = image | rtf | html
                  path 是**包内**路径 / in-package path

----------------------------------------------------------------
时间字段 / time fields
----------------------------------------------------------------

全部时间字段都是 ISO-8601 且**带时区偏移**，例如：

  2026-09-15T11:41:58+08:00

导入方应转成 UTC epoch 存储。这样跨时区迁移时，条目说的仍然是
同一个**时刻**，而不是同一个"墙上时间"。

All timestamps are ISO-8601 **with a timezone offset**. Importers should
convert them to a UTC epoch. That way a migration across timezones
preserves the same *instant*, not the same wall-clock reading.

----------------------------------------------------------------
示例：三行 Python 读出全部文本 / read all text in 3 lines
----------------------------------------------------------------

  import zipfile, json
  z = zipfile.ZipFile("pawclip-backup-full-YYYYMMDD-HHMMSS.clipbak")
  m = json.loads(z.read("manifest.json"))
  texts = [i["text"] for i in m["items"] if i.get("text")]

（清单是 manifest.yaml 时改用 PyYAML：
  import yaml; m = yaml.safe_load(z.read("manifest.yaml"))）

----------------------------------------------------------------
注意 / notes
----------------------------------------------------------------

* 回收站内容（已删除条目）不在包内 / trashed items are excluded
* 缩略图不在包内，导入后重新生成 / thumbnails are regenerated on import
* 文件类条目只存路径，路径在目标机器上可能已失效
  / file entries store paths only; they may be invalid on the target machine
* 这个格式是**归档**不是同步：没有增量、没有冲突解决、没有删除传播
  / this is an archive format, not sync: no deltas, no conflict resolution,
    no delete propagation

`, FormatTag, FormatVersion, appVer, opt.Platform, exportedAt.Format(time.RFC3339), FormatVersion, FormatVersion)

	return b.String()
}

// hostPlatform 返回当前平台在清单里用的取值。
func hostPlatform() string {
	if runtime.GOOS == "windows" {
		return "windows"
	}
	return "macos"
}
