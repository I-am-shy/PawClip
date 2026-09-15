#ifndef PAWCLIP_PANEL_DARWIN_H
#define PAWCLIP_PANEL_DARWIN_H

// 对照组：验证"免抢焦点面板"到底依赖哪个手段
#define PAWCLIP_MODE_BASELINE 0 // 原样，不做任何处理
#define PAWCLIP_MODE_MASKONLY 1 // 只给 NSWindow 加 nonactivatingPanel 样式位
#define PAWCLIP_MODE_PANEL    2 // 换成 NSPanel 动态子类 + 样式位 + 覆写 canBecomeKey
#define PAWCLIP_MODE_PANELMIN 3 // 只换类 + 样式位，不碰 NSPanel 独有方法（定位崩溃点）
#define PAWCLIP_MODE_ADOPT    4 // 新建真 NSPanel，接管 Wails 的 contentView

// NSApp.windows 里是否已经有窗口（用于等待 Wails 建窗完成）
int pawclip_has_window(void);

// 应用处理方案。accessory=1 时同时把激活策略设为 Accessory。
// 返回 0 成功，非 0 失败，失败原因写入 err。
int pawclip_setup(int mode, int accessory, char *err, int errlen);

// 隐藏面板（orderOut，不改变激活状态）
void pawclip_hide(void);

// 显示面板且**不调用 activate**。返回 1 表示窗口确实成了 key window。
int pawclip_show_noactivate(void);

// 让本 App 变成 active（用于分离"App 激活"与"窗口成为 key"两个因素）
void pawclip_activate_app(void);

// 只做 orderFrontRegardless + makeKeyWindow，返回是否成为 key window
int pawclip_make_key(void);

// 采集一次诊断快照，返回 malloc 的 JSON 字符串，调用方需 pawclip_free
char *pawclip_diag_json(void);

// 释放 diag 字符串
void pawclip_free(char *p);

#endif
