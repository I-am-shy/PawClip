#import "panel_darwin.h"
#import <Cocoa/Cocoa.h>
#import <Carbon/Carbon.h>
#import <ApplicationServices/ApplicationServices.h>
#import <QuartzCore/QuartzCore.h>
#import <objc/runtime.h>
#import <string.h>
#import <stdlib.h>
#import <stdio.h>

// Go 侧通过 cgo //export 提供这两个回调（见 export_darwin.go）。
// 声明成 void 参数形式以匹配 cgo 生成的原型。
extern void pawGoHotkey(void);
extern void pawGoTrayAction(int action);

// 热键的 four-char signature。用 'PAWC' 避免和别的 App 的热键 ID 撞上。
#define PAW_HOTKEY_SIG 'PAWC'

static NSWindow  *g_panel       = nil;
static NSWindow  *g_wailsWindow = nil; // 保留原窗口引用，避免被释放
static NSView    *g_webView     = nil; // 面板当前承载的 WebView（= Wails 的 contentView）
static Class      g_panelCls    = Nil;
static NSStatusItem *g_statusItem = nil;
static NSMenu    *g_statusMenu  = nil;
static id         g_trayTarget  = nil;
static EventHotKeyRef g_hotkeyRef = NULL;
static EventHandlerRef g_hotkeyHandler = NULL;

// 用户是否已经亲手摆过面板（拖动过位置或拉伸过尺寸）。
// 置位之后 paw_panel_recenter 不再在每次呼出时把面板拉回屏幕顶部居中——
// 用户既然自己放了位置，呼出时把窗口"传送"走就是跟他抢鼠标。
static BOOL g_userPlaced = NO;

// block 不允许捕获"数组类型"的局部变量（clang: cannot refer to declaration
// with an array type inside block），所以把错误缓冲区包进 struct 再用 __block 共享。
typedef struct { char msg[512]; } PawMsgBuf;

static void PawClip_Log(const char *fmt, ...) {
    va_list ap;
    va_start(ap, fmt);
    fprintf(stderr, "[pawclip/panel] ");
    vfprintf(stderr, fmt, ap);
    fprintf(stderr, "\n");
    va_end(ap);
    fflush(stderr);
}

// 把一段 block 放到主线程同步执行。
// 所有 AppKit 操作都必须走它：Wails 的 OnStartup 回调不在主线程上。
static void PawClip_OnMain(dispatch_block_t blk) {
    if ([NSThread isMainThread]) {
        blk();
    } else {
        dispatch_sync(dispatch_get_main_queue(), blk);
    }
}

// ── NSPanel 动态子类 ────────────────────────────────────────────
// borderless 面板默认 canBecomeKeyWindow = NO，不覆写就拿不到键盘。
static BOOL PawClip_canBecomeKey(id self, SEL _cmd)  { return YES; }
static BOOL PawClip_canBecomeMain(id self, SEL _cmd) { return YES; }

static Class PawClip_PanelClass(void) {
    if (g_panelCls) return g_panelCls;

    Class base = NSClassFromString(@"NSPanel");
    if (!base) return Nil;
    Class existing = NSClassFromString(@"PawClipPanel");
    if (existing) { g_panelCls = existing; return existing; }

    Class cls = objc_allocateClassPair(base, "PawClipPanel", 0);
    if (!cls) return Nil;

    // encoding 随平台可能是 "B"(bool) 或 "c"(signed char)，交给编译器决定。
    char types[16];
    snprintf(types, sizeof(types), "%s@:", @encode(BOOL));

    if (!class_addMethod(cls, @selector(canBecomeKeyWindow), (IMP)PawClip_canBecomeKey, types))
        class_replaceMethod(cls, @selector(canBecomeKeyWindow), (IMP)PawClip_canBecomeKey, types);
    if (!class_addMethod(cls, @selector(canBecomeMainWindow), (IMP)PawClip_canBecomeMain, types))
        class_replaceMethod(cls, @selector(canBecomeMainWindow), (IMP)PawClip_canBecomeMain, types);

    objc_registerClassPair(cls);
    g_panelCls = cls;
    return cls;
}

