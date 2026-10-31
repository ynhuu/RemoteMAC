#import <AppKit/AppKit.h>
#import <dlfcn.h>
#import <CoreGraphics/CoreGraphics.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#import <Foundation/Foundation.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <VideoToolbox/VideoToolbox.h>
#import <mach/mach_time.h>
#import <stddef.h>
#import <sys/file.h>
#import <fcntl.h>
#import <unistd.h>
#import <math.h>
#import <stdlib.h>
#import <string.h>
#include "_cgo_export.h"
#include "VirtualDisplayBridge.h"

static const uint32_t RMVendorID = SRDisplayVendorID;
static const uint32_t RMProductID = SRDisplayProductID;
static const uint32_t RMSerialNumber = SRDisplaySerialNumber;
static NSMutableSet<id> *RMRetiredSessions;

static CGEventSourceRef RMRemoteInputEventSource(void) {
    static CGEventSourceRef source = NULL;
    static dispatch_once_t onceToken;
    dispatch_once(&onceToken, ^{
        source = CGEventSourceCreate(kCGEventSourceStatePrivate);
    });
    return source;
}

static void RMSetError(char *buffer, size_t length, NSString *message) {
    if (!buffer || length == 0) return;
    const char *utf8 = message.UTF8String ?: "macOS 原生桌面桥接失败";
    if (strlcpy(buffer, utf8, length) < length || length < 2) return;

    size_t end = length - 1;
    size_t start = end - 1;
    while (start > 0 && ((unsigned char)buffer[start] & 0xC0) == 0x80) start--;
    unsigned char lead = (unsigned char)buffer[start];
    size_t expected = (lead & 0x80) == 0 ? 1 : (lead & 0xE0) == 0xC0 ? 2 :
        (lead & 0xF0) == 0xE0 ? 3 : (lead & 0xF8) == 0xF0 ? 4 : 1;
    if (end - start < expected) buffer[start] = '\0';
}

static NSString *RMStringFromUTF8(const char *value, NSString *fallback) {
    return value && value[0] ? ([NSString stringWithUTF8String:value] ?: fallback) : fallback;
}

int rm_set_cursor_scale(float scale) {
    int (*connection)(void) = dlsym(RTLD_DEFAULT, "CGSMainConnectionID");
    CGError (*setScale)(int, float) = dlsym(RTLD_DEFAULT, "CGSSetCursorScale");
    if (!connection || !setScale) return -1;
    return setScale(connection(), scale) == kCGErrorSuccess ? 0 : -1;
}

static float RMCursorScaleForResolution(uint32_t width, uint32_t height) {
    uint32_t longestEdge = width > height ? width : height;
    float scale = (float)longestEdge / 960.0f;
    return scale < 1.0f ? 1.0f : scale > 2.0f ? 2.0f : scale;
}

static BOOL RMConfigureMirror(CGDirectDisplayID target, CGDirectDisplayID source, char *error, size_t errorLength) {
    CGDisplayConfigRef config = NULL;
    CGError status = CGBeginDisplayConfiguration(&config);
    if (status != kCGErrorSuccess || !config) {
        RMSetError(error, errorLength, [NSString stringWithFormat:@"开始镜像配置失败：%d", status]);
        return NO;
    }
    status = CGConfigureDisplayMirrorOfDisplay(config, target, source);
    if (status != kCGErrorSuccess) {
        CGCancelDisplayConfiguration(config);
        RMSetError(error, errorLength, [NSString stringWithFormat:@"设置虚拟屏幕镜像关系失败：%d", status]);
        return NO;
    }
    status = CGCompleteDisplayConfiguration(config, kCGConfigureForAppOnly);
    if (status != kCGErrorSuccess) {
        RMSetError(error, errorLength, [NSString stringWithFormat:@"提交虚拟屏幕镜像关系失败：%d", status]);
        return NO;
    }
    return YES;
}

static uint64_t RMDeadlineAfter(double seconds) {
    mach_timebase_info_data_t timebase = {0};
    mach_timebase_info(&timebase);
    return mach_absolute_time() + (uint64_t)(seconds * 1000000000.0 * (double)timebase.denom / (double)timebase.numer);
}

static BOOL RMWaitForMirror(CGDirectDisplayID target, CGDirectDisplayID expected, uint64_t stableNanos) {
    uint64_t started = 0;
    mach_timebase_info_data_t timebase = {0};
    mach_timebase_info(&timebase);
    uint64_t deadline = RMDeadlineAfter(3.0);
    while (mach_absolute_time() < deadline) {
        uint64_t now = mach_absolute_time();
        if (CGDisplayMirrorsDisplay(target) == expected) {
            if (!started) started = now;
            uint64_t elapsedNanos = (now - started) * timebase.numer / timebase.denom;
            if (elapsedNanos >= stableNanos) return YES;
        } else {
            started = 0;
        }
        CFRunLoopRunInMode(kCFRunLoopDefaultMode, 0.025, true);
    }
    return NO;
}

static BOOL RMConfigureMirrorAndWait(CGDirectDisplayID target, CGDirectDisplayID source,
                                     CGDirectDisplayID savedSource, uint64_t stableNanos,
                                     char *error, size_t errorLength) {
    if (!RMConfigureMirror(target, source, error, errorLength)) return NO;
    if (RMWaitForMirror(target, source, stableNanos)) return YES;

    CGDirectDisplayID actual = CGDisplayMirrorsDisplay(target);
    if (actual != savedSource && actual != source) {
        RMSetError(error, errorLength, [NSString stringWithFormat:
            @"等待虚拟屏幕镜像关系稳定超时，保留当前布局：target=%u，saved=%u，expected=%u，actual=%u",
            target, savedSource, source, actual]);
        return NO;
    }

    // CoreGraphics can accept a mirror transaction before WindowServer
    // publishes a stable edge. Retry once only while the original edge remains
    // or the requested edge is already selected; preserve every other source.
    if (actual == source) {
        NSLog(@"实体镜像目标已切到虚拟屏幕但尚未稳定，再等待一次：target=%u，source=%u",
              target, source);
        if (RMWaitForMirror(target, source, stableNanos)) return YES;
        RMSetError(error, errorLength, [NSString stringWithFormat:
            @"镜像关系已切到虚拟屏幕但未能稳定：target=%u，source=%u，actual=%u",
            target, source, CGDisplayMirrorsDisplay(target)]);
        return NO;
    }
    NSLog(@"实体镜像目标仍保持原关系，稳定后重试一次：target=%u，source=%u，actual=%u",
          target, source, actual);
    if (!RMWaitForMirror(target, savedSource, 500000000)) {
        RMSetError(error, errorLength, [NSString stringWithFormat:
            @"重试镜像配置前拓扑未稳定：target=%u，saved=%u，actual=%u",
            target, savedSource, CGDisplayMirrorsDisplay(target)]);
        return NO;
    }
    if (!RMConfigureMirror(target, source, error, errorLength)) return NO;
    if (RMWaitForMirror(target, source, stableNanos)) return YES;

    RMSetError(error, errorLength, [NSString stringWithFormat:
        @"重试后虚拟屏幕镜像关系仍未稳定：target=%u，source=%u，actual=%u",
        target, source, CGDisplayMirrorsDisplay(target)]);
    return NO;
}

static BOOL RMWaitForMirrorOffMain(CGDirectDisplayID target, CGDirectDisplayID expected, uint64_t stableNanos) {
    uint64_t started = 0;
    mach_timebase_info_data_t timebase = {0};
    mach_timebase_info(&timebase);
    uint64_t deadline = RMDeadlineAfter(3.0);
    while (mach_absolute_time() < deadline) {
        uint64_t now = mach_absolute_time();
        if (CGDisplayMirrorsDisplay(target) == expected) {
            if (!started) started = now;
            uint64_t elapsedNanos = (now - started) * timebase.numer / timebase.denom;
            if (elapsedNanos >= stableNanos) return YES;
        } else {
            started = 0;
        }
        usleep(25000);
    }
    return NO;
}

