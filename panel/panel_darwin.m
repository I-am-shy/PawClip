#import "panel_darwin.h"
#import <Cocoa/Cocoa.h>
#import <Carbon/Carbon.h>
#import <ApplicationServices/ApplicationServices.h>
#import <QuartzCore/QuartzCore.h>
#import <WebKit/WebKit.h>
#import <objc/runtime.h>
#import <string.h>
#import <stdlib.h>
#import <stdio.h>

// Go 侧通过 cgo //export 提供这两个回调（见 export_darwin.go）。
// 声明成 void 参数形式以匹配 cgo 生成的原型。
extern void pawGoHotkey(void);
extern void pawGoTrayAction(int action);
extern void pawGoPanelBlur(void);

// 热键的 four-char signature。用 'PAWC' 避免和别的 App 的热键 ID 撞上。
#define PAW_HOTKEY_SIG 'PAWC'

static NSWindow  *g_panel       = nil;
static NSWindow  *g_wailsWindow = nil; // 保留原窗口引用，避免被释放
static NSView    *g_webView     = nil; // 面板当前承载的 WebView（= Wails 的 contentView）
static CALayer   *g_cardLayer   = nil; // 不透明圆角底座（见 PawClip_AttachOnMain）
// 底座颜色，来自前端 body 的 --bg（由 PawClip_SyncBackdropFromDom 从 DOM 取）。
// 0.94,0.94,0.94 只是"前端还没说话之前"的占位，取不到也不至于全黑。
// 底座颜色，来自前端 body 的 --bg（由 PawClip_SyncBackdropFromDom 从 DOM 取）。
// 初值按系统外观猜（首个呼出前 DOM 的回答还没回来），取不到也不至于全黑。
static CGFloat    g_backdropRGB[3] = {0.94, 0.94, 0.94};
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

// 见后半的「失焦自动收起」一节：attach 里要注册观察者，所以先声明。
static void PawClip_ScheduleBlurCheck(void);

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

// ── 窗口状态探针（默认关闭）─────────────────────────────────────
// 面板窗口是**非不透明**的（setOpaque:NO + clearColor，为的是让系统投影
// 跟着圆角卡片走），它自己一个像素都不画，可见性完全依赖 WKWebView 合成
// 内容。于是所有"面板看起来是透明的"这一类问题，都只可能出在
// **"窗口可见"与"内容已合成"这两件事没对齐**上：要么窗口本来就不该可见
// （收起没生效），要么窗口可见但内容没跟上（App 不活跃 / 重新呼出）。
//
// 光读代码分不清是哪一种，所以留一个默认关闭的探针：打开之后每次显隐、
// 每次失焦判定都打一行状态。webInPanel 是最关键的那一列——它为 0 就说明
// WebView 根本不在面板的视图树里，窗口必然是一块空壳。
static int g_diag = 0;

void paw_set_diag(int on) { g_diag = on ? 1 : 0; }