// ── 激活策略 ────────────────────────────────────────────────────
int paw_set_accessory(void) {
    __block int rc = 0;
    PawClip_OnMain(^{
        NSApplicationActivationPolicy cur = [NSApp activationPolicy];
        // ⚠️ 已经是 Accessory 时必须**提前返回**，不能照旧调一次再拿返回值判断。
        //
        // 实测（scripts/accept.sh 的真机实跑）：App 带 LSUIElement=true 启动时，
        // 进程一起来就是 Accessory，此时 `setActivationPolicy:Accessory` 返回 NO
        // —— 它表示"策略没有发生变化"，而不是"切换失败"。原来把它当成失败，
        // 于是每次启动都固定打一条
        //     WARN 切换到 Accessory 激活策略失败
        // 而这条 WARN 恰好出现在 docs/DESIGN.md §0.2 认定的"免抢焦点必要条件"上，
        // 排查"面板抢焦点"的人会被它带偏（去查一个根本不存在的故障）。
        // 真正需要报警的只有下面那条路径：策略不是 Accessory，且切不过去。
        if (cur == NSApplicationActivationPolicyAccessory) {
            PawClip_Log("activationPolicy 已是 Accessory（cur=%ld），无需切换", (long)cur);
            return;
        }
        BOOL ok = [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
        rc = ok ? 0 : 1;
        PawClip_Log("activationPolicy %ld -> Accessory (ok=%d)", (long)cur, ok);
    });
    return rc;
}

int paw_has_window(void) {
    __block int found = 0;
    PawClip_OnMain(^{
        for (NSWindow *w in [NSApp windows]) {
            if (w.contentView != nil) { found = 1; break; }
        }
    });
    return found;
}

// ── 接管 Wails 的 WebView ───────────────────────────────────────
// 思路（M0 实测的路线 D）：不动 Wails 自己建的窗口，而是**新建一个真 NSPanel**，
// 把它的 contentView（含 WKWebView）搬过来，然后把 Wails 的窗口藏起来。
//
// 为什么不 object_setClass 换成 NSPanel：Wails 窗口的真实类是
// NSKVONotifying_WailsWindow，带 userMinSize/userMaxSize 两个 ivar；
// NSPanel 的 ivar 布局与之重叠，调用 NSPanel 专有方法会写坏尺寸约束内存，
// 随后任何 orderOut 直接 SIGTRAP。实测结论，别再试。
static int PawClip_AttachOnMain(int width, int height,
                                int minw, int minh, int maxw, int maxh,
                                char *err, int errlen) {
    if (g_panel != nil) return 0; // 幂等

    NSWindow *w = nil;
    for (NSWindow *cand in [NSApp windows]) {
        if (cand.contentView != nil) { w = cand; break; }
    }
    if (!w) { snprintf(err, errlen, "no window in NSApp.windows"); return 3; }

    NSView *cv = [w contentView];
    if (!cv) { snprintf(err, errlen, "contentView is nil"); return 5; }

    Class cls = PawClip_PanelClass();
    if (!cls) { snprintf(err, errlen, "cannot create PawClipPanel class"); return 4; }

    NSRect frame = NSMakeRect(0, 0, width, height);

    // styleMask 里必须有 Resizable：borderless 窗口默认完全没有缩放边缘，
    // 加上这一位后 macOS 会在窗口四周给出约 8pt 的隐形拉伸边（没有标题栏，
    // 视觉上仍然是全圆角卡片）。NonactivatingPanel 是免抢焦点的根基，
    // 见 docs/DESIGN.md §0.2。
    id panel = [[cls alloc] initWithContentRect:frame
                                      styleMask:(NSWindowStyleMaskNonactivatingPanel |
                                                 NSWindowStyleMaskBorderless |
                                                 NSWindowStyleMaskResizable)
                                        backing:NSBackingStoreBuffered
                                          defer:NO];
    if (!panel) { snprintf(err, errlen, "NSPanel init returned nil"); return 6; }

    // 保住 Wails 窗口不被释放，并让它腾出 contentView。
    g_wailsWindow = [w retain];
    [cv retain];
    [w setContentView:[[[NSView alloc] initWithFrame:frame] autorelease]];
    [cv setFrame:frame];
    [cv setAutoresizingMask:(NSViewWidthSizable | NSViewHeightSizable)];

    // ── 圆角卡片 + 投影（对齐喵剪贴图标的"扁平 + 柔和阴影"风格）────────
    // 窗口本身透明、由内容层裁出圆角：非不透明窗口的系统投影取自内容的
    // alpha 形状，所以圆角卡片得到的是跟着圆角走的柔和投影，而不是方角影子。
    // 注意这与 Wails 的 WebviewIsTransparent 是两回事：WebView 自己仍然
    // 不透明地画底色，只是被内容层的 cornerRadius 裁掉四个角，文字抗锯齿
    // 不受影响（M0 的结论针对的是 WebView 画在透明底上的情形）。
    [(NSWindow *)panel setOpaque:NO];
    [(NSWindow *)panel setBackgroundColor:[NSColor clearColor]];
    [(NSWindow *)panel setHasShadow:YES];
    [cv setWantsLayer:YES];
    CALayer *cvLayer = [cv layer];
    if (cvLayer) {
        [cvLayer setCornerRadius:16.0];
        [cvLayer setMasksToBounds:YES];
    }

    // 缩放边界：太小布局会挤碎，太大就失去了"轻量面板"的形态。
    // 数值由 Go 侧传入（panel.go 是唯一定义处），避免两处常量各自漂移。
    [(NSWindow *)panel setContentMinSize:NSMakeSize(minw, minh)];
    [(NSWindow *)panel setContentMaxSize:NSMakeSize(maxw, maxh)];

    [(NSWindow *)panel setContentView:cv];
    [cv release];
    g_webView = cv;

    [(NSPanel *)panel setBecomesKeyOnlyIfNeeded:NO]; // 一显示就收键盘，不等点击
    [(NSWindow *)panel setLevel:NSFloatingWindowLevel];
    [(NSWindow *)panel setHidesOnDeactivate:NO];
    [(NSWindow *)panel setReleasedWhenClosed:NO];
    [(NSWindow *)panel setCollectionBehavior:(NSWindowCollectionBehaviorCanJoinAllSpaces |
                                              NSWindowCollectionBehaviorFullScreenAuxiliary)];

    // 用户开始拉伸尺寸也算"亲手摆过"：呼出时同样不再自动居中。
    [[NSNotificationCenter defaultCenter]
        addObserverForName:NSWindowWillStartLiveResizeNotification
                    object:panel
                     queue:nil
                usingBlock:^(__unused NSNotification *note) { g_userPlaced = YES; }];

    g_panel = panel;

    // 把 Wails 的窗口藏起来。它已经不含 WebView 了，留着只是个空壳。
    [w orderOut:nil];
    PawClip_Log("attached: panel=%s webview=%s",
                class_getName(object_getClass(panel)), class_getName(object_getClass(cv)));
    return 0;
}

int paw_attach(int width, int height, int minw, int minh, int maxw, int maxh,
               char *err, int errlen) {
    __block int rc = 0;
    __block PawMsgBuf mb;
    mb.msg[0] = '\0';
    PawClip_OnMain(^{
        rc = PawClip_AttachOnMain(width, height, minw, minh, maxw, maxh,
                                  mb.msg, (int)sizeof(mb.msg));
    });
    if (rc != 0) snprintf(err, errlen, "%s", mb.msg);
    return rc;
}

// ── 显示 / 隐藏 ─────────────────────────────────────────────────
// 顺序不能反：orderFrontRegardless 只把窗口提到其层级最前、不激活 App；
// 但窗口不在屏幕上时 makeKeyWindow 是**空操作**，所以必须先让它可见。
static int PawClip_ShowOnMain(void) {
    if (!g_panel) return 0;
    [g_panel orderFrontRegardless];
    if ([g_panel canBecomeKeyWindow]) [g_panel makeKeyWindow];
    return [g_panel isKeyWindow] ? 1 : 0;
}

int paw_show(void) {
    __block int r = 0;
    PawClip_OnMain(^{
        PawClip_ShowOnMain();
        r = [g_panel isKeyWindow] ? 1 : 0;
    });
    return r;
}

void paw_hide(void) {
    PawClip_OnMain(^{
        if (g_panel) [g_panel orderOut:nil];
    });
}

int paw_visible(void) {
    __block int v = 0;
    PawClip_OnMain(^{ v = (g_panel && [g_panel isVisible]) ? 1 : 0; });
    return v;
}

double paw_panel_width(void) {
    __block double v = 0;
    PawClip_OnMain(^{ if (g_panel) v = [g_panel frame].size.width; });
    return v;
}

double paw_panel_height(void) {
    __block double v = 0;
    PawClip_OnMain(^{ if (g_panel) v = [g_panel frame].size.height; });
    return v;
}

// 把面板放到鼠标所在屏幕的顶部居中。
// 多屏时用户正在看的是鼠标那块屏，而不是"主屏"——用主屏会让面板出现在
// 另一块显示器上，这是"呼出后没看见面板"最常见的成因。
void paw_panel_recenter(void) {
    PawClip_OnMain(^{
        if (!g_panel) return;
        // 用户亲手拖过/拉伸过面板之后，位置归用户管：呼出时不再把它
        // "传送"回顶部居中，否则拖动功能等于形同虚设。
        if (g_userPlaced) return;
        NSScreen *screen = nil;
        NSPoint mouse = [NSEvent mouseLocation];
        for (NSScreen *s in [NSScreen screens]) {
            if (NSPointInRect(mouse, [s frame])) { screen = s; break; }
        }
        if (!screen) screen = [NSScreen mainScreen];
        if (!screen) return;

        NSRect vis = [screen visibleFrame];
        NSRect f = [g_panel frame];
        // 距屏幕顶部约 12% 处居中：手指不用移动太多就能扫到列表。
        CGFloat x = vis.origin.x + (vis.size.width - f.size.width) / 2.0;
        CGFloat y = vis.origin.y + vis.size.height - f.size.height - vis.size.height * 0.12;
        if (y < vis.origin.y) y = vis.origin.y;
        [g_panel setFrameOrigin:NSMakePoint(x, y)];
    });
}

// ── 拖动 ────────────────────────────────────────────────────────
// 面板是 borderless 窗口，没有标题栏可抓；Wails 的 CSS app-region 机制
// 作用在它自己的（已被掏空的）宿主窗口上，对面板无效。所以拖动由前端
// 在标题栏空白处按下鼠标时调用本函数，交还给 AppKit 的标准拖动循环。
void paw_panel_drag(void) {
    PawClip_OnMain(^{
        if (!g_panel) return;
        // performWindowDragWithEvent 需要一个鼠标事件来启动拖动循环；
        // 前端按下 → IPC → 到这里，NSApp 的 currentEvent 仍是那次
        // leftMouseDown（拖动还没开始，事件循环没有推进）。
        NSEvent *ev = [NSApp currentEvent];
        if (ev == nil) return;
        g_userPlaced = YES;
        [g_panel performWindowDragWithEvent:ev];
    });
}

// ── 全局热键 ────────────────────────────────────────────────────
static OSStatus PawClip_HotkeyHandler(EventHandlerCallRef next, EventRef ev, void *user) {
    (void)next; (void)user;
    EventHotKeyID hk;
    OSStatus st = GetEventParameter(ev, kEventParamDirectObject, typeEventHotKeyID,
                                    NULL, sizeof(hk), NULL, &hk);
    if (st == noErr && hk.signature == PAW_HOTKEY_SIG) {
        // 回调在 App 的主事件循环里，**不是** Go 的 goroutine。
        // Go 侧负责把它转交给自己的 goroutine（见 export_darwin.go）。
        pawGoHotkey();
    }
    return noErr;
}

int paw_register_hotkey(unsigned int keycode, unsigned int modifiers, char *err, int errlen) {
    __block int rc = 0;
    __block PawMsgBuf mb;
    mb.msg[0] = '\0';

    PawClip_OnMain(^{
        if (!g_hotkeyHandler) {
            EventTypeSpec spec = { kEventClassKeyboard, kEventHotKeyPressed };
            OSStatus st = InstallApplicationEventHandler(&PawClip_HotkeyHandler, 1, &spec,
                                                         NULL, &g_hotkeyHandler);
            if (st != noErr) {
                snprintf(mb.msg, sizeof(mb.msg), "InstallApplicationEventHandler failed: %d", (int)st);
                rc = 1;
                return;
            }
        }
        if (g_hotkeyRef) { UnregisterEventHotKey(g_hotkeyRef); g_hotkeyRef = NULL; }

        EventHotKeyID hk;
        hk.signature = PAW_HOTKEY_SIG;
        hk.id = 1;
        OSStatus st = RegisterEventHotKey(keycode, modifiers, hk,
                                         GetApplicationEventTarget(), 0, &g_hotkeyRef);
        if (st != noErr) {
            // -9868 == eventHotKeyExistsErr：被别的程序占了。
            snprintf(mb.msg, sizeof(mb.msg), "RegisterEventHotKey failed: %d%s", (int)st,
                     st == (OSStatus)-9868 ? " (eventHotKeyExistsErr：热键已被占用)" : "");
            rc = 2;
            return;
        }
        PawClip_Log("hotkey registered (keycode=%u modifiers=0x%x)", keycode, modifiers);
    });

    if (rc != 0) snprintf(err, errlen, "%s", mb.msg);
    return rc;
}

void paw_unregister_hotkey(void) {
    PawClip_OnMain(^{
        if (g_hotkeyRef) { UnregisterEventHotKey(g_hotkeyRef); g_hotkeyRef = NULL; }
        if (g_hotkeyHandler) { RemoveEventHandler(g_hotkeyHandler); g_hotkeyHandler = NULL; }
    });
}

// ── 托盘 ────────────────────────────────────────────────────────
// 菜单点击的 target。用一个独立的小对象而不是让 AppDelegate 承担，
// 这样托盘可以被整体装/卸而不影响别的部分。
@interface PawTrayTarget : NSObject
- (void)menuItemClicked:(id)sender;
@end

@implementation PawTrayTarget
- (void)menuItemClicked:(id)sender {
    NSInteger tag = [sender tag];
    pawGoTrayAction((int)tag);
}
@end

int paw_tray_install(const void *icon_png, int png_len, const char *tooltip, char *err, int errlen) {
    __block int rc = 0;
    __block PawMsgBuf mb;
    mb.msg[0] = '\0';

    PawClip_OnMain(^{
        if (g_statusItem == nil) {
            g_statusItem = [[NSStatusBar systemStatusBar]
                statusItemWithLength:NSSquareStatusItemLength];
            [g_statusItem retain];
        }
        if (g_trayTarget == nil) g_trayTarget = [[PawTrayTarget alloc] init];

        if (icon_png != NULL && png_len > 0) {
            NSData *data = [NSData dataWithBytes:icon_png length:(NSUInteger)png_len];
            NSImage *img = [[NSImage alloc] initWithData:data];
            if (!img) {
                snprintf(mb.msg, sizeof(mb.msg), "tray icon data is not a decodable image");
                rc = 1;
                return;
            }
            [img setSize:NSMakeSize(18, 18)];
            // 模板图：只由 alpha 承载形状，颜色交给系统按菜单栏明暗渲染。
            // 做成彩色会在深色菜单栏下完全看不见（docs/DESIGN.md §15.5）。
            [img setTemplate:YES];
            [[g_statusItem button] setImage:img];
            [img release];
        }
        if (tooltip) [[g_statusItem button] setToolTip:[NSString stringWithUTF8String:tooltip]];
    });

    if (rc != 0) snprintf(err, errlen, "%s", mb.msg);
    return rc;
}

void paw_tray_clear(void) {
    // 每轮重建一个 NSMenu：菜单项数量少、重建成本可忽略，
    // 而就地改菜单项很容易留下悬空 target（点击后 crash）。
    PawClip_OnMain(^{
        NSMenu *menu = [[NSMenu alloc] init];
        [menu setAutoenablesItems:NO];
        g_statusMenu = menu; // 由 statusItem 持有（setMenu 会 retain）
    });
}

void paw_tray_add(const char *label, int action, int checked, int disabled) {
    PawClip_OnMain(^{
        if (!g_statusMenu) return;
        NSString *title = label ? [NSString stringWithUTF8String:label] : @"";
        NSMenuItem *it = [[NSMenuItem alloc] initWithTitle:title
                                                   action:@selector(menuItemClicked:)
                                            keyEquivalent:@""];
        [it setTarget:g_trayTarget];
        [it setTag:(NSInteger)action];
        [it setEnabled:!disabled];
        if (checked) [it setState:NSControlStateValueOn];
        [g_statusMenu addItem:it];
        [it release];
    });
}

void paw_tray_separator(void) {
    PawClip_OnMain(^{
        if (!g_statusMenu) return;
        [g_statusMenu addItem:[NSMenuItem separatorItem]];
    });
}

void paw_tray_remove(void) {
    PawClip_OnMain(^{
        if (g_statusItem) {
            [[NSStatusBar systemStatusBar] removeStatusItem:g_statusItem];
            [g_statusItem release];
            g_statusItem = nil;
        }
        g_statusMenu = nil;
    });
}

// 把刚建好的菜单挂到 status item 上。
// 单独一个函数是为了不必给 paw_tray_add 传 C 数组（数组跨 cgo 传很别扭）：
// Go 侧在 paw_tray_clear + N 次 paw_tray_add 之后调用它提交。
void paw_tray_commit(void) {
    PawClip_OnMain(^{
        if (g_statusItem && g_statusMenu) [g_statusItem setMenu:g_statusMenu];
    });
}

// ── 自动粘贴 ────────────────────────────────────────────────────
int paw_is_trusted(void) {
    return AXIsProcessTrusted() ? 1 : 0;
}

void paw_request_trust(void) {
    // 直接打开"隐私与安全性 → 辅助功能"面板。比只弹一句"请去授权"有用得多：
    // 用户不会知道那个开关埋在哪。
    NSURL *url = [NSURL URLWithString:
        @"x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility"];
    if (url) [[NSWorkspace sharedWorkspace] openURL:url];
}

int paw_autopaste(char *err, int errlen) {
    if (!AXIsProcessTrusted()) {
        snprintf(err, errlen, "没有辅助功能授权（AXIsProcessTrusted = false）");
        return 1;
    }
    // 用 kVK_ANSI_V + maskCommand 发一次按键。
    // 目标是 cghidEventTap：比 cgSessionEventTap 更底层，能穿到别的 App。
    CGEventSourceRef src = CGEventSourceCreate(kCGEventSourceStateHIDSystemState);
    if (!src) {
        snprintf(err, errlen, "CGEventSourceCreate failed");
        return 2;
    }
    CGEventRef down = CGEventCreateKeyboardEvent(src, (CGKeyCode)kVK_ANSI_V, true);
    CGEventRef up   = CGEventCreateKeyboardEvent(src, (CGKeyCode)kVK_ANSI_V, false);
    if (!down || !up) {
        if (down) CFRelease(down);
        if (up) CFRelease(up);
        CFRelease(src);
        snprintf(err, errlen, "CGEventCreateKeyboardEvent failed");
        return 3;
    }
    CGEventSetFlags(down, kCGEventFlagMaskCommand);
    CGEventSetFlags(up, kCGEventFlagMaskCommand);
    // 加一点延迟再 post：紧跟在我们写剪贴板之后立刻投递，
    // 目标 App 可能还没读到新内容就收到了粘贴事件。
    CGEventPost(kCGHIDEventTap, down);
    usleep(15000); // 15ms
    CGEventPost(kCGHIDEventTap, up);
    CFRelease(down);
    CFRelease(up);
    CFRelease(src);
    return 0;
}

// ── 诊断 ────────────────────────────────────────────────────────
// 判断"有没有抢焦点"的正确判据是 frontmostApplication 有没有变成自己，
// **不是** NSApp.isActive —— Accessory 策略下面板持有键盘时 isActive 是 true，
// 但前台 App 依然是原来那个（菜单栏没被抢走）。M0 实测结论。
char *paw_diag_json(void) {
    __block char *out = NULL;
    PawClip_OnMain(^{
        NSString *selfID = [[NSBundle mainBundle] bundleIdentifier] ?: @"";
        NSRunningApplication *front = [[NSWorkspace sharedWorkspace] frontmostApplication];
        NSString *frontID = front.bundleIdentifier ?: @"";
        NSString *frontName = front.localizedName ?: @"";

        BOOL isKey    = g_panel ? [g_panel isKeyWindow] : NO;
        BOOL visible  = g_panel ? [g_panel isVisible] : NO;
        long mask     = g_panel ? (long)[g_panel styleMask] : 0;
        BOOL nonact   = (mask & NSWindowStyleMaskNonactivatingPanel) != 0;

        NSString *json = [NSString stringWithFormat:
            @"{\"appBundleID\":\"%@\",\"panelClass\":\"%@\",\"nsAppIsActive\":%d,"
             "\"panelIsKey\":%d,\"panelVisible\":%d,\"nonactivatingBit\":%d,"
             "\"frontmostBundleID\":\"%@\",\"frontmostName\":\"%@\","
             "\"stoleFocus\":%d,\"axTrusted\":%d}",
            selfID,
            g_panel ? NSStringFromClass([g_panel class]) : @"(nil)",
            [NSApp isActive], isKey, visible, nonact,
            frontID, frontName,
            (frontID.length > 0 && ![frontID isEqualToString:selfID]) ? 0 : 1,
            AXIsProcessTrusted()];
        out = strdup([json UTF8String]);
    });
    return out;
}

void paw_free(char *p) { if (p) free(p); }