static BOOL RMConfigureHiDPIMode(CGDirectDisplayID displayID, uint32_t pixelWidth, uint32_t pixelHeight,
                                 uint32_t logicalWidth, uint32_t logicalHeight, float density,
                                 char *error, size_t errorLength) {
    uint64_t deadline = RMDeadlineAfter(3.0);
    SRDisplayModeResult result = {0};
    while (mach_absolute_time() < deadline) {
        SRSelectDisplayMode(displayID, logicalWidth, logicalHeight, density, &result);
        if (result.status == 1 && CGDisplayIsOnline(displayID)
            && result.activePixelWidth == pixelWidth
            && result.activePixelHeight == pixelHeight) {
            NSLog(@"RemoteMAC 活动模式读回核验通过：mode=%d/%d，logical=%ux%u，active=%dx%d→%ux%u，density=%.2f，usable=%u，flags=0x%x，CGDisplayPixels=%zux%zu",
                  result.targetIndex, result.modeCount, logicalWidth, logicalHeight,
                  result.activeWidth, result.activeHeight, result.activePixelWidth,
                  result.activePixelHeight, result.activeDensity,
                  result.targetIsUsable, result.targetIOFlags,
                  CGDisplayPixelsWide(displayID), CGDisplayPixelsHigh(displayID));
            return YES;
        }

        // WindowServer may reject the exact requested mode while keeping a
        // nearby usable HiDPI mode active. Reuse it when its backing can still
        // supply the requested capture size and its aspect ratio is preserved.
        if (density >= 1.99f && CGDisplayIsOnline(displayID)) {
            CGDisplayModeRef activeMode = CGDisplayCopyDisplayMode(displayID);
            if (activeMode) {
                const size_t activeWidth = CGDisplayModeGetWidth(activeMode);
                const size_t activeHeight = CGDisplayModeGetHeight(activeMode);
                const size_t activePixelWidth = CGDisplayModeGetPixelWidth(activeMode);
                const size_t activePixelHeight = CGDisplayModeGetPixelHeight(activeMode);
                const double activeDensityX = activeWidth > 0
                    ? (double)activePixelWidth / (double)activeWidth : 0;
                const double activeDensityY = activeHeight > 0
                    ? (double)activePixelHeight / (double)activeHeight : 0;
                const double targetAspect = pixelHeight > 0
                    ? (double)pixelWidth / (double)pixelHeight : 0;
                const double activeAspect = activePixelHeight > 0
                    ? (double)activePixelWidth / (double)activePixelHeight : 0;
                const BOOL activeModeIsUsable = CGDisplayModeIsUsableForDesktopGUI(activeMode);
                const BOOL compatibleHiDPI = activeModeIsUsable
                    && activeDensityX >= 1.99 && activeDensityY >= 1.99
                    && activePixelWidth >= pixelWidth && activePixelHeight >= pixelHeight
                    && (uint64_t)activePixelWidth * activePixelHeight <= SR_MAXIMUM_BACKING_AREA
                    && activeWidth <= pixelWidth && activeHeight <= pixelHeight
                    && targetAspect > 0
                    && fabs(activeAspect / targetAspect - 1.0) <= 0.005;
                if (compatibleHiDPI) {
                    NSLog(@"RemoteMAC 请求模式未激活，采用 WindowServer 当前可用的 HiDPI 模式：请求=%ux%u，active=%zux%zu→%zux%zu，density=%.2f，usable=1，CGDisplayPixels=%zux%zu",
                          pixelWidth, pixelHeight, activeWidth, activeHeight,
                          activePixelWidth, activePixelHeight, activeDensityX,
                          CGDisplayPixelsWide(displayID), CGDisplayPixelsHigh(displayID));
                    CFRelease(activeMode);
                    return YES;
                }
                CFRelease(activeMode);
            }
        }
        if (result.status < 0) break;
        CFRunLoopRunInMode(kCFRunLoopDefaultMode, 0.1, true);
    }
    RMSetError(error, errorLength, [NSString stringWithFormat:
        @"WindowServer 未激活请求的显示模式：status=%d，mode=%dx%d@%.1f，active=%dx%d→%ux%u@%.2f，CGDisplayPixels=%zux%zu，配置=%d，usable=%u，flags=0x%x",
        result.status, logicalWidth, logicalHeight, density,
        result.activeWidth, result.activeHeight, result.activePixelWidth,
        result.activePixelHeight, result.activeDensity,
        CGDisplayPixelsWide(displayID), CGDisplayPixelsHigh(displayID), result.configurationStatus,
        result.targetIsUsable, result.targetIOFlags]);
    return NO;
}

static BOOL RMActivateDisplayMode(CGDirectDisplayID displayID, uint32_t width, uint32_t height,
                                  char *error, size_t errorLength) {
    if ((uint64_t)width * height > SR_MAXIMUM_BACKING_AREA) {
        RMSetError(error, errorLength, @"虚拟桌面 backing 面积超过上限");
        return NO;
    }
    uint32_t logicalWidth = (width + 1) / 2;
    uint32_t logicalHeight = (height + 1) / 2;
    if (RMConfigureHiDPIMode(displayID, width, height, logicalWidth, logicalHeight, 2.0f, error, errorLength)) {
        NSLog(@"RemoteMAC HiDPI 已核验并激活：采集尺寸=%ux%u，活动模式以 CoreGraphics 读回为准，density=2.00",
              width, height);
        return YES;
    }

    char halfScaleError[512] = {0};
    strlcpy(halfScaleError, error && error[0] ? error : "半尺寸逻辑@2x模式不可用",
            sizeof(halfScaleError));
    NSString *halfScaleMessage = RMStringFromUTF8(halfScaleError, @"半尺寸逻辑@2x模式不可用");
    if (width <= UINT32_MAX / 2 && height <= UINT32_MAX / 2
        && (uint64_t)width * height <= SR_MAXIMUM_BACKING_AREA / 4
        && RMConfigureHiDPIMode(displayID, width * 2, height * 2, width, height, 2.0f,
                                error, errorLength)) {
        NSLog(@"RemoteMAC HiDPI 已核验并激活：采集尺寸=%ux%u，活动模式以 CoreGraphics 读回为准，density=2.00；半尺寸模式未激活（%@）",
              width, height, halfScaleMessage);
        return YES;
    }

    char originalScaleError[512] = {0};
    strlcpy(originalScaleError, error && error[0] ? error : "原尺寸逻辑@2x模式超出虚拟显示器范围",
            sizeof(originalScaleError));
    NSString *originalScaleMessage = RMStringFromUTF8(originalScaleError, @"原尺寸逻辑@2x模式超出虚拟显示器范围");
    if (RMConfigureHiDPIMode(displayID, width, height, width, height, 1.0f,
                             error, errorLength)) {
        NSLog(@"RemoteMAC HiDPI 两档均未激活（半尺寸@2x：%@；原尺寸@2x：%@），已核验回退 1x：逻辑=%ux%u，backing=%ux%u，density=1.00",
              halfScaleMessage, originalScaleMessage, width, height, width, height);
        return YES;
    }
    char oneXError[512] = {0};
    strlcpy(oneXError, error && error[0] ? error : "原尺寸 1x 模式不可用", sizeof(oneXError));
    NSString *oneXMessage = RMStringFromUTF8(oneXError, @"原尺寸 1x 模式不可用");
    RMSetError(error, errorLength, [NSString stringWithFormat:
        @"三档显示模式均未激活；半尺寸逻辑@2x=%@；原尺寸逻辑@2x=%@；原尺寸1x=%@",
        halfScaleMessage, originalScaleMessage, oneXMessage]);
    return NO;
}

static BOOL RMRefreshMirrorAfterModeChange(CGDirectDisplayID target, CGDirectDisplayID source,
                                           char *error, size_t errorLength) {
    CGDirectDisplayID actualMaster = CGDisplayMirrorsDisplay(target);
    if (actualMaster != source && actualMaster != 0) {
        RMSetError(error, errorLength, [NSString stringWithFormat:
            @"切换显示模式期间实体镜像目标已改到其他源，保留当前布局：target=%u，expected=%u，actual=%u",
            target, source, actualMaster]);
        return NO;
    }
    if (actualMaster == source) {
        if (!RMConfigureMirror(target, 0, error, errorLength)) return NO;
        if (!RMWaitForMirror(target, 0, 50000000)) {
            RMSetError(error, errorLength, @"解除旧镜像关系以刷新显示模式失败");
            return NO;
        }
    }
    if (!RMConfigureMirror(target, source, error, errorLength)) return NO;
    if (!RMWaitForMirror(target, source, 300000000)) {
        RMSetError(error, errorLength, @"恢复实体镜像关系失败");
        return NO;
    }
    return YES;
}

static BOOL RMIsDisplayOnline(CGDirectDisplayID displayID) {
    uint32_t count = 0;
    CGError status = CGGetOnlineDisplayList(0, NULL, &count);
    if (status != kCGErrorSuccess) return YES;
    CGDirectDisplayID *online = calloc(count ?: 1, sizeof(CGDirectDisplayID));
    if (!online) return YES;
    status = CGGetOnlineDisplayList(count, online, &count);
    BOOL found = status != kCGErrorSuccess;
    if (status == kCGErrorSuccess) {
        for (uint32_t i = 0; i < count; i++) {
            if (online[i] == displayID) { found = YES; break; }
        }
    }
    free(online);
    return found;
}

static BOOL RMWaitForDisplayReady(CGDirectDisplayID displayID, char *error, size_t errorLength) {
    uint64_t deadline = RMDeadlineAfter(5.0);
    CGRect bounds = CGRectZero;
    size_t pixelWidth = 0;
    size_t pixelHeight = 0;
    while (mach_absolute_time() < deadline) {
        bounds = CGDisplayBounds(displayID);
        pixelWidth = CGDisplayPixelsWide(displayID);
        pixelHeight = CGDisplayPixelsHigh(displayID);
        if (RMIsDisplayOnline(displayID) && bounds.size.width > 0 && bounds.size.height > 0
            && pixelWidth > 0 && pixelHeight > 0) {
            NSLog(@"RemoteMAC 虚拟显示器已就绪：ID=%u，bounds=%.0fx%.0f，pixels=%zux%zu",
                  displayID, bounds.size.width, bounds.size.height, pixelWidth, pixelHeight);
            return YES;
        }
        CFRunLoopRunInMode(kCFRunLoopDefaultMode, 0.025, true);
    }
    RMSetError(error, errorLength, [NSString stringWithFormat:
        @"等待虚拟显示器就绪超时：ID=%u，bounds=%.0fx%.0f，pixels=%zux%zu，online=%d",
        displayID, bounds.size.width, bounds.size.height, pixelWidth, pixelHeight,
        RMIsDisplayOnline(displayID)]);
    return NO;
}

