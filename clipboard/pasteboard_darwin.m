#import "pasteboard_darwin.h"

#import <AppKit/AppKit.h>
#import <Foundation/Foundation.h>
#import <CoreGraphics/CoreGraphics.h>

#import <stdarg.h>
#import <stdio.h>
#import <stdlib.h>
#import <string.h>

// 旧式文件名类型常量。用字面量而不是已有宏，是为了避开陈旧 API 的 deprecation 告警。
static NSString *const kPawClipFilenamesPboardType = @"NSFilenamesPboardType";

// ── 缓存的 pasteboard 指针 ──────────────────────────────────────
//
// 热路径必须走它。每次调 [NSPasteboard generalPasteboard] 都会返回一个
// autoreleased 对象——在 0.2s 一次的轮询里反复调用会持续产生自动释放对象，
// 正是 §14 第 9 条要避免的那类分配。
static NSPasteboard *g_pb = nil;

static NSPasteboard *PawClipPasteboard(void) {
    if (g_pb == nil) {
        g_pb = [[NSPasteboard generalPasteboard] retain];
    }
    return g_pb;
}

// ── 工具 ────────────────────────────────────────────────────────

static void PawClipSetErr(char *err, int errlen, const char *fmt, ...) {
    if (err == NULL || errlen <= 0) return;
    va_list ap;
    va_start(ap, fmt);
    vsnprintf(err, (size_t)errlen, fmt, ap);
    va_end(ap);
}

// 把 NSString 复制成 malloc 的 UTF-8 C 字符串。
static char *PawClipDupString(NSString *s) {
    if (s == nil) return NULL;
    const char *utf8 = [s UTF8String];
    if (utf8 == NULL) return NULL;
    return strdup(utf8);
}

// 把 NSArray 序列化成 JSON 字符串。
//
// 手写转义很容易在路径里的引号 / 反斜杠 / 非 ASCII 上翻车，所以交给
// NSJSONSerialization。返回的是 malloc 的字符串，调用方负责 free。
static char *PawClipJSONArray(NSArray *arr) {
    if (arr == nil || [arr count] == 0) return NULL;
    if (![NSJSONSerialization isValidJSONObject:arr]) return NULL;

    NSError *error = nil;
    NSData *data = [NSJSONSerialization dataWithJSONObject:arr options:0 error:&error];
    if (data == nil) return NULL;

    NSUInteger n = [data length];
    char *out = (char *)malloc(n + 1);
    if (out == NULL) return NULL;
    memcpy(out, [data bytes], n);
    out[n] = '\0';
    return out;
}

static void *PawClipDupBytes(NSData *data, int *outLen) {
    if (data == nil || outLen == NULL) return NULL;
    NSUInteger n = [data length];
    if (n == 0) return NULL;
    void *out = malloc(n);
    if (out == NULL) return NULL;
    memcpy(out, [data bytes], n);
    *outLen = (int)n;
    return out;
}

// ── 热路径 ──────────────────────────────────────────────────────

long pb_change_count(void) {
    NSPasteboard *pb = g_pb;
    if (pb == nil) {
        pb = PawClipPasteboard();
    }
    // 纯 ivar 读取：不碰 autorelease、不构造对象、不需要 autoreleasepool。
    return (long)[pb changeCount];
}

double pb_seconds_since_last_input(void) {
    return (double)CGEventSourceSecondsSinceLastEventType(kCGEventSourceStateCombinedSessionState,
                                                         kCGAnyInputEventType);
}

// ── 读取 ────────────────────────────────────────────────────────

