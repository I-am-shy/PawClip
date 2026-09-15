#import "panel_darwin.h"
#import <Cocoa/Cocoa.h>
#import <objc/runtime.h>
#import <string.h>
#import <stdlib.h>
#import <stdio.h>

static NSWindow *g_panel       = nil;
static NSWindow *g_wailsWindow = nil; // adopt 模式下保留原窗口，避免被释放
static Class     g_panelCls    = Nil;
static int       g_mode        = PAWCLIP_MODE_BASELINE;
static int       g_accessory   = 0;

// 逐步日志：崩溃时靠 stderr 的无缓冲输出定位到具体哪一步
static double PawClip_Now(void) {
    static CFAbsoluteTime t0 = 0;
    if (t0 == 0) t0 = CFAbsoluteTimeGetCurrent();
    return CFAbsoluteTimeGetCurrent() - t0;
}
#define STEP(fmt, ...) do { \
    fprintf(stderr, "[STEP %7.2fs] " fmt "\n", PawClip_Now(), ##__VA_ARGS__); \
    fflush(stderr); \
} while (0)

// ── NSPanel 动态子类 ────────────────────────────────────────────
// canBecomeKeyWindow / canBecomeMainWindow 必须返回 YES，
// 否则 nonactivating panel 下的搜索框收不到键盘。
static BOOL PawClip_canBecomeKey(id self, SEL _cmd)  { return YES; }
static BOOL PawClip_canBecomeMain(id self, SEL _cmd) { return YES; }