@interface RMNativeSession : NSObject <SCStreamOutput, SCStreamDelegate>
@property(nonatomic) uintptr_t callbackHandle;
@property(nonatomic) void *displayRef;
@property(nonatomic) CGDirectDisplayID displayID;
@property(nonatomic) CGDirectDisplayID targetDisplayID;
@property(nonatomic) CGDirectDisplayID previousMirrorID;
@property(nonatomic, strong) SCStream *stream;
@property(nonatomic, strong) dispatch_queue_t captureQueue;
@property(nonatomic) VTCompressionSessionRef compression;
@property(nonatomic) uint32_t width;
@property(nonatomic) uint32_t height;
@property(nonatomic) uint32_t screenWidth;
@property(nonatomic) uint32_t screenHeight;
@property(nonatomic) uint32_t activeLogicalWidth;
@property(nonatomic) uint32_t activeLogicalHeight;
@property(nonatomic) uint32_t fps;
@property(nonatomic) uint32_t bitrate;
@property(nonatomic) uint32_t pendingWidth;
@property(nonatomic) uint32_t pendingHeight;
@property(nonatomic) uint32_t pendingFPS;
@property(nonatomic) uint32_t pendingBitrate;
@property(nonatomic) int lockFD;
@property(nonatomic) BOOL cursorScaleChanged;
@property(nonatomic) BOOL forceKeyframe;
@property(nonatomic) BOOL stopped;
@property(nonatomic) BOOL cleanupStarted;
@property(nonatomic) BOOL cleanupFinished;
@property(nonatomic) BOOL displayModeRecoveryFailed;
@property(nonatomic) BOOL paused;
@property(nonatomic) BOOL openingStream;
@property(nonatomic) BOOL encodingFrame;
@property(nonatomic) uint64_t configGeneration;
@property(nonatomic, strong) NSMutableSet<NSNumber *> *pressedKeys;
@property(nonatomic, strong) NSMutableSet<NSNumber *> *pressedButtons;
@property(nonatomic, strong) NSMutableArray<NSNumber *> *videoConfigRequests;
@property(nonatomic, strong) NSMutableArray *stopCompletions;
- (void)applyPendingVideoConfig;
- (BOOL)completeVideoConfigRequestsWithError:(NSString *)error;
- (BOOL)start:(char *)error errorLength:(size_t)errorLength;
- (void)refreshActiveDisplaySize;
- (BOOL)resizeDisplay:(uint32_t)width height:(uint32_t)height error:(char *)error errorLength:(size_t)errorLength;
- (BOOL)setVideoWidth:(uint32_t)width height:(uint32_t)height fps:(uint32_t)fps bitrate:(uint32_t)bitrate requestID:(uintptr_t)requestID error:(char *)error errorLength:(size_t)errorLength;
- (void)sendKeyframe;
- (void)pointer:(int)action button:(int)button x:(double)x y:(double)y width:(uint32_t)width height:(uint32_t)height;
- (void)key:(uint16_t)keycode down:(BOOL)down;
- (void)text:(NSString *)text;
- (void)releasePressedInputs;
- (void)stopWithCompletion:(dispatch_block_t)completion;
- (void)finishStopCleanup;
- (void)completeStopCleanup;
@end

static RMNativeSession *RMCurrentSession;

static void RMFinishEncodingFrame(RMNativeSession *session) {
    @synchronized (session) {
        session.encodingFrame = NO;
    }
}

static void RMCompressionOutput(void *outputRefCon, void *sourceFrameRefCon, OSStatus status,
                                VTEncodeInfoFlags infoFlags, CMSampleBufferRef sampleBuffer) {
    (void)sourceFrameRefCon;
    RMNativeSession *session = (__bridge RMNativeSession *)outputRefCon;
    if (!session) return;
    uint32_t width = 0, height = 0, fps = 0;
    @synchronized (session) {
        if (session.stopped || session.paused) {
            session.encodingFrame = NO;
            return;
        }
        width = session.width;
        height = session.height;
        fps = session.fps;
    }
    if (status == noErr && (infoFlags & kVTEncodeInfo_FrameDropped) != 0) {
        RMFinishEncodingFrame(session);
        return;
    }
    if (status != noErr || !sampleBuffer) {
        RMFinishEncodingFrame(session);
        rmGoError(session.callbackHandle, "VideoToolbox H.264 编码回调失败");
        return;
    }
    CMBlockBufferRef block = CMSampleBufferGetDataBuffer(sampleBuffer);
    if (!block) {
        RMFinishEncodingFrame(session);
        rmGoError(session.callbackHandle, "VideoToolbox 返回空 H.264 数据块");
        return;
    }
    size_t length = CMBlockBufferGetDataLength(block);
    if (length == 0 || length > 32 * 1024 * 1024) {
        RMFinishEncodingFrame(session);
        rmGoError(session.callbackHandle, "VideoToolbox 输出帧大小无效");
        return;
    }
    uint8_t *avcc = malloc(length);
    if (!avcc) {
        RMFinishEncodingFrame(session);
        rmGoError(session.callbackHandle, "分配 H.264 输出缓冲失败");
        return;
    }
    if (CMBlockBufferCopyDataBytes(block, 0, length, avcc) != kCMBlockBufferNoErr) {
        free(avcc);
        RMFinishEncodingFrame(session);
        rmGoError(session.callbackHandle, "复制 VideoToolbox H.264 输出失败");
        return;
    }

    CMFormatDescriptionRef format = CMSampleBufferGetFormatDescription(sampleBuffer);
    const uint8_t *firstParameterSet = NULL;
    size_t firstParameterSetSize = 0;
    size_t parameterSetCount = 0;
    int naluLengthSize = 4;
    OSStatus formatStatus = format ? CMVideoFormatDescriptionGetH264ParameterSetAtIndex(
        format, 0, &firstParameterSet, &firstParameterSetSize, &parameterSetCount, &naluLengthSize) : -1;
    if (formatStatus != noErr || !firstParameterSet || firstParameterSetSize == 0
        || naluLengthSize < 1 || naluLengthSize > 4) {
        free(avcc);
        RMFinishEncodingFrame(session);
        rmGoError(session.callbackHandle, "VideoToolbox 未提供有效的 H.264 格式描述");
        return;
    }

    NSMutableData *naluData = [NSMutableData dataWithCapacity:length + 256];
    const uint8_t startCode[] = {0, 0, 0, 1};
    size_t offset = 0;
    size_t naluCount = 0;
    BOOL keyframe = NO;
    BOOL malformed = NO;
    while (offset + (size_t)naluLengthSize <= length) {
        uint32_t naluLength = 0;
        for (int index = 0; index < naluLengthSize; index++) naluLength = (naluLength << 8) | avcc[offset + index];
        offset += naluLengthSize;
        if (naluLength == 0 || naluLength > length - offset) {
            malformed = YES;
            break;
        }
        if ((avcc[offset] & 0x1f) == 5) keyframe = YES;
        [naluData appendBytes:startCode length:sizeof(startCode)];
        [naluData appendBytes:avcc + offset length:naluLength];
        offset += naluLength;
        naluCount++;
    }
    free(avcc);
    if (malformed || offset != length || naluCount == 0) {
        RMFinishEncodingFrame(session);
        rmGoError(session.callbackHandle, "VideoToolbox 输出包含无效的 H.264 NAL 单元");
        return;
    }
    NSMutableData *annexB = [NSMutableData dataWithCapacity:naluData.length + 256];
    if (keyframe) {
        for (size_t index = 0; index < 2; index++) {
            const uint8_t *parameterSet = NULL;
            size_t parameterSetSize = 0;
            size_t parameterCount = 0;
            int headerLength = 4;
            OSStatus parameterStatus = CMVideoFormatDescriptionGetH264ParameterSetAtIndex(
                format, index, &parameterSet, &parameterSetSize, &parameterCount, &headerLength);
            if (parameterStatus != noErr || !parameterSet || parameterSetSize == 0 || headerLength != naluLengthSize) {
                RMFinishEncodingFrame(session);
                rmGoError(session.callbackHandle, "IDR 帧缺少有效的 H.264 SPS/PPS 参数集");
                return;
            }
            [annexB appendBytes:startCode length:sizeof(startCode)];
            [annexB appendBytes:parameterSet length:parameterSetSize];
        }
    }
    [annexB appendData:naluData];
    if (annexB.length == 0 || annexB.length > 32 * 1024 * 1024) {
        RMFinishEncodingFrame(session);
        rmGoError(session.callbackHandle, "VideoToolbox 转换后的 Annex B 帧大小无效");
        return;
    }
    CMTime capturePTS = CMSampleBufferGetPresentationTimeStamp(sampleBuffer);
    int64_t presentationNS = CMTIME_IS_NUMERIC(capturePTS) ? CMTimeConvertScale(capturePTS, 1000000000, kCMTimeRoundingMethod_Default).value : -1;
    rmGoFrame(session.callbackHandle, (uint8_t *)annexB.bytes, annexB.length, width, height,
              fps, keyframe ? 1 : 0, presentationNS);
    RMFinishEncodingFrame(session);
}

@implementation RMNativeSession

- (BOOL)acquireDisplayLock:(char *)error errorLength:(size_t)errorLength {
    NSString *support = NSSearchPathForDirectoriesInDomains(NSApplicationSupportDirectory, NSUserDomainMask, YES).firstObject;
    NSString *directory = [support stringByAppendingPathComponent:@"RemoteMAC"];
    NSDictionary *attributes = @{NSFilePosixPermissions: @0700};
    if (![[NSFileManager defaultManager] createDirectoryAtPath:directory withIntermediateDirectories:YES attributes:attributes error:nil]) {
        RMSetError(error, errorLength, @"创建 RemoteMAC 显示器锁目录失败");
        return NO;
    }
    NSString *path = [directory stringByAppendingPathComponent:@"virtual-display.lock"];
    int fd = open(path.fileSystemRepresentation, O_CREAT | O_RDWR | O_CLOEXEC | O_NOFOLLOW, 0600);
    if (fd < 0 || flock(fd, LOCK_EX | LOCK_NB) != 0) {
        if (fd >= 0) close(fd);
        RMSetError(error, errorLength, @"RemoteMAC 虚拟屏幕正被另一个进程占用");
        return NO;
    }
    self.lockFD = fd;
    return YES;
}

