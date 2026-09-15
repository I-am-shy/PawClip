#ifndef PAWCLIP_PASTEBOARD_DARWIN_H
#define PAWCLIP_PASTEBOARD_DARWIN_H

// macOS 剪贴板桥。只暴露 C 接口，Go 侧用 cgo 调。
//
// 实现是 MRC（手动引用计数）而非 ARC —— 与 poc/wails-panel 的验证工程保持一致，
// 少一个编译开关就少一处跨版本行为差异。

// 变更计数。**热路径**：内部只读一个 NSInteger，不构造任何 Objective-C 对象。
// 之所以要单独给它一个函数，是因为 DESIGN.md §14 第 9 条把"轮询里零分配"
// 列为决定空闲 CPU 与内存增长率的关键。
long pb_change_count(void);

// 系统距上一次输入事件的秒数。用于自适应轮询间隔（§7 第 1 条）。
double pb_seconds_since_last_input(void);

// 一次剪贴板快照。
//
// 所有指针字段由实现 malloc，调用方必须用 pb_snapshot_free 释放。
// 为 NULL / 长度为 0 表示剪贴板里没有该表示。
typedef struct {
    long change_count;  // 本次读取观察到的 changeCount
    char *types_json;   // JSON 数组：原始 pasteboard type 名（保密判定要用）
    char *text;         // UTF-8 纯文本
    char *html;         // HTML（macOS 侧本身就是纯 HTML，无 CF_HTML 头）
    void *rtf;
    int rtf_len;
    void *png;          // 已归一化为 PNG 的位图字节
    int png_len;
    char *files_json;   // JSON 数组：文件绝对路径
    char *app_id;       // 前台 App 的 bundle identifier
    char *app_name;     // 前台 App 的 localizedName
} pb_snapshot;

// 读取快照。
//
// ⚠️ changeCount 与 frontmostApplication 必须在**同一次调用**里取，
// 否则来源应用会记错（DESIGN.md §7 第 2 条）。调用方拿到 change_count 后
// 应再调一次 pb_change_count()，不一致说明读的过程中剪贴板又变了（抖动）。
//
// 返回 0 成功；非 0 失败，原因写入 err。
int pb_read(pb_snapshot *out, char *err, int errlen);

// 释放快照里所有由实现分配的内存，并把结构体清零。
void pb_snapshot_free(pb_snapshot *s);

// 一次性写回。任何字段为空（字符串 NULL / 长度 <= 0）表示不带该表示。
//
// 只做一次 clearContents，多种表示同时写入——分成多次调用会让中间态
// 被别的进程看到（也会多推一次变更信号）。
int pb_write(const char *text,
             const char *html,
             const void *rtf, int rtf_len,
             const void *png, int png_len,
             const char *const *files, int files_n,
             char *err, int errlen);

// 释放实现分配的字节缓冲（供将来返回裸指针的接口用）。
void pb_release(void *p);

#endif
