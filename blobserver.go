package main

import (
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/zego/pawclip/store"
)

// BlobURLPrefix 是缩略图 / 原图在 WebView 里的 URL 前缀。
//
// docs/DESIGN.md §14 第 1 条是一条硬约束：
//
//	**列表只传元数据，图片绝不走 IPC。** 图片/缩略图通过自定义协议直接交给
//	WebView 加载（Wails 静态资源服务）。把 100 张缩略图 base64 塞进 IPC 会让
//	首屏卡 1 秒以上、内存翻倍。
//
// 所以前端拿到的是 `blob/9f/2a/<sha>.thumb.png` 这样的**相对 URL**，
// 由 Wails 的 AssetServer 直接把它变成响应体。
//
// 前缀刻意不带前导 `/`，也不写成 `blob://`：相对 URL 在 Wails 的
// 自定义 scheme 下会解析成 `wails://wails/blob/...`，这正是 AssetServer 的
// 用户 handler 能接到的形式（见 wails 的 assetHandler.ServeHTTP：嵌入式
// 前端产物里找不到这个文件时，才会把请求转给用户 handler）。
const BlobURLPrefix = "blob/"

// blobServer 是 AssetServer 的用户 handler（只会收到"内嵌资源里没有"的请求）。
//
// 安全模型：**只读、只服务一个目录、路径必须过 SafeRel**。
// 它是唯一一个"把磁盘上的字节直接递给 WebView"的入口，所以多一道校验不嫌多：
//
//   - `store.SafeRel` 拒绝绝对路径、卷标、`..`、NUL；
//   - 拼出来的绝对路径还要再确认一次"仍在 blobs/ 之内"（防止 SafeRel
//     将来被改松）；
//   - 只允许普通文件（Lstat，拒绝目录，也拒绝符号链接）。
type blobServer struct {
	root func() *store.BlobStore
	log  func(format string, args ...any)
}

func newBlobServer(root func() *store.BlobStore, logf func(string, ...any)) *blobServer {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &blobServer{root: root, log: logf}
}

func (b *blobServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	bs := b.root()
	if bs == nil {
		http.Error(w, "blob store not ready", http.StatusServiceUnavailable)
		return
	}

	// 先 Clean 再判前缀：`/blob/../secret` 这类请求要靠 Clean 归一再判，
	// 否则前缀匹配会被绕过。
	cleaned := path.Clean("/" + r.URL.Path)
	if !strings.HasPrefix(cleaned, "/"+BlobURLPrefix) {
		http.NotFound(w, r)
		return
	}
	rel := strings.TrimPrefix(cleaned, "/"+BlobURLPrefix)
	if rel == "" {
		// 光访问 blob/ 根目录不是错误，只是没有内容可说——不要列目录。
		http.NotFound(w, r)
		return
	}
	if err := store.SafeRel(rel); err != nil {
		b.log("拒绝非法的 blob 路径", "path", rel, "err", err)
		http.NotFound(w, r)
		return
	}

	abs := bs.Abs(rel)

	// 二次确认：SafeRel 过了也要再看一眼最终路径确实落在根目录内。
	// 这是"两道锁"里便宜的那一道，用来兜住 SafeRel 本身的回归。
	rootAbs := bs.Root()
	if !strings.HasPrefix(abs, rootAbs+string(os.PathSeparator)) {
		b.log("blob 路径逃出根目录", "path", rel, "abs", abs)
		http.NotFound(w, r)
		return
	}

	fi, err := os.Lstat(abs)
	if err != nil || !fi.Mode().IsRegular() {
		// Lstat 而不是 Stat：内容寻址的 blob 目录里**不该有任何符号链接**，
		// 有一个就说明有人在往里面塞东西。直接拒绝，不去跟。
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()

	// 内容寻址 → 同一个 URL 的字节永不变，可以放心长缓存。
	// 这一条直接决定列表滚动的流畅度：滚动时浏览器不会重复问我们要同一张缩略图。
	w.Header().Set("Content-Type", contentTypeForExt(path.Ext(rel)))
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, path.Base(rel), fi.ModTime(), f)
}

// contentTypeForExt 只认识我们自己会写进 blobs/ 的那几种扩展名。
//
// 不引入 mime.TypeByExtension：它读系统注册表（Windows 上结果因机器而异），
// 而且对未知扩展名会回落到 text/plain —— 一个"未知二进制"被当成纯文本
// 递给 WebView 是能造成 XSS 的形态。这里显式白名单，其余一律
// application/octet-stream + nosniff。
func contentTypeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".tiff":
		return "image/tiff"
	case ".rtf":
		return "application/rtf"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".txt":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// blobURL 把数据库里的相对路径变成前端可用的 URL；非法路径返回空串
// （前端据此显示占位图，而不是去请求一个会被拒的 URL）。
func blobURL(rel string) string {
	if rel == "" {
		return ""
	}
	if err := store.SafeRel(rel); err != nil {
		return ""
	}
	return BlobURLPrefix + rel
}