- (BOOL)start:(char *)error errorLength:(size_t)errorLength {
    if (!CGPreflightScreenCaptureAccess()) {
        (void)CGRequestScreenCaptureAccess();
        RMSetError(error, errorLength, @"尚未授予屏幕录制权限；请在系统设置中允许 RemoteMAC 或启动它的终端，然后重新连接");
        return NO;
    }
    if (!CGPreflightPostEventAccess()) {
        (void)CGRequestPostEventAccess();
        RMSetError(error, errorLength, @"尚未授予辅助功能输入权限；请在系统设置中允许 RemoteMAC 或启动它的终端，然后重新连接");
        return NO;
    }
    if (![self acquireDisplayLock:error errorLength:errorLength]) return NO;
    uint32_t count = 0;
    CGGetOnlineDisplayList(0, NULL, &count);
    CGDirectDisplayID *online = calloc(count ?: 1, sizeof(CGDirectDisplayID));
    CGGetOnlineDisplayList(count, online, &count);
    for (uint32_t index = 0; index < count; index++) {
        CGDirectDisplayID id = online[index];
        if (CGDisplayVendorNumber(id) == RMVendorID && CGDisplayModelNumber(id) == RMProductID && CGDisplaySerialNumber(id) == RMSerialNumber) {
            free(online);
            RMSetError(error, errorLength, @"RemoteMAC 固定身份的虚拟屏幕仍在线，等待前一会话清理后重试");
            return NO;
        }
    }
    free(online);
    self.targetDisplayID = CGMainDisplayID();
    if (!self.targetDisplayID || CGDisplayIsOnline(self.targetDisplayID) == 0) {
        RMSetError(error, errorLength, @"当前图形会话没有可用主显示器");
        return NO;
    }
    self.previousMirrorID = CGDisplayMirrorsDisplay(self.targetDisplayID);
    const char *failureStage = NULL;
    self.displayRef = SRCreateDisplay(self.screenWidth, self.screenHeight, 60.0, &_displayID, &failureStage);
    if (!self.displayRef) {
        RMSetError(error, errorLength, [NSString stringWithFormat:@"创建 RemoteMAC 虚拟屏幕失败：%@",
                                        RMStringFromUTF8(failureStage, @"unknown")]);
        return NO;
    }
    if (!RMWaitForDisplayReady(self.displayID, error, errorLength)) return NO;
    if (!RMConfigureMirrorAndWait(self.targetDisplayID, self.displayID, self.previousMirrorID,
                                  500000000, error, errorLength)) {
        if (!error || errorLength == 0 || error[0] == '\0') RMSetError(error, errorLength, @"WindowServer 尚未确认虚拟屏幕镜像关系");
        return NO;
    }
    if (!RMActivateDisplayMode(self.displayID, self.screenWidth, self.screenHeight, error, errorLength)
        || !RMRefreshMirrorAfterModeChange(self.targetDisplayID, self.displayID, error, errorLength)) {
        return NO;
    }
    if (!RMActivateDisplayMode(self.displayID, self.screenWidth, self.screenHeight, error, errorLength)) return NO;
    if (CGDisplayMirrorsDisplay(self.targetDisplayID) != self.displayID) {
        RMSetError(error, errorLength, @"WindowServer 尚未确认虚拟屏幕镜像关系");
        return NO;
    }
    [self refreshActiveDisplaySize];
    [self requestShareableDisplay];
    return YES;
}

- (void)refreshActiveDisplaySize {
    self.activeLogicalWidth = self.screenWidth;
    self.activeLogicalHeight = self.screenHeight;
    CGDisplayModeRef mode = CGDisplayCopyDisplayMode(self.displayID);
    if (!mode) return;
    self.activeLogicalWidth = (uint32_t)CGDisplayModeGetWidth(mode);
    self.activeLogicalHeight = (uint32_t)CGDisplayModeGetHeight(mode);
    CFRelease(mode);
}

- (void)requestShareableDisplay {
    if (self.stopped || self.paused || self.openingStream) return;
    self.openingStream = YES;
    __weak RMNativeSession *weakSelf = self;
    [SCShareableContent getShareableContentExcludingDesktopWindows:NO onScreenWindowsOnly:NO completionHandler:^(SCShareableContent *content, NSError *error) {
        RMNativeSession *strongSelf = weakSelf;
        if (!strongSelf || strongSelf.stopped) return;
        if (error) {
            dispatch_async(dispatch_get_main_queue(), ^{
                strongSelf.openingStream = NO;
                if (!strongSelf.stopped) [strongSelf failVideoConfigRequestsWithError:error.localizedDescription ?: @"查询可采集显示器失败"];
            });
            return;
        }
        SCDisplay *display = nil;
        for (SCDisplay *candidate in content.displays) if (candidate.displayID == strongSelf.displayID) { display = candidate; break; }
        if (!display) {
            dispatch_async(dispatch_get_main_queue(), ^{
                strongSelf.openingStream = NO;
                if (!strongSelf.stopped) [strongSelf failVideoConfigRequestsWithError:@"ScreenCaptureKit 未枚举 RemoteMAC 虚拟显示器"];
            });
            return;
        }
        dispatch_async(dispatch_get_main_queue(), ^{
            if (strongSelf.stopped || strongSelf.paused) {
                strongSelf.openingStream = NO;
                if (!strongSelf.stopped && strongSelf.paused && !strongSelf.stream) {
                    [strongSelf completeVideoConfigRequestsWithError:nil];
                }
                return;
            }
            [strongSelf openStreamForDisplay:display];
        });
    }];
}

- (void)applyPendingVideoConfig {
    if (!self.pendingBitrate || !self.pendingFPS) return;
    @synchronized (self) {
        self.width = self.pendingWidth;
        self.height = self.pendingHeight;
        self.fps = self.pendingFPS;
        self.bitrate = self.pendingBitrate;
    }
    self.pendingWidth = 0;
    self.pendingHeight = 0;
    self.pendingFPS = 0;
    self.pendingBitrate = 0;
}

- (BOOL)completeVideoConfigRequestsWithError:(NSString *)error {
    NSArray<NSNumber *> *requests = self.videoConfigRequests.copy;
    [self.videoConfigRequests removeAllObjects];
    const char *message = error.UTF8String;
    for (NSNumber *requestID in requests) {
        rmGoVideoConfigComplete((uintptr_t)requestID.unsignedLongLongValue, (char *)message);
    }
    return requests.count > 0;
}

- (void)failVideoConfigRequestsWithError:(NSString *)error {
    if (![self completeVideoConfigRequestsWithError:error]) {
        rmGoError(self.callbackHandle, (char *)(error.UTF8String ?: "macOS 视频采集配置失败"));
    }
}

- (BOOL)createCompression:(char *)error errorLength:(size_t)errorLength {
    if (self.compression) {
        VTCompressionSessionInvalidate(self.compression);
        CFRelease(self.compression);
        self.compression = NULL;
    }
    NSDictionary *specification = @{
        (__bridge NSString *)kVTVideoEncoderSpecification_RequireHardwareAcceleratedVideoEncoder: @YES,
        (__bridge NSString *)kVTVideoEncoderSpecification_EnableLowLatencyRateControl: @YES,
    };
    OSStatus status = VTCompressionSessionCreate(kCFAllocatorDefault, self.width, self.height,
        kCMVideoCodecType_H264, (__bridge CFDictionaryRef)specification,
        (__bridge CFDictionaryRef)@{(__bridge NSString *)kCVPixelBufferPixelFormatTypeKey: @(kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange)}, NULL,
        RMCompressionOutput, (__bridge void *)self, &_compression);
    if (status != noErr || !self.compression) {
        RMSetError(error, errorLength, [NSString stringWithFormat:@"创建硬件 H.264 编码器失败：%d", (int)status]);
        return NO;
    }
    if (VTSessionSetProperty(self.compression, kVTCompressionPropertyKey_RealTime, kCFBooleanTrue) != noErr
        || VTSessionSetProperty(self.compression, kVTCompressionPropertyKey_AllowFrameReordering, kCFBooleanFalse) != noErr
        || VTSessionSetProperty(self.compression, kVTCompressionPropertyKey_ProfileLevel, kVTProfileLevel_H264_ConstrainedBaseline_AutoLevel) != noErr) {
        RMSetError(error, errorLength, @"VideoToolbox 无法应用实时、无帧重排的 H.264 参数");
        return NO;
    }
    int32_t expectedFPS = (int32_t)self.fps;
    CFNumberRef fps = CFNumberCreate(kCFAllocatorDefault, kCFNumberSInt32Type, &expectedFPS);
    status = VTSessionSetProperty(self.compression, kVTCompressionPropertyKey_ExpectedFrameRate, fps);
    CFRelease(fps);
    if (status != noErr) {
        RMSetError(error, errorLength, [NSString stringWithFormat:@"设置 H.264 目标帧率失败：%d", (int)status]);
        return NO;
    }
    CFNumberRef maximumRealtimeFPS = CFNumberCreate(kCFAllocatorDefault, kCFNumberSInt32Type, &expectedFPS);
    status = VTSessionSetProperty(self.compression, kVTCompressionPropertyKey_MaximumRealTimeFrameRate, maximumRealtimeFPS);
    CFRelease(maximumRealtimeFPS);
    if (status != noErr) {
        // This property is an encoder setup hint. Keep the realtime session
        // usable on hardware that does not expose it; ScreenCaptureKit still
        // enforces the configured submission interval.
        NSLog(@"VideoToolbox 最大实时帧率提示不可用（%d），继续使用采集帧率上限", (int)status);
    }
    int32_t keyInterval = (int32_t)self.fps;
    CFNumberRef keyFrames = CFNumberCreate(kCFAllocatorDefault, kCFNumberSInt32Type, &keyInterval);
    status = VTSessionSetProperty(self.compression, kVTCompressionPropertyKey_MaxKeyFrameInterval, keyFrames);
    CFRelease(keyFrames);
    if (status != noErr) {
        RMSetError(error, errorLength, [NSString stringWithFormat:@"设置 H.264 关键帧间隔失败：%d", (int)status]);
        return NO;
    }
    status = VTCompressionSessionPrepareToEncodeFrames(self.compression);
    if (status != noErr) {
        RMSetError(error, errorLength, [NSString stringWithFormat:@"准备 VideoToolbox 编码器失败：%d", (int)status]);
        return NO;
    }
    CFTypeRef hardwareValue = NULL;
    status = VTSessionCopyProperty(self.compression, kVTCompressionPropertyKey_UsingHardwareAcceleratedVideoEncoder,
                                   kCFAllocatorDefault, &hardwareValue);
    BOOL hardwareEnabled = YES;
    if (status == noErr) {
        hardwareEnabled = hardwareValue
            && CFGetTypeID(hardwareValue) == CFBooleanGetTypeID()
            && CFBooleanGetValue((CFBooleanRef)hardwareValue);
    } else {
        // The session was created with RequireHardwareAcceleratedVideoEncoder.
        // VideoToolbox fails session creation when hardware encoding is unavailable,
        // so an unsupported readback property must not reject a valid hardware session.
        NSLog(@"VideoToolbox 硬件编码属性不可查询（%d）；由强制硬件会话创建成功确认", (int)status);
    }
    if (hardwareValue) CFRelease(hardwareValue);
    if (!hardwareEnabled) {
        RMSetError(error, errorLength, @"VideoToolbox 未确认使用硬件 H.264 编码器");
        return NO;
    }
    @synchronized (self) { self.forceKeyframe = YES; }
    return [self setCompressionBitrate:self.bitrate error:error errorLength:errorLength];
}

