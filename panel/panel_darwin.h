#ifndef PAWCLIP_PANEL_DARWIN_H
#define PAWCLIP_PANEL_DARWIN_H

// PawClip 的 macOS 面板桥：免抢焦点面板 + 全局热键 + 托盘 + 自动粘贴。
//
// 免抢焦点的方案来自 M0 实测（docs/DESIGN.md §0.2 / poc/wails-panel），两个必要条件
// **缺一不可**，且都在这一个文件里：
//
//   ① 真正 alloc/init 出一个 NSPanel（不要 object_setClass，实测必崩）
//   ② App 跑在 NSApplicationActivationPolicyAccessory 下
//
// 显示顺序也不能反：窗口不可见时 makeKeyWindow 是空操作，
// 必须先 orderFrontRegardless 再 makeKeyWindow。

// 把 App 切到 Accessory 激活策略（等价于 Info.plist 的 LSUIElement）。
// 越早调用越好——最好在窗口创建之前，见 docs/DESIGN.md §7 第 7 条。
int paw_set_accessory(void);

// NSApp.windows 里是否已经有窗口（用于等 Wails 建窗完成）。
int paw_has_window(void);

// 等到 Wails 窗口出现并把它接管进免抢焦点面板。
// width/height 是面板的逻辑尺寸；minw/minh/maxw/maxh 是缩放边界
// （来自 panel.go 的 MinPanelWidth 等常量——边界的唯一定义在 Go 侧，
// 这里只负责把系统窗口的约束设成同样的数）。
// 返回 0 成功。
int paw_attach(int width, int height, int minw, int minh, int maxw, int maxh,
               char *err, int errlen);

// 免抢焦点显示面板并取得键盘。返回 1 表示面板确实成了 key window。
int paw_show(void);

// 隐藏面板。
void paw_hide(void);

// 失焦自动收起的**触发源**在原生侧：面板丢掉键盘（用户点了面板以外的
// 任何地方）时，通过回调 pawGoPanelBlur（见 export_darwin.go）把这件事
// 报给 Go，由 Go 按设置 ui.closeOnBlur 决定收不收。原生只负责识别时机、
// 不做策略判断——所以这里没有配套的导出函数，逻辑在 panel_darwin.m 的
// PawClip_ScheduleBlurCheck / PawClip_ReportBlur 里。

int paw_visible(void);

// ── 全局热键 ────────────────────────────────────────────────────
// keycode 是 kVK_* 虚拟键码，modifiers 是 Carbon 的 cmdKey/shiftKey/... 位。
int paw_register_hotkey(unsigned int keycode, unsigned int modifiers, char *err, int errlen);
void paw_unregister_hotkey(void);

// ── 托盘 ────────────────────────────────────────────────────────
// icon_png 是模板图（纯黑 + alpha）。菜单由 Go 侧通过 paw_tray_clear /
// paw_tray_add 逐项构建，避免在 C 里维护一个 Item 数组的生命周期。
int paw_tray_install(const void *icon_png, int png_len, const char *tooltip, char *err, int errlen);
void paw_tray_clear(void);
void paw_tray_add(const char *label, int action, int checked, int disabled);
void paw_tray_separator(void);
// 提交：把 clear + add 出来的菜单真正挂到 status item 上。
void paw_tray_commit(void);
void paw_tray_remove(void);

// ── 自动粘贴 ────────────────────────────────────────────────────
// 是否已获得"辅助功能"授权（docs/DESIGN.md §7 第 4 条 / §13 风险表）。
int paw_is_trusted(void);
// 弹出系统授权引导（打开"系统设置 → 隐私与安全性 → 辅助功能"）。
void paw_request_trust(void);
// 模拟一次 ⌘V。返回 0 成功，非 0 表示没有授权或事件创建失败。
int paw_autopaste(char *err, int errlen);

// ── 诊断 ────────────────────────────────────────────────────────
// 采集一次快照，返回 malloc 的 JSON；调用方需 paw_free。
// 用于验收：证明"面板持有键盘但前台 App 没被抢走"。
char *paw_diag_json(void);
void paw_free(char *p);

// 当前面板尺寸（供 WebView 自适应）。
double paw_panel_width(void);
double paw_panel_height(void);

// 把面板移到鼠标所在屏幕的顶部居中（多屏时出现在用户正在看的那块屏上）。
// 用户手动拖动/缩放过面板后不再生效（位置归用户管）。
void paw_panel_recenter(void);

// 启动一次原生窗口拖动（前端在标题栏空白处按下鼠标时调用）。
// borderless 面板没有标题栏，Wails 的 CSS app-region 也管不到它，只能走
// performWindowDragWithEvent 交还 AppKit 的标准拖动循环。
void paw_panel_drag(void);

#endif