static Class PawClip_EnsurePanelClass(char *err, int errlen) {
    if (g_panelCls) return g_panelCls;

    Class base = NSClassFromString(@"NSPanel");
    if (!base) {
        snprintf(err, errlen, "NSPanel class not found");
        return Nil;
    }
    Class existing = NSClassFromString(@"PawClipPanel");
    if (existing) { g_panelCls = existing; return existing; }

    Class cls = objc_allocateClassPair(base, "PawClipPanel", 0);
    if (!cls) {
        snprintf(err, errlen, "objc_allocateClassPair failed");
        return Nil;
    }

    // encoding 随平台可能是 "B"(bool) 或 "c"(signed char)，交给编译器决定
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

// ── 取 Wails 的主窗口 ───────────────────────────────────────────
static NSWindow *PawClip_MainWindow(void) {
    for (NSWindow *w in [NSApp windows]) {
        if (w.contentView != nil) return w;
    }
    return nil;
}

int pawclip_has_window(void) {
    return PawClip_MainWindow() != nil ? 1 : 0;
}

// ── 路线 D：新建真 NSPanel，接管 Wails 的 contentView ────────────
// 思路：NSPanel 必须走正常的 alloc/init 才能正确初始化内部状态。
// 所以不动 Wails 的窗口，而是把它的 contentView（含 WKWebView）搬到
// 我们自己创建的真 NSPanel 上，再把 Wails 的窗口隐藏掉。
static int PawClip_AdoptWebView(NSWindow *w, char *err, int errlen) {
    NSView *cv = [w contentView];
    if (!cv) { snprintf(err, errlen, "contentView is nil"); return 5; }
    STEP("adopt: wails contentView class=%s frame=%.0fx%.0f",
         class_getName(object_getClass(cv)), [cv frame].size.width, [cv frame].size.height);

    NSRect frame = [w frame];

    Class cls = PawClip_EnsurePanelClass(err, errlen);
    if (!cls) return 4;

    STEP("adopt: creating real NSPanel (alloc/init)");
    id panel = [[cls alloc] initWithContentRect:frame
                                      styleMask:(NSWindowStyleMaskNonactivatingPanel |
                                                 NSWindowStyleMaskBorderless)
                                        backing:NSBackingStoreBuffered
                                          defer:NO];
    if (!panel) { snprintf(err, errlen, "NSPanel init returned nil"); return 6; }
    STEP("adopt: NSPanel created, class=%s", class_getName(object_getClass(panel)));

    // 保留 Wails 窗口（避免被释放），并让它腾出 contentView
    g_wailsWindow = [w retain];
    [cv retain];
    [w setContentView:[[[NSView alloc] initWithFrame:frame] autorelease]];
    STEP("adopt: wails window detached from webview");

    [(NSWindow *)panel setContentView:cv];
    [cv release];
    STEP("adopt: webview re-attached to panel");

    [(NSPanel *)panel setBecomesKeyOnlyIfNeeded:NO]; // 面板一显示就接收键盘，不等点击
    [(NSWindow *)panel setLevel:NSFloatingWindowLevel];
    [(NSWindow *)panel setHidesOnDeactivate:NO];
    [(NSWindow *)panel setCollectionBehavior:(NSWindowCollectionBehaviorCanJoinAllSpaces |
                                              NSWindowCollectionBehaviorFullScreenAuxiliary)];
    [(NSWindow *)panel center];
    [w orderOut:nil];
    STEP("adopt: wails window ordered out");

    g_panel = panel;
    STEP("adopt complete");
    return 0;
}

// ── 核心：应用处理方案（必须在主线程执行）───────────────────────
static int PawClip_SetupOnMain(int mode, int accessory, char *err, int errlen) {
    STEP("setup enter: mode=%d accessory=%d", mode, accessory);

    NSWindow *w = PawClip_MainWindow();
    if (!w) {
        snprintf(err, errlen, "no window in NSApp.windows");
        return 3;
    }
    STEP("target window class=%s instanceSize=%zu",
         class_getName(object_getClass(w)), class_getInstanceSize(object_getClass(w)));

    g_panel     = w;
    g_mode      = mode;
    g_accessory = accessory;

    if (accessory) {
        STEP("setActivationPolicy:Accessory");
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
        STEP("activationPolicy done");
    }

    if (mode == PAWCLIP_MODE_BASELINE) {
        STEP("baseline: no-op");
        return 0;
    }

    if (mode == PAWCLIP_MODE_ADOPT) {
        return PawClip_AdoptWebView(w, err, errlen);
    }

    if (mode == PAWCLIP_MODE_PANEL || mode == PAWCLIP_MODE_PANELMIN) {
        Class cls = PawClip_EnsurePanelClass(err, errlen);
        if (!cls) return 4;
        STEP("object_setClass -> %s", class_getName(cls));
        object_setClass(w, cls);
        STEP("object_setClass done, class now=%s", class_getName(object_getClass(w)));
    }

    STEP("read styleMask=%lu", (unsigned long)[w styleMask]);
    NSWindowStyleMask mask = [w styleMask];
    mask |= NSWindowStyleMaskNonactivatingPanel;
    STEP("setStyleMask -> %lu", (unsigned long)mask);
    [w setStyleMask:mask];
    STEP("setStyleMask done, now=%lu", (unsigned long)[w styleMask]);

    if (mode == PAWCLIP_MODE_PANEL) {
        STEP("setBecomesKeyOnlyIfNeeded:NO");
        if ([w respondsToSelector:@selector(setBecomesKeyOnlyIfNeeded:)])
            [(NSPanel *)w setBecomesKeyOnlyIfNeeded:NO];
        STEP("setFloatingPanel:YES");
        if ([w respondsToSelector:@selector(setFloatingPanel:)])
            [(NSPanel *)w setFloatingPanel:YES];
        STEP("panel-specific opts done");
    }

    STEP("setHidesOnDeactivate:NO");
    [w setHidesOnDeactivate:NO];
    STEP("setLevel:Floating");
    [w setLevel:NSFloatingWindowLevel];
    STEP("setCollectionBehavior");
    [w setCollectionBehavior:(NSWindowCollectionBehaviorCanJoinAllSpaces |
                             NSWindowCollectionBehaviorFullScreenAuxiliary)];
    STEP("setup complete");
    return 0;
}

// block 不允许捕获数组类型，所以把错误缓冲区包进 struct 再用 __block 共享
typedef struct { char msg[512]; } PawErrBuf;

int pawclip_setup(int mode, int accessory, char *err, int errlen) {
    __block int rc = 0;
    __block PawErrBuf eb;
    eb.msg[0] = '\0';

    dispatch_block_t blk = ^{
        rc = PawClip_SetupOnMain(mode, accessory, eb.msg, (int)sizeof(eb.msg));
    };

    if ([NSThread isMainThread]) {
        blk();
    } else {
        dispatch_sync(dispatch_get_main_queue(), blk);
    }

    if (rc != 0) snprintf(err, errlen, "%s", eb.msg);
    return rc;
}

// ── 显示与隐藏 ──────────────────────────────────────────────────
static void PawClip_HideOnMain(void) {
    if (g_panel) [g_panel orderOut:nil];
}

void pawclip_hide(void) {
    if ([NSThread isMainThread]) PawClip_HideOnMain();
    else dispatch_sync(dispatch_get_main_queue(), ^{ PawClip_HideOnMain(); });
}

// orderFrontRegardless 只把窗口提到其层级最前，不改变 key/main window、不激活 App
static int PawClip_ShowOnMain(void) {
    if (!g_panel) return 0;

    // 顺序很重要：窗口不在屏幕上时 makeKeyWindow 是空操作，
    // 所以必须先 orderFrontRegardless 让它可见，再去取 key。
    [g_panel orderFrontRegardless];

    if ([g_panel canBecomeKeyWindow]) {
        [g_panel makeKeyWindow];
    }
    return [g_panel isKeyWindow] ? 1 : 0;
}

int pawclip_show_noactivate(void) {
    __block int r = 0;
    if ([NSThread isMainThread]) {
        r = PawClip_ShowOnMain();
    } else {
        dispatch_sync(dispatch_get_main_queue(), ^{ r = PawClip_ShowOnMain(); });
    }
    return r;
}

// 显式激活本 App —— 用来分离"App 激活"与"窗口成为 key"两个因素
void pawclip_activate_app(void) {
    void (^blk)(void) = ^{
        STEP("explicit [NSApp activateIgnoringOtherApps:YES]");
        [NSApp activateIgnoringOtherApps:YES];
    };
    if ([NSThread isMainThread]) blk();
    else dispatch_sync(dispatch_get_main_queue(), blk);
}

// 只尝试拿 key window，不碰激活状态
int pawclip_make_key(void) {
    __block int r = 0;
    void (^blk)(void) = ^{
        if (!g_panel) return;
        [g_panel orderFrontRegardless];
        [g_panel makeKeyWindow];
        r = [g_panel isKeyWindow] ? 1 : 0;
        STEP("make_key -> isKeyWindow=%d (appActive=%d)", r, [NSApp isActive]);
    };
    if ([NSThread isMainThread]) blk();
    else dispatch_sync(dispatch_get_main_queue(), blk);
    return r;
}

// ── 诊断快照 ────────────────────────────────────────────────────
static char *PawClip_DiagOnMain(void) {
    NSString *selfID = [[NSBundle mainBundle] bundleIdentifier] ?: @"";
    NSRunningApplication *front = [[NSWorkspace sharedWorkspace] frontmostApplication];
    NSString *frontID   = front.bundleIdentifier ?: @"";
    NSString *frontName = front.localizedName    ?: @"";

    NSString *cls = g_panel ? NSStringFromClass([g_panel class]) : @"(nil)";
    BOOL  isActive = [NSApp isActive];
    BOOL  isKey    = g_panel ? [g_panel isKeyWindow] : NO;
    BOOL  canKey   = g_panel ? [g_panel canBecomeKeyWindow] : NO;
    BOOL  visible  = g_panel ? [g_panel isVisible] : NO;
    long  mask     = g_panel ? (long)[g_panel styleMask] : 0;
    BOOL  nonact   = (mask & NSWindowStyleMaskNonactivatingPanel) != 0;
    long  behavior = g_panel ? (long)[g_panel collectionBehavior] : 0;

    size_t winSz   = class_getInstanceSize([NSWindow class]);
    size_t panelSz = class_getInstanceSize(NSClassFromString(@"NSPanel"));
    size_t actSz   = g_panel ? class_getInstanceSize(object_getClass(g_panel)) : 0;

    NSString *json = [NSString stringWithFormat:
        @"{\"mode\":%d,\"accessory\":%d,\"appBundleID\":\"%@\","
         "\"windowClass\":\"%@\",\"instanceSize\":%zu,\"nsWindowSize\":%zu,"
         "\"nsPanelSize\":%zu,\"styleMask\":%ld,\"nonactivatingBit\":%d,"
         "\"collectionBehavior\":%ld,\"nsAppIsActive\":%d,\"windowIsKey\":%d,"
         "\"canBecomeKey\":%d,\"windowVisible\":%d,"
         "\"frontmostBundleID\":\"%@\",\"frontmostName\":\"%@\"}",
        g_mode, g_accessory, selfID,
        cls, actSz, winSz, panelSz, mask, nonact, behavior,
        isActive, isKey, canKey, visible, frontID, frontName];

    return strdup([json UTF8String]);
}

char *pawclip_diag_json(void) {
    __block char *out = NULL;
    if ([NSThread isMainThread]) {
        out = PawClip_DiagOnMain();
    } else {
        dispatch_sync(dispatch_get_main_queue(), ^{ out = PawClip_DiagOnMain(); });
    }
    return out;
}

void pawclip_free(char *p) { if (p) free(p); }