- (BOOL)setCompressionBitrate:(uint32_t)bitrate error:(char *)error errorLength:(size_t)errorLength {
    if (!self.compression || bitrate == 0) return YES;
    int32_t target = (int32_t)bitrate;
    CFNumberRef number = CFNumberCreate(kCFAllocatorDefault, kCFNumberSInt32Type, &target);
    OSStatus status = VTSessionSetProperty(self.compression, kVTCompressionPropertyKey_AverageBitRate, number);
    CFRelease(number);
    if (status != noErr) {
        RMSetError(error, errorLength, [NSString stringWithFormat:@"更新 H.264 目标码率失败：%d", (int)status]);
        return NO;
    }
    // Keep a small keyframe burst allowance above the long-term average.
    // VideoToolbox is more likely to undershoot AverageBitRate when this hard
    // window cap is set to the same value; match the pacer's 25% headroom.
    NSArray *limits = @[@((uint64_t)bitrate * 5 / 4 / 8), @1];
    status = VTSessionSetProperty(self.compression, kVTCompressionPropertyKey_DataRateLimits, (__bridge CFArrayRef)limits);
    if (status != noErr) {
        RMSetError(error, errorLength, [NSString stringWithFormat:@"更新 VideoToolbox 数据速率限制失败：%d", (int)status]);
        return NO;
    }
    return YES;
}

- (void)openStreamForDisplay:(SCDisplay *)display {
    if (self.stopped || self.paused) {
        self.openingStream = NO;
        return;
    }
    [self applyPendingVideoConfig];
    uint64_t generation = self.configGeneration;
    char errorText[512] = {0};
    if (![self createCompression:errorText errorLength:sizeof(errorText)]) {
        self.openingStream = NO;
        [self failVideoConfigRequestsWithError:[NSString stringWithUTF8String:errorText]];
        return;
    }
    SCContentFilter *filter = [[SCContentFilter alloc] initWithDisplay:display excludingWindows:@[]];
    SCStreamConfiguration *config = [[SCStreamConfiguration alloc] init];
    config.width = self.width;
    config.height = self.height;
    // Capture in the encoder's native bi-planar YUV format instead of BGRA.
    config.pixelFormat = kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange;
    config.showsCursor = YES;
    config.queueDepth = 3;
    config.minimumFrameInterval = CMTimeMake(1, (int32_t)self.fps);
    config.capturesAudio = NO;
    self.stream = [[SCStream alloc] initWithFilter:filter configuration:config delegate:self];
    NSError *addError = nil;
    if (![self.stream addStreamOutput:self type:SCStreamOutputTypeScreen sampleHandlerQueue:self.captureQueue error:&addError]) {
        self.stream = nil;
        self.openingStream = NO;
        [self failVideoConfigRequestsWithError:addError.localizedDescription ?: @"注册屏幕采集回调失败"];
        return;
    }
    __weak RMNativeSession *weakSelf = self;
    SCStream *opening = self.stream;
    [opening startCaptureWithCompletionHandler:^(NSError *error) {
        RMNativeSession *strongSelf = weakSelf;
        if (!strongSelf) return;
        dispatch_async(dispatch_get_main_queue(), ^{
            if (strongSelf.stream != opening || strongSelf.stopped) return;
            if (error && generation == strongSelf.configGeneration) {
                strongSelf.stream = nil;
                strongSelf.openingStream = NO;
                [strongSelf failVideoConfigRequestsWithError:error.localizedDescription ?: @"启动屏幕采集失败"];
                return;
            }
            if (generation == strongSelf.configGeneration && !strongSelf.paused) {
                strongSelf.openingStream = NO;
                [strongSelf completeVideoConfigRequestsWithError:nil];
                return;
            }

            [opening stopCaptureWithCompletionHandler:^(NSError *stopError) {
                dispatch_async(dispatch_get_main_queue(), ^{
                    if (strongSelf.stream == opening) strongSelf.stream = nil;
                    if (stopError) {
                        strongSelf.openingStream = NO;
                        [strongSelf failVideoConfigRequestsWithError:stopError.localizedDescription ?: @"停止旧屏幕采集失败"];
                        return;
                    }
                    if (strongSelf.compression) VTCompressionSessionCompleteFrames(strongSelf.compression, kCMTimeInvalid);
                    [strongSelf applyPendingVideoConfig];
                    strongSelf.openingStream = NO;
                    if (!strongSelf.stopped && !strongSelf.paused) [strongSelf requestShareableDisplay];
                    else if (!strongSelf.stopped && strongSelf.paused) [strongSelf completeVideoConfigRequestsWithError:nil];
                });
            }];
        });
    }];
}

- (void)stream:(SCStream *)stream didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer ofType:(SCStreamOutputType)type {
    VTCompressionSessionRef compression = NULL;
    uint32_t fps = 0;
    BOOL busyDrop = NO;
    CVImageBufferRef imageBuffer = CMSampleBufferGetImageBuffer(sampleBuffer);
    if (type != SCStreamOutputTypeScreen || !imageBuffer) return;
    if (@available(macOS 12.3, *)) {
        CFArrayRef attachments = CMSampleBufferGetSampleAttachmentsArray(sampleBuffer, false);
        if (attachments && CFArrayGetCount(attachments) > 0) {
            CFDictionaryRef frameInfo = CFArrayGetValueAtIndex(attachments, 0);
            CFNumberRef frameStatus = CFDictionaryGetValue(frameInfo, (__bridge const void *)SCStreamFrameInfoStatus);
            NSInteger status = SCFrameStatusComplete;
            if (frameStatus && CFNumberGetValue(frameStatus, kCFNumberNSIntegerType, &status)
                && status != SCFrameStatusComplete && status != SCFrameStatusStarted) return;
        }
    }
    NSDictionary *properties = nil;
    @synchronized (self) {
        if (stream != self.stream || self.stopped || self.paused || !self.compression) return;
        if (self.encodingFrame) {
            busyDrop = YES;
        } else {
            compression = self.compression;
            fps = self.fps;
            self.encodingFrame = YES;
            if (self.forceKeyframe) {
                properties = @{(__bridge NSString *)kVTEncodeFrameOptionKey_ForceKeyFrame: @YES};
                self.forceKeyframe = NO;
            }
        }
    }
    if (busyDrop) return;
    VTEncodeInfoFlags flags = 0;
    CMTime pts = CMSampleBufferGetPresentationTimeStamp(sampleBuffer);
    CMTime duration = CMTimeMake(1, (int32_t)fps);
    OSStatus status = VTCompressionSessionEncodeFrame(compression, imageBuffer, pts, duration,
        (__bridge CFDictionaryRef)properties, NULL, &flags);
    if (status != noErr) {
        RMFinishEncodingFrame(self);
        if (status != kVTInvalidSessionErr) rmGoError(self.callbackHandle, "VideoToolbox 提交帧失败");
    }
}

- (void)stream:(SCStream *)stream didStopWithError:(NSError *)error {
    (void)stream;
    if (!self.stopped && error) rmGoError(self.callbackHandle, (char *)error.localizedDescription.UTF8String);
}