static void PawClip_Diag(const char *tag) {
    if (!g_diag) return;
    if (![NSThread isMainThread]) return; // 窗口状态只有主线程读到的才算数
    BOOL vis    = g_panel ? [g_panel isVisible]   : NO;
    BOOL key    = g_panel ? [g_panel isKeyWindow] : NO;
    BOOL inWin  = (g_webView != nil && [g_webView window] == g_panel) ? YES : NO;
    BOOL modal  = ([NSApp modalWindow] != nil ||
                   (g_panel != nil && [g_panel attachedSheet] != nil)) ? YES : NO;
    NSRect f    = g_panel ? [g_panel frame] : NSZeroRect;
    // 屏幕与坐标系信息：这类问题（"面板去哪了""是不是透明的"）十有八九
    // 和多显示器与坐标系换算纠缠在一起，光有 frame 不够——AppKit 的
    // frame 是"主屏左下原点"，而人看到的位置、截图工具的 -R 参数都是
    // "主屏左上原点"。alpha 一并列出是为了区分"窗口级透明"与"内容未合成"。
    NSArray<NSScreen *> *screens = [NSScreen screens];
    NSScreen *prim = screens.firstObject;
    NSScreen *ps   = [g_panel screen];
    NSRect pf = prim ? prim.frame : NSZeroRect;
    NSRect wf = ps ? ps.frame : NSZeroRect;
    double tlx = f.origin.x;
    double tly = (prim ? NSMaxY(pf) : 0) - (f.origin.y + f.size.height);
    PawClip_Log("diag %-18s visible=%d key=%d appActive=%d modal=%d webInPanel=%d "
                "frame=%.0fx%.0f@%.0f,%.0f screens=%lu panelScreen=%.0fx%.0f@%.0f,%.0f "
                "primary=%.0fx%.0f topleft=%.0f,%.0f alpha=%.2f",
                tag, (int)vis, (int)key, (int)[NSApp isActive], (int)modal,
                (int)inWin, f.size.width, f.size.height, f.origin.x, f.origin.y,
                (unsigned long)screens.count, wf.size.width, wf.size.height, wf.origin.x, wf.origin.y,
                pf.size.width, pf.size.height, tlx, tly,
                g_panel ? [g_panel alphaValue] : -1.0);
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

    // ── 圆角卡片 + 投影（对齐喵剪贴图标的"扁平 + 柔和阴影"风格）────────
    // 窗口本身透明、由内容层裁出圆角：非不透明窗口的系统投影取自内容的
    // alpha 形状，所以圆角卡片得到的是跟着圆角走的柔和投影，而不是方角影子。
    // 注意这与 Wails 的 WebviewIsTransparent 是两回事：WebView 自己仍然
    // 不透明地画底色，只是被内容层的 cornerRadius 裁掉四个角，文字抗锯齿
    // 不受影响（M0 的结论针对的是 WebView 画在透明底上的情形）。
    //
    // ⚠️ 内容层必须垫一块**不透明的圆角底座**（g_cardLayer），这不是装饰：
    //
    //   这个窗口自己一个像素都不画（setOpaque:NO + clearColor），可见性
    //   完全依赖 WKWebView 合成内容。真机复现（2026-09-18，CGWindowList +
    //   screencapture 定点取证）证实：WebView 没有合成内容的那段时间，
    //   AppKit 的 isVisible=1、WindowServer 的 alpha=1.0 都拦不住它——
    //   屏幕上那块就是一扇能看穿到壁纸的透明空壳（"打开文件管理器后
    //   出现一个透明窗口/遮罩"的成因）。垫上底座之后，这个失效模式从
    //   构造上消失：最坏情况是一块底色的卡片，不再有透明的洞。
    //
    // 底座颜色从 DOM 的 body 背景色同步（PawClip_SyncBackdropFromDom），
    // 主题切换后由 hide/show 时机刷新；取不到时用占位灰，不会更糟。
    // 初值先按系统外观猜：DOM 的准确回答要等第一次 JS 求值回来。
    if ([[NSApp effectiveAppearance].name isEqualToString:NSAppearanceNameDarkAqua]) {
        g_backdropRGB[0] = g_backdropRGB[1] = g_backdropRGB[2] = 0.12;
    } else {
        g_backdropRGB[0] = g_backdropRGB[1] = g_backdropRGB[2] = 0.94;
    }
    NSView *host = [[NSView alloc] initWithFrame:frame];
    [host setWantsLayer:YES];
    CALayer *card = [host layer];
    if (!card) { card = [CALayer layer]; [host setLayer:card]; }
    [card setCornerRadius:16.0];
    [card setMasksToBounds:YES];
    [card setBackgroundColor:[NSColor colorWithSRGBRed:g_backdropRGB[0]
                                                 green:g_backdropRGB[1]
                                                  blue:g_backdropRGB[2]
                                                 alpha:1.0].CGColor];
    [card retain];
    g_cardLayer = card;
    [host setAutoresizingMask:(NSViewWidthSizable | NSViewHeightSizable)];

    [cv setFrame:host.bounds];
    [cv setAutoresizingMask:(NSViewWidthSizable | NSViewHeightSizable)];
    [cv setWantsLayer:YES];
    [host addSubview:cv];

    [(NSWindow *)panel setOpaque:NO];
    [(NSWindow *)panel setBackgroundColor:[NSColor clearColor]];
    [(NSWindow *)panel setHasShadow:YES];

    // 缩放边界：太小布局会挤碎，太大就失去了"轻量面板"的形态。
    // 数值由 Go 侧传入（panel.go 是唯一定义处），避免两处常量各自漂移。
    [(NSWindow *)panel setContentMinSize:NSMakeSize(minw, minh)];
    [(NSWindow *)panel setContentMaxSize:NSMakeSize(maxw, maxh)];

    [(NSWindow *)panel setContentView:host];
    [host release];
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

    // 失焦自动收起的两条触发源（见「失焦自动收起」一节）。
    // 这里只报告"面板丢掉了键盘"，是否真的收起由 Go 侧按 ui.closeOnBlur 决定：
    // 策略留在 Go 里，三平台才有一致的语义，也不必把设置传进 C。
    NSNotificationCenter *nc = [NSNotificationCenter defaultCenter];
    [nc addObserverForName:NSWindowDidResignKeyNotification
                    object:panel
                     queue:nil
                usingBlock:^(__unused NSNotification *note) { PawClip_ScheduleBlurCheck(); }];
    [nc addObserverForName:NSApplicationDidResignActiveNotification
                    object:NSApp
                     queue:nil
                usingBlock:^(__unused NSNotification *note) { PawClip_ScheduleBlurCheck(); }];

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

// ── 底座颜色同步 ────────────────────────────────────────────────
// 底座必须跟前端卡片同色，否则"WebView 还没画出来的头几帧"会闪一块异色。
// 颜色的唯一真源是 DOM（body 的 --bg），从 WebView 里取回来就行，不必给
// Go/前端加任何接口。取不到时保持上一次的颜色（占位灰起步）。
static WKWebView *PawClip_FindWKWebView(NSView *root) {
    if ([root isKindOfClass:[WKWebView class]]) return (WKWebView *)root;
    for (NSView *sub in root.subviews) {
        WKWebView *found = PawClip_FindWKWebView(sub);
        if (found) return found;
    }
    return nil;
}

// cv 是 Wails 的 contentView（容器），真正的 WKWebView 在它下面。
// ⚠️ 不缓存引用：对 Wails 的 WailsWebView 调 isDescendantOfView: 会在运行时
// 抛 unrecognized selector（实测 SIGABRT，2026-09-18），所以任何"验证引用
// 还在视图树上"的手段都不能用——每次现找，浅层遍历，成本可忽略。
static WKWebView *PawClip_WKWebView(void) {
    if (!g_webView) return nil;
    return PawClip_FindWKWebView(g_webView);
}

static void PawClip_ApplyBackdropFromDOM(id res) {
    if (![res isKindOfClass:[NSString class]]) return;
    NSString *s = res;
    // 形如 "rgb(24, 24, 26)" / "rgba(24, 24, 26, 0.8)" / "#181a1a" 之外的
    // 颜色写法（webkit 的 getComputedStyle 对 backgroundColor 恒返回 rgb 形式）。
    NSRegularExpression *re = [NSRegularExpression regularExpressionWithPattern:@"[0-9.]+"
                                                                        options:0 error:nil];
    NSArray<NSTextCheckingResult *> *ms = [re matchesInString:s options:0
                                                        range:NSMakeRange(0, s.length)];
    if (ms.count < 3) return;
    CGFloat rgb[3];
    for (int i = 0; i < 3; i++) {
        double v = [[s substringWithRange:[ms[i] range]] doubleValue];
        rgb[i] = v > 1.0 ? v / 255.0 : v;   // 0..255 与 0..1 两种写法都兼容
    }
    g_backdropRGB[0] = rgb[0];
    g_backdropRGB[1] = rgb[1];
    g_backdropRGB[2] = rgb[2];
    if (g_cardLayer) {
        [g_cardLayer setBackgroundColor:[NSColor colorWithSRGBRed:rgb[0]
                                                            green:rgb[1]
                                                             blue:rgb[2]
                                                            alpha:1.0].CGColor];
    }
}

// 异步一次 JS 求值。顺带的好处：这会强制 WebContent 进程醒来应答一次，
// 对"面板重新呼出后内容没跟上"是最温和的一记催促。
//
// 整个函数包在 @try 里：这里是 hide/show 路径上的旁路逻辑，WebView 对象
// 出现任何意外（类不受信、selector 缺失）都不允许带崩收起/呼出本身。
static void PawClip_SyncBackdropFromDom(void) {
    WKWebView *wv = PawClip_WKWebView();
    if (!wv) return;
    @try {
        if (![(id)wv respondsToSelector:@selector(evaluateJavaScript:completionHandler:)]) return;
        [wv evaluateJavaScript:@"getComputedStyle(document.body).backgroundColor"
             completionHandler:^(id res, NSError *__unused err) {
                 PawClip_ApplyBackdropFromDOM(res);
             }];
    } @catch (NSException *__unused e) {
        // 拿不到就不拿：底座保持上一次的颜色，不会更糟。
    }
}

// ── 显示 / 隐藏 ─────────────────────────────────────────────────
// 顺序不能反：orderFrontRegardless 只把窗口提到其层级最前、不激活 App；
// 但窗口不在屏幕上时 makeKeyWindow 是**空操作**，所以必须先让它可见。
static int PawClip_ShowOnMain(void) {
    if (!g_panel) return 0;
    PawClip_SyncBackdropFromDom();   // 先取最新主题色（异步，落地在显示之后）
    [g_panel orderFrontRegardless];
    if ([g_panel canBecomeKeyWindow]) [g_panel makeKeyWindow];
    [g_panel invalidateShadow];      // 非不透明窗口的投影取自内容 alpha，别用残影
    return [g_panel isKeyWindow] ? 1 : 0;
}

int paw_show(void) {
    __block int r = 0;
    PawClip_OnMain(^{
        PawClip_Diag("show-before");
        PawClip_ShowOnMain();
        r = [g_panel isKeyWindow] ? 1 : 0;
        PawClip_Diag(r ? "show-after(key)" : "show-after(no-key)");
    });
    return r;
}

void paw_hide(void) {
    PawClip_OnMain(^{
        PawClip_Diag("hide-before");
        if (g_panel) [g_panel orderOut:nil];
        // 收起之后再看一眼：这里 webInPanel 若变成 0，说明 WebView 已经不在
        // 面板的视图树里，下次呼出必然是空壳（而不是"内容没来得及合成"）。
        PawClip_Diag("hide-after");
        // 收起时顺手同步一次底座色：下次呼出的头几帧就用得上，
        // 也把"主题刚切过、呼出时才第一次取"的竞态压掉。
        PawClip_SyncBackdropFromDom();
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

// ── 失焦自动收起 ────────────────────────────────────────────────
// 需求：点到面板以外的任何地方（别的 App、桌面、别的窗口），面板自己收起。
//
// 判据**不能**用 NSApp.isActive。面板是 NonactivatingPanel：它拿键盘、
// 却不把 App 切到前台（docs/DESIGN.md §0.2），所以"用户点走了"这件事在
// App 层面基本看不见——只能盯面板自己的 key 状态。两条通知都要听：
//
//   · NSWindowDidResignKeyNotification —— 点别的 App / 桌面 / 本 App 的
//     别的窗口，面板丢掉 key。这是主路径。
//   · NSApplicationDidResignActiveNotification —— App 整体失去激活
//     （打开 Spotlight、切换 Space、系统弹窗抢了前台），这时面板可能连
//     key 都还没拿到，不会有上一条。
//
// 判定**延后 80ms** 再上报：一次点击可能同时触发两条通知，拖动与新建
// 窗口时 key 状态也可能瞬时抖动；合并掉既避免重复上报，也留出
// "再过一帧是不是又变回 key 了"的复核机会（复核不过就不报）。
static BOOL g_blurPending = NO;

static void PawClip_ReportBlur(void) {
    if (g_panel == nil) { PawClip_Diag("blur-drop(no-panel)"); return; }
    if (![g_panel isVisible]) { PawClip_Diag("blur-drop(hidden)"); return; }   // 已经收起了，没什么可报的
    if ([g_panel isKeyWindow]) { PawClip_Diag("blur-drop(still-key)"); return; }  // 复核：key 又回来了（抖动）
    // 有模态窗口时不算"用户点走了"：导出/导入要弹系统的文件面板，它会把
    // key 从面板手里拿走，那一刻顺手收起面板等于把用户的操作上下文弄丢。
    //
    // 两种形态都要挡：runModal 式的（NSApp.modalWindow）与 sheet 式的
    // （附在某个窗口上的 attachedSheet —— 它不注册成 modalWindow）。
    if ([NSApp modalWindow] != nil || [g_panel attachedSheet] != nil) {
        PawClip_Diag("blur-drop(modal)");
        return;
    }
    PawClip_Diag("blur-REPORT");
    pawGoPanelBlur();
}

static void PawClip_ScheduleBlurCheck(void) {
    if (g_blurPending) return;
    if (g_panel == nil || ![g_panel isVisible]) {
        PawClip_Diag("blur-skip(hidden)");
        return;
    }
    if ([g_panel isKeyWindow]) {
        PawClip_Diag("blur-skip(still-key)");
        return;
    }
    PawClip_Diag("blur-scheduled");
    g_blurPending = YES;
    dispatch_after(dispatch_time(DISPATCH_TIME_NOW, (int64_t)(80 * NSEC_PER_MSEC)),
                   dispatch_get_main_queue(), ^{
                       g_blurPending = NO;
                       PawClip_ReportBlur();
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