int pb_read(pb_snapshot *out, char *err, int errlen) {
    if (out == NULL) {
        PawClipSetErr(err, errlen, "null snapshot pointer");
        return 1;
    }
    memset(out, 0, sizeof(*out));

    // 读取路径会构造大量 Objective-C 对象，必须有 autoreleasepool，
    // 否则从 Go 的非主线程调用时会持续累积自动释放对象。
    @autoreleasepool {
        NSPasteboard *pb = PawClipPasteboard();
        if (pb == nil) {
            PawClipSetErr(err, errlen, "NSPasteboard.generalPasteboard is nil");
            return 1;
        }
        out->change_count = (long)[pb changeCount];

        // ① 来源应用：必须在同一帧读（DESIGN.md §7 第 2 条）。
        //    放在读内容之前，把"变更发生"到"记下是谁复制的"之间的窗口压到最小。
        NSRunningApplication *app = [[NSWorkspace sharedWorkspace] frontmostApplication];
        if (app != nil) {
            out->app_id = PawClipDupString([app bundleIdentifier]);
            out->app_name = PawClipDupString([app localizedName]);
        }

        // ② 原始类型名：保密标记判定与诊断都要用
        NSArray *types = [pb types];
        if (types != nil) {
            out->types_json = PawClipJSONArray(types);
        }

        // ③ 文本类
        NSString *text = [pb stringForType:NSPasteboardTypeString];
        if (text != nil) {
            out->text = PawClipDupString(text);
        }
        // macOS 的 HTML 已经是纯 HTML，没有 Windows 那个 CF_HTML 头
        NSString *html = [pb stringForType:NSPasteboardTypeHTML];
        if (html != nil) {
            out->html = PawClipDupString(html);
        }
        NSData *rtf = [pb dataForType:NSPasteboardTypeRTF];
        if (rtf != nil) {
            out->rtf = PawClipDupBytes(rtf, &out->rtf_len);
        }

        // ④ 图片：一律归一化成 PNG。
        //    优先取 public.png（我们自己回写时放的就是它，字节完全一致，
        //    这一点直接决定 SelfWriteGuard 能不能靠指纹比对命中）；
        //    否则把 public.tiff / public.jpeg 转成 PNG。
        NSData *png = [pb dataForType:NSPasteboardTypePNG];
        if (png == nil) {
            NSData *tiff = [pb dataForType:NSPasteboardTypeTIFF];
            if (tiff != nil) {
                NSBitmapImageRep *rep = [NSBitmapImageRep imageRepWithData:tiff];
                if (rep != nil) {
                    png = [rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
                }
            }
        }
        if (png == nil) {
            // AppKit 没有 NSPasteboardTypeJPEG 常量，直接用 UTI 字面量。
            NSData *jpeg = [pb dataForType:@"public.jpeg"];
            if (jpeg != nil) {
                NSBitmapImageRep *rep = [NSBitmapImageRep imageRepWithData:jpeg];
                if (rep != nil) {
                    png = [rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
                }
            }
        }
        if (png != nil) {
            out->png = PawClipDupBytes(png, &out->png_len);
        }

        // ⑤ 文件列表
        NSArray<NSURL *> *urls =
            [pb readObjectsForClasses:@[ [NSURL class] ]
                              options:@{NSPasteboardURLReadingFileURLsOnlyKey : @YES}];
        if (urls != nil && [urls count] > 0) {
            NSMutableArray *paths = [NSMutableArray arrayWithCapacity:[urls count]];
            for (NSURL *u in urls) {
                NSString *p = [u path];
                if (p != nil) {
                    [paths addObject:p];
                }
            }
            if ([paths count] > 0) {
                out->files_json = PawClipJSONArray(paths);
            }
        }
        if (out->files_json == NULL) {
            // 旧式类型兜底：个别老应用只放这个
            id plist = [pb propertyListForType:kPawClipFilenamesPboardType];
            if ([plist isKindOfClass:[NSArray class]]) {
                out->files_json = PawClipJSONArray(plist);
            }
        }
    }
    return 0;
}

void pb_snapshot_free(pb_snapshot *s) {
    if (s == NULL) return;
    free(s->types_json);
    free(s->text);
    free(s->html);
    free(s->rtf);
    free(s->png);
    free(s->files_json);
    free(s->app_id);
    free(s->app_name);
    memset(s, 0, sizeof(*s));
}

// ── 写回 ────────────────────────────────────────────────────────

int pb_write(const char *text,
             const char *html,
             const void *rtf, int rtf_len,
             const void *png, int png_len,
             const char *const *files, int files_n,
             char *err, int errlen) {
    @autoreleasepool {
        NSPasteboard *pb = PawClipPasteboard();
        if (pb == nil) {
            PawClipSetErr(err, errlen, "NSPasteboard.generalPasteboard is nil");
            return 1;
        }
        [pb clearContents];

        if (text != NULL) {
            NSString *s = [NSString stringWithUTF8String:text];
            if (s == nil) {
                PawClipSetErr(err, errlen, "text is not valid UTF-8");
                return 1;
            }
            [pb setString:s forType:NSPasteboardTypeString];
        }
        if (html != NULL) {
            NSString *s = [NSString stringWithUTF8String:html];
            if (s == nil) {
                PawClipSetErr(err, errlen, "html is not valid UTF-8");
                return 1;
            }
            [pb setString:s forType:NSPasteboardTypeHTML];
        }
        if (rtf != NULL && rtf_len > 0) {
            NSData *d = [NSData dataWithBytes:rtf length:(NSUInteger)rtf_len];
            [pb setData:d forType:NSPasteboardTypeRTF];
        }
        if (png != NULL && png_len > 0) {
            NSData *d = [NSData dataWithBytes:png length:(NSUInteger)png_len];
            [pb setData:d forType:NSPasteboardTypePNG];
        }
        if (files != NULL && files_n > 0) {
            NSMutableArray *paths = [NSMutableArray arrayWithCapacity:(NSUInteger)files_n];
            for (int i = 0; i < files_n; i++) {
                if (files[i] == NULL) continue;
                NSString *p = [NSString stringWithUTF8String:files[i]];
                if (p != nil) {
                    [paths addObject:p];
                }
            }
            if ([paths count] > 0) {
                [pb setPropertyList:paths forType:kPawClipFilenamesPboardType];
            }
        }
    }
    return 0;
}

void pb_release(void *p) {
    if (p != NULL) free(p);
}