- (BOOL)resizeDisplay:(uint32_t)width height:(uint32_t)height error:(char *)error errorLength:(size_t)errorLength {
    if (!self.displayRef || width < 320 || height < 200
        || (uint64_t)width * height > SR_MAXIMUM_BACKING_AREA) {
        RMSetError(error, errorLength, @"虚拟屏幕尺寸超出范围");
        return NO;
    }
    if (width == self.screenWidth && height == self.screenHeight) return YES;
    uint32_t oldWidth = self.screenWidth;
    uint32_t oldHeight = self.screenHeight;
    const char *failureStage = NULL;
    if (!SRUpdateDisplayMode(self.displayRef, width, height, 60.0, &failureStage)) {
        RMSetError(error, errorLength, [NSString stringWithFormat:@"更新虚拟屏幕模式失败：%@",
                                        RMStringFromUTF8(failureStage, @"unknown")]);
        return NO;
    }
    self.screenWidth = width;
    self.screenHeight = height;
    BOOL applied = RMActivateDisplayMode(self.displayID, width, height, error, errorLength)
        && RMRefreshMirrorAfterModeChange(self.targetDisplayID, self.displayID, error, errorLength)
        && RMActivateDisplayMode(self.displayID, width, height, error, errorLength);
    if (applied) {
        [self refreshActiveDisplaySize];
        float cursorScale = RMCursorScaleForResolution(self.activeLogicalWidth, self.activeLogicalHeight);
        if (rm_set_cursor_scale(cursorScale) == 0) {
            self.cursorScaleChanged = cursorScale > 1.0f;
            NSLog(@"RemoteMAC 鼠标缩放已更新：最长边=%u，系数=%.2f",
                  self.activeLogicalWidth > self.activeLogicalHeight ? self.activeLogicalWidth : self.activeLogicalHeight,
                  cursorScale);
        } else {
            NSLog(@"RemoteMAC 无法更新鼠标缩放系数=%.2f，保留当前大小", cursorScale);
        }
        return YES;
    }

    char originalError[512] = {0};
    strlcpy(originalError, error && error[0] ? error : "显示模式激活失败", sizeof(originalError));
    NSString *originalMessage = RMStringFromUTF8(originalError, @"显示模式激活失败");
    const char *rollbackStage = NULL;
    if (!SRUpdateDisplayMode(self.displayRef, oldWidth, oldHeight, 60.0, &rollbackStage)) {
        self.displayModeRecoveryFailed = YES;
        RMSetError(error, errorLength, [NSString stringWithFormat:
            @"%@；恢复旧虚拟屏幕模式失败：%@", originalMessage,
            RMStringFromUTF8(rollbackStage, @"unknown")]);
        return NO;
    }
    self.screenWidth = oldWidth;
    self.screenHeight = oldHeight;
    char rollbackError[512] = {0};
    if (!RMActivateDisplayMode(self.displayID, oldWidth, oldHeight, rollbackError, sizeof(rollbackError))
        || !RMRefreshMirrorAfterModeChange(self.targetDisplayID, self.displayID, rollbackError, sizeof(rollbackError))
        || !RMActivateDisplayMode(self.displayID, oldWidth, oldHeight, rollbackError, sizeof(rollbackError))) {
        self.displayModeRecoveryFailed = YES;
        NSString *rollbackMessage = RMStringFromUTF8(rollbackError, @"unknown");
        RMSetError(error, errorLength, [NSString stringWithFormat:
            @"%@；恢复旧显示模式或镜像关系失败：%@", originalMessage, rollbackMessage]);
        return NO;
    }
    RMSetError(error, errorLength, originalMessage);
    return NO;
}

- (BOOL)setVideoWidth:(uint32_t)width height:(uint32_t)height fps:(uint32_t)fps bitrate:(uint32_t)bitrate requestID:(uintptr_t)requestID error:(char *)error errorLength:(size_t)errorLength {
    BOOL pausing = bitrate == 0 || fps == 0;
    if (!pausing && (width < 320 || height < 200
        || (uint64_t)width * height > SR_MAXIMUM_BACKING_AREA || fps > 60 || bitrate > 50000000)) {
        RMSetError(error, errorLength, @"视频编码参数超出范围");
        return NO;
    }
    if (!self.videoConfigRequests) self.videoConfigRequests = [NSMutableArray array];
    [self.videoConfigRequests addObject:@((unsigned long long)requestID)];

    if (pausing) {
        ++self.configGeneration;
        @synchronized (self) { self.paused = YES; }
        if (self.openingStream) return YES;
        SCStream *oldStream = self.stream;
        if (!oldStream) {
            [self completeVideoConfigRequestsWithError:nil];
            return YES;
        }
        self.openingStream = YES;
        __weak RMNativeSession *weakSelf = self;
        [oldStream stopCaptureWithCompletionHandler:^(NSError *stopError) {
            dispatch_async(dispatch_get_main_queue(), ^{
                RMNativeSession *strongSelf = weakSelf;
                if (!strongSelf || strongSelf.stopped) return;
                if (stopError) {
                    strongSelf.openingStream = NO;
                    [strongSelf failVideoConfigRequestsWithError:stopError.localizedDescription ?: @"暂停时停止屏幕采集失败"];
                    return;
                }
                if (strongSelf.compression) VTCompressionSessionCompleteFrames(strongSelf.compression, kCMTimeInvalid);
                if (strongSelf.stream == oldStream) strongSelf.stream = nil;
                [strongSelf applyPendingVideoConfig];
                strongSelf.openingStream = NO;
                if (!strongSelf.paused) [strongSelf requestShareableDisplay];
                else [strongSelf completeVideoConfigRequestsWithError:nil];
            });
        }];
        return YES;
    }

    if (self.openingStream) {
        ++self.configGeneration;
        self.pendingWidth = width;
        self.pendingHeight = height;
        self.pendingFPS = fps;
        self.pendingBitrate = bitrate;
        @synchronized (self) { self.paused = NO; }
        return YES;
    }

    BOOL recreate = NO;
    @synchronized (self) {
        recreate = width != self.width || height != self.height || fps != self.fps || self.paused;
    }
    if (!recreate && self.stream && self.compression) {
        if (![self setCompressionBitrate:bitrate error:error errorLength:errorLength]) {
            [self.videoConfigRequests removeObject:@((unsigned long long)requestID)];
            return NO;
        }
        @synchronized (self) { self.bitrate = bitrate; }
        [self completeVideoConfigRequestsWithError:nil];
        return YES;
    }

    ++self.configGeneration;
    SCStream *oldStream = self.stream;
    self.pendingWidth = width;
    self.pendingHeight = height;
    self.pendingFPS = fps;
    self.pendingBitrate = bitrate;
    @synchronized (self) { self.paused = NO; }
    if (!oldStream) {
        [self requestShareableDisplay];
        return YES;
    }

    self.openingStream = YES;
    __weak RMNativeSession *weakSelf = self;
    [oldStream stopCaptureWithCompletionHandler:^(NSError *stopError) {
        dispatch_async(dispatch_get_main_queue(), ^{
            RMNativeSession *strongSelf = weakSelf;
            if (!strongSelf || strongSelf.stopped) return;
            if (stopError) {
                strongSelf.openingStream = NO;
                [strongSelf failVideoConfigRequestsWithError:stopError.localizedDescription ?: @"重配时停止旧屏幕采集失败"];
                return;
            }
            if (strongSelf.compression) VTCompressionSessionCompleteFrames(strongSelf.compression, kCMTimeInvalid);
            if (strongSelf.stream == oldStream) strongSelf.stream = nil;
            [strongSelf applyPendingVideoConfig];
            strongSelf.openingStream = NO;
            [strongSelf requestShareableDisplay];
        });
    }];
    return YES;
}

- (void)sendKeyframe {
    @synchronized (self) { self.forceKeyframe = YES; }
}

- (void)pointer:(int)action button:(int)button x:(double)x y:(double)y width:(uint32_t)width height:(uint32_t)height {
    if (!width || !height || self.stopped) return;
    CGRect bounds = CGDisplayBounds(self.targetDisplayID);
    if (CGRectIsEmpty(bounds)) return;
    CGFloat contentWidth = bounds.size.width;
    CGFloat contentHeight = bounds.size.height;
    CGFloat offsetX = 0;
    CGFloat offsetY = 0;
    CGFloat sourceAspect = (CGFloat)width / (CGFloat)height;
    CGFloat displayAspect = bounds.size.width / bounds.size.height;
    if (displayAspect > sourceAspect) {
        contentWidth = bounds.size.height * sourceAspect;
        offsetX = (bounds.size.width - contentWidth) / 2;
    } else {
        contentHeight = bounds.size.width / sourceAspect;
        offsetY = (bounds.size.height - contentHeight) / 2;
    }
    CGFloat maxContentX = MAX(contentWidth - 1, 0);
    CGFloat maxContentY = MAX(contentHeight - 1, 0);
    CGFloat normalizedX = MIN(MAX((CGFloat)x, 0), (CGFloat)width - 1);
    CGFloat normalizedY = MIN(MAX((CGFloat)y, 0), (CGFloat)height - 1);
    CGPoint point = CGPointMake(
        bounds.origin.x + offsetX + normalizedX * maxContentX / MAX((CGFloat)width - 1, 1),
        bounds.origin.y + offsetY + normalizedY * maxContentY / MAX((CGFloat)height - 1, 1)
    );
    CGMouseButton mouseButton = button == 2 ? kCGMouseButtonRight : (button == 1 ? kCGMouseButtonCenter : kCGMouseButtonLeft);
    CGEventType eventType;
    if (action == 3) {
        // Pointer wheel values from touch gestures are pixel deltas. Keep them
        // as pixel scroll events so small trackpad movements are not rounded
        // down to zero whole lines.
        CGEventRef scroll = CGEventCreateScrollWheelEvent(NULL, kCGScrollEventUnitPixel, 2,
            (int32_t)round(-y), (int32_t)round(-x));
        if (scroll) { CGEventPost(kCGHIDEventTap, scroll); CFRelease(scroll); }
        return;
    }
    if (action == 1) eventType = mouseButton == kCGMouseButtonRight ? kCGEventRightMouseDown : (mouseButton == kCGMouseButtonCenter ? kCGEventOtherMouseDown : kCGEventLeftMouseDown);
    else if (action == 2) eventType = mouseButton == kCGMouseButtonRight ? kCGEventRightMouseUp : (mouseButton == kCGMouseButtonCenter ? kCGEventOtherMouseUp : kCGEventLeftMouseUp);
    else if (self.pressedButtons.count) eventType = mouseButton == kCGMouseButtonRight ? kCGEventRightMouseDragged : (mouseButton == kCGMouseButtonCenter ? kCGEventOtherMouseDragged : kCGEventLeftMouseDragged);
    else eventType = kCGEventMouseMoved;
    CGEventRef event = CGEventCreateMouseEvent(NULL, eventType, point, mouseButton);
    if (event) { CGEventPost(kCGHIDEventTap, event); CFRelease(event); }
    if (action == 1) [self.pressedButtons addObject:@(button)];
    if (action == 2) [self.pressedButtons removeObject:@(button)];
}

- (void)key:(uint16_t)keycode down:(BOOL)down {
    @synchronized (self) { if (self.stopped && down) return; }
    CGEventSourceRef source = RMRemoteInputEventSource();
    if (!source) return;
    CGEventRef event = CGEventCreateKeyboardEvent(source, (CGKeyCode)keycode, down);
    if (!event) return;
    CGEventFlags flags = 0;

    BOOL shiftDown = [self.pressedKeys containsObject:@56] || [self.pressedKeys containsObject:@60];
    BOOL controlDown = [self.pressedKeys containsObject:@59] || [self.pressedKeys containsObject:@62];
    BOOL alternateDown = [self.pressedKeys containsObject:@58] || [self.pressedKeys containsObject:@61];
    BOOL commandDown = [self.pressedKeys containsObject:@55] || [self.pressedKeys containsObject:@54];
    if (keycode == 56 || keycode == 60) {
        shiftDown = down || [self.pressedKeys containsObject:@(keycode == 56 ? 60 : 56)];
    } else if (keycode == 59 || keycode == 62) {
        controlDown = down || [self.pressedKeys containsObject:@(keycode == 59 ? 62 : 59)];
    } else if (keycode == 58 || keycode == 61) {
        alternateDown = down || [self.pressedKeys containsObject:@(keycode == 58 ? 61 : 58)];
    } else if (keycode == 55 || keycode == 54) {
        commandDown = down || [self.pressedKeys containsObject:@(keycode == 55 ? 54 : 55)];
    }
    if (shiftDown) flags |= kCGEventFlagMaskShift;
    if (controlDown) flags |= kCGEventFlagMaskControl;
    if (alternateDown) flags |= kCGEventFlagMaskAlternate;
    if (commandDown) flags |= kCGEventFlagMaskCommand;
    CGEventSetFlags(event, flags);
    CGEventPost(kCGHIDEventTap, event);
    CFRelease(event);
    if (down) [self.pressedKeys addObject:@(keycode)];
    else [self.pressedKeys removeObject:@(keycode)];
}

- (void)text:(NSString *)text {
    if (self.stopped || text.length == 0) return;
    CGEventSourceRef source = RMRemoteInputEventSource();
    if (!source) return;
    NSUInteger offset = 0;
    while (offset < text.length) {
        NSUInteger count = MIN((NSUInteger)20, text.length - offset);
        if (offset + count < text.length && count > 0) {
            unichar last = [text characterAtIndex:offset + count - 1];
            unichar next = [text characterAtIndex:offset + count];
            if (last >= 0xD800 && last <= 0xDBFF && next >= 0xDC00 && next <= 0xDFFF) count--;
        }
        unichar characters[20];
        [text getCharacters:characters range:NSMakeRange(offset, count)];
        CGEventRef down = CGEventCreateKeyboardEvent(source, 0, true);
        if (!down) { offset += count; continue; }
        CGEventKeyboardSetUnicodeString(down, (UniCharCount)count, characters);
        CGEventPost(kCGHIDEventTap, down);
        CFRelease(down);
        CGEventRef up = CGEventCreateKeyboardEvent(source, 0, false);
        if (!up) { offset += count; continue; }
        CGEventKeyboardSetUnicodeString(up, (UniCharCount)count, characters);
        CGEventPost(kCGHIDEventTap, up);
        CFRelease(up);
        offset += count;
    }
}

- (void)releasePressedInputs {
    for (NSNumber *keycode in self.pressedKeys.copy) [self key:keycode.unsignedShortValue down:NO];
    CGEventRef current = CGEventCreate(NULL);
    CGPoint point = current ? CGEventGetLocation(current) : CGPointZero;
    if (current) CFRelease(current);
    for (NSNumber *button in self.pressedButtons.copy) {
        CGMouseButton mouseButton = button.intValue == 2 ? kCGMouseButtonRight : (button.intValue == 1 ? kCGMouseButtonCenter : kCGMouseButtonLeft);
        CGEventType type = mouseButton == kCGMouseButtonRight ? kCGEventRightMouseUp : (mouseButton == kCGMouseButtonCenter ? kCGEventOtherMouseUp : kCGEventLeftMouseUp);
        CGEventRef event = CGEventCreateMouseEvent(NULL, type, point, mouseButton);
        if (event) { CGEventPost(kCGHIDEventTap, event); CFRelease(event); }
    }
    [self.pressedKeys removeAllObjects];
    [self.pressedButtons removeAllObjects];
}

- (void)stopWithCompletion:(dispatch_block_t)completion {
    if (![NSThread isMainThread]) {
        dispatch_sync(dispatch_get_main_queue(), ^{ [self stopWithCompletion:completion]; });
        return;
    }
    if (completion) {
        if (self.cleanupFinished) {
            completion();
            return;
        }
        if (!self.stopCompletions) self.stopCompletions = [NSMutableArray array];
        [self.stopCompletions addObject:[completion copy]];
    }
    @synchronized (self) {
        if (self.stopped) return;
        self.stopped = YES;
        self.openingStream = NO;
        ++self.configGeneration;
    }
    [self completeVideoConfigRequestsWithError:@"桌面会话已停止，视频配置已取消"];
    [self releasePressedInputs];

    __weak RMNativeSession *weakSelf = self;
    void (^beginCleanup)(void) = ^{
        RMNativeSession *strongSelf = weakSelf;
        if (!strongSelf || strongSelf.cleanupStarted || strongSelf.cleanupFinished) return;
        strongSelf.cleanupStarted = YES;
        dispatch_async(dispatch_get_global_queue(QOS_CLASS_UTILITY, 0), ^{
            [strongSelf finishStopCleanup];
        });
    };
    SCStream *stream = self.stream;
    if (!stream) {
        beginCleanup();
        return;
    }
    [stream stopCaptureWithCompletionHandler:^(NSError *error) {
        if (error) NSLog(@"停止 ScreenCaptureKit 返回错误，将继续回收虚拟屏幕：%@", error.localizedDescription);
        dispatch_async(dispatch_get_main_queue(), beginCleanup);
    }];
    dispatch_after(dispatch_time(DISPATCH_TIME_NOW, (int64_t)(10 * NSEC_PER_SEC)), dispatch_get_main_queue(), ^{
        RMNativeSession *strongSelf = weakSelf;
        if (!strongSelf || strongSelf.cleanupStarted || strongSelf.cleanupFinished) return;
        NSLog(@"停止 ScreenCaptureKit 超时，后台执行虚拟屏幕回收：%u", strongSelf.displayID);
        beginCleanup();
    });
}

- (void)finishStopCleanup {
    @autoreleasepool {
        if (self.cursorScaleChanged) {
            if (rm_set_cursor_scale(1.0f) == 0) {
                self.cursorScaleChanged = NO;
            } else {
                NSLog(@"RemoteMAC 恢复鼠标大小失败");
            }
        }
        if (self.targetDisplayID && self.displayID && CGDisplayMirrorsDisplay(self.targetDisplayID) == self.displayID) {
            char error[256] = {0};
            CGDirectDisplayID restore = self.previousMirrorID;
            if (restore != 0 && CGDisplayIsOnline(restore) == 0) restore = 0;
            if (!RMConfigureMirror(self.targetDisplayID, restore, error, sizeof(error))
                || !RMWaitForMirrorOffMain(self.targetDisplayID, restore, 50000000)) {
                NSLog(@"RemoteMAC 恢复镜像关系失败：%@", RMStringFromUTF8(error, @"未知错误"));
            }
        }

        VTCompressionSessionRef compression = NULL;
        @synchronized (self) { compression = self.compression; }
        if (compression) {
            VTCompressionSessionCompleteFrames(compression, kCMTimeInvalid);
            VTCompressionSessionInvalidate(compression);
            CFRelease(compression);
            @synchronized (self) {
                if (self.compression == compression) self.compression = NULL;
            }
        }

        @synchronized (self) { self.stream = nil; }
        void *displayRef = NULL;
        @synchronized (self) {
            displayRef = self.displayRef;
            self.displayRef = NULL;
        }
        BOOL displayRemoved = YES;
        if (displayRef) {
            SRDestroyDisplay(displayRef);
            uint64_t deadline = RMDeadlineAfter(5.0);
            while (mach_absolute_time() < deadline && RMIsDisplayOnline(self.displayID)) usleep(25000);
            displayRemoved = !RMIsDisplayOnline(self.displayID);
        }

        dispatch_async(dispatch_get_main_queue(), ^{
            if (!displayRemoved) {
                NSLog(@"等待 RemoteMAC 虚拟屏幕离线超时，暂时保留显示器锁：%u", self.displayID);
                [RMRetiredSessions addObject:self];
                dispatch_async(dispatch_get_global_queue(QOS_CLASS_UTILITY, 0), ^{
                    while (RMIsDisplayOnline(self.displayID)) usleep(25000);
                    dispatch_async(dispatch_get_main_queue(), ^{
                        [self completeStopCleanup];
                    });
                });
                return;
            }
            [self completeStopCleanup];
        });
    }
}

- (void)completeStopCleanup {
    if (self.cleanupFinished) return;
    if (self.lockFD >= 0) { close(self.lockFD); self.lockFD = -1; }
    [RMRetiredSessions removeObject:self];
    if (RMCurrentSession == self) RMCurrentSession = nil;
    self.cleanupFinished = YES;
    NSArray *completions = self.stopCompletions.copy;
    [self.stopCompletions removeAllObjects];
    for (dispatch_block_t completion in completions) completion();
}

- (void)stop {
    __block BOOL finished = NO;
    [self stopWithCompletion:^{ finished = YES; }];
    while (!finished) CFRunLoopRunInMode(kCFRunLoopDefaultMode, 0.025, true);
}

- (void)dealloc {
    if (_displayRef) SRDestroyDisplay(_displayRef);
    if (_compression) {
        VTCompressionSessionInvalidate(_compression);
        CFRelease(_compression);
    }
    if (_lockFD >= 0) close(_lockFD);
}
@end

int rm_app_initialize(void) {
    @autoreleasepool {
        [NSApplication sharedApplication];
        RMRetiredSessions = [NSMutableSet set];
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
        return 0;
    }
}

void rm_app_run(void) { @autoreleasepool { [NSApp run]; } }

void rm_app_stop(void) {
    dispatch_async(dispatch_get_main_queue(), ^{
        [NSApp stop:nil];
        NSEvent *event = [NSEvent otherEventWithType:NSEventTypeApplicationDefined location:NSZeroPoint modifierFlags:0 timestamp:0 windowNumber:0 context:nil subtype:0 data1:0 data2:0];
        [NSApp postEvent:event atStart:YES];
    });
}

int rm_capture_start(uintptr_t handle, uint32_t screenWidth, uint32_t screenHeight, uint32_t width, uint32_t height, uint32_t fps, uint32_t bitrate, char *error, size_t errorLength) {
    __block int result = 0;
    __block NSString *failure = nil;
    __block RMNativeSession *failedSession = nil;
    void (^create)(void) = ^{
        if (RMCurrentSession) { failure = @"已有 RemoteMAC 桌面会话运行"; return; }
        if (RMRetiredSessions.count > 0) {
            failure = @"上一个 RemoteMAC 虚拟屏幕仍在系统回收，请稍后重新连接";
            return;
        }
        RMNativeSession *session = [[RMNativeSession alloc] init];
        session.callbackHandle = handle;
        session.width = width;
        session.height = height;
        session.screenWidth = screenWidth;
        session.screenHeight = screenHeight;
        dispatch_queue_attr_t captureQueueAttributes = dispatch_queue_attr_make_with_qos_class(
            DISPATCH_QUEUE_SERIAL, QOS_CLASS_USER_INTERACTIVE, 0
        );
        session.captureQueue = dispatch_queue_create("com.remotemac.capture", captureQueueAttributes);
        session.fps = fps;
        session.bitrate = bitrate;
        session.lockFD = -1;
        session.pressedKeys = [NSMutableSet set];
        session.pressedButtons = [NSMutableSet set];
        session.videoConfigRequests = [NSMutableArray array];
        if (![session start:error errorLength:errorLength]) {
            failure = error && error[0] ? [NSString stringWithUTF8String:error] : @"启动虚拟屏幕失败";
            failedSession = session;
            // Retain the session until its caller can wait for display cleanup.
            [RMRetiredSessions addObject:session];
            return;
        }
        // start: has installed the asynchronous ScreenCaptureKit discovery
        // callback. It cannot complete until this main-queue block returns, so
        // register the startup request before that callback opens the stream.
        [session.videoConfigRequests addObject:@((unsigned long long)handle)];
        float cursorScale = RMCursorScaleForResolution(session.activeLogicalWidth, session.activeLogicalHeight);
        int cursorScaleStatus = rm_set_cursor_scale(cursorScale);
        session.cursorScaleChanged = cursorScale > 1.0f && cursorScaleStatus == 0;
        if (cursorScaleStatus != 0) {
            NSLog(@"RemoteMAC 无法应用鼠标缩放系数=%.2f，保留系统鼠标大小", cursorScale);
        } else {
            NSLog(@"RemoteMAC 鼠标缩放已应用：最长边=%u，系数=%.2f",
                  session.activeLogicalWidth > session.activeLogicalHeight
                      ? session.activeLogicalWidth : session.activeLogicalHeight,
                  cursorScale);
        }
        RMCurrentSession = session;
    };
    if ([NSThread isMainThread]) create(); else dispatch_sync(dispatch_get_main_queue(), create);
    if (failure) {
        if (failedSession) {
            if ([NSThread isMainThread]) {
                [failedSession stop];
            } else {
                dispatch_semaphore_t cleaned = dispatch_semaphore_create(0);
                [failedSession stopWithCompletion:^{ dispatch_semaphore_signal(cleaned); }];
                dispatch_semaphore_wait(cleaned, DISPATCH_TIME_FOREVER);
            }
        }
        RMSetError(error, errorLength, failure);
        result = -1;
    }
    return result;
}

void rm_capture_stop(void) {
    if ([NSThread isMainThread]) {
        [RMCurrentSession stop];
        return;
    }
    dispatch_semaphore_t stopped = dispatch_semaphore_create(0);
    dispatch_async(dispatch_get_main_queue(), ^{
        RMNativeSession *session = RMCurrentSession;
        if (!session) {
            dispatch_semaphore_signal(stopped);
            return;
        }
        [session stopWithCompletion:^{ dispatch_semaphore_signal(stopped); }];
    });
    dispatch_semaphore_wait(stopped, DISPATCH_TIME_FOREVER);
}

void rm_capture_get_display_size(uint32_t *width, uint32_t *height) {
    __block uint32_t logicalWidth = 0;
    __block uint32_t logicalHeight = 0;
    void (^readSize)(void) = ^{
        RMNativeSession *session = RMCurrentSession;
        if (session) {
            logicalWidth = session.activeLogicalWidth;
            logicalHeight = session.activeLogicalHeight;
        }
    };
    if ([NSThread isMainThread]) readSize();
    else dispatch_sync(dispatch_get_main_queue(), readSize);
    if (width) *width = logicalWidth;
    if (height) *height = logicalHeight;
}

int rm_capture_resize_display(uint32_t width, uint32_t height, char *error, size_t errorLength) {
    __block BOOL ok = NO;
    __block BOOL recoveryFailed = NO;
    void (^resize)(void) = ^{
        RMNativeSession *session = RMCurrentSession;
        if (!session) {
            RMSetError(error, errorLength, @"当前没有活动的 RemoteMAC 虚拟屏幕");
            return;
        }
        ok = [session resizeDisplay:width height:height error:error errorLength:errorLength];
        recoveryFailed = session.displayModeRecoveryFailed;
    };
    if ([NSThread isMainThread]) resize(); else dispatch_sync(dispatch_get_main_queue(), resize);
    return ok ? 0 : (recoveryFailed ? -2 : -1);
}

int rm_capture_set_video(uintptr_t requestID, uint32_t width, uint32_t height, uint32_t fps, uint32_t bitrate, char *error, size_t errorLength) {
    __block BOOL ok = NO;
    void (^configure)(void) = ^{ ok = [RMCurrentSession setVideoWidth:width height:height fps:fps bitrate:bitrate requestID:requestID error:error errorLength:errorLength]; };
    if ([NSThread isMainThread]) configure(); else dispatch_sync(dispatch_get_main_queue(), configure);
    return ok ? 0 : -1;
}

void rm_capture_request_keyframe(void) {
    void (^request)(void) = ^{ [RMCurrentSession sendKeyframe]; };
    if ([NSThread isMainThread]) request(); else dispatch_async(dispatch_get_main_queue(), request);
}

void rm_input_pointer(int action, int button, double x, double y, uint32_t width, uint32_t height) {
	void (^inject)(void) = ^{ [RMCurrentSession pointer:action button:button x:x y:y width:width height:height]; };
	if ([NSThread isMainThread]) inject();
	else dispatch_sync(dispatch_get_main_queue(), inject);
}

void rm_input_key(uint16_t keycode, int down) {
    void (^inject)(void) = ^{ [RMCurrentSession key:keycode down:down != 0]; };
    if ([NSThread isMainThread]) inject();
    else dispatch_sync(dispatch_get_main_queue(), inject);
}

void rm_input_mission_control(void) {
    void (^openMissionControl)(void) = ^{
        NSWorkspace *workspace = [NSWorkspace sharedWorkspace];
        NSURL *applicationURL = [workspace URLForApplicationWithBundleIdentifier:@"com.apple.exposelauncher"];
        if (!applicationURL) {
            applicationURL = [NSURL fileURLWithPath:@"/System/Applications/Mission Control.app"];
        }
        if (![[NSFileManager defaultManager] fileExistsAtPath:applicationURL.path]) {
            NSLog(@"Mission Control 应用不存在：%@", applicationURL.path);
            return;
        }

        NSWorkspaceOpenConfiguration *configuration = [NSWorkspaceOpenConfiguration configuration];
        configuration.activates = YES;
        [workspace openApplicationAtURL:applicationURL configuration:configuration completionHandler:^(NSRunningApplication *application, NSError *error) {
            if (error) NSLog(@"打开 Mission Control 失败：%@", error);
        }];
    };
    if ([NSThread isMainThread]) openMissionControl();
    else dispatch_async(dispatch_get_main_queue(), openMissionControl);
}

void rm_input_text(const char *text) {
    NSString *value = text ? [NSString stringWithUTF8String:text] : @"";
    dispatch_async(dispatch_get_main_queue(), ^{ [RMCurrentSession text:value]; });
}

void rm_input_release_all(void) {
    void (^release)(void) = ^{ [RMCurrentSession releasePressedInputs]; };
    if ([NSThread isMainThread]) release();
    else dispatch_sync(dispatch_get_main_queue(), release);
}
