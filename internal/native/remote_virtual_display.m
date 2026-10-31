#import <Foundation/Foundation.h>
#import <CoreGraphics/CoreGraphics.h>
#include "VirtualDisplayBridge.h"
#import <math.h>
#import <stdlib.h>
#import <string.h>

@interface CGVirtualDisplayDescriptor : NSObject
@property(nonatomic) unsigned int maxPixelsWide;
@property(nonatomic) unsigned int maxPixelsHigh;
@property(nonatomic) unsigned int vendorID;
@property(nonatomic) unsigned int productID;
@property(nonatomic) unsigned int serialNum;
@property(nonatomic) unsigned int serialNumber;
@property(nonatomic) CGSize sizeInMillimeters;
@property(nonatomic) CGPoint whitePoint;
@property(nonatomic) CGPoint redPrimary;
@property(nonatomic) CGPoint greenPrimary;
@property(nonatomic) CGPoint bluePrimary;
@property(nonatomic, copy) NSString *name;
@property(nonatomic, strong) dispatch_queue_t queue;
@end

@interface CGVirtualDisplayMode : NSObject
- (instancetype)initWithWidth:(unsigned int)width height:(unsigned int)height
                  refreshRate:(double)refreshRate;
@end

@interface CGVirtualDisplaySettings : NSObject
@property(nonatomic) unsigned int hiDPI;
@property(nonatomic) unsigned int rotation;
@property(nonatomic, strong) NSArray *modes;
@end

@interface CGVirtualDisplay : NSObject
- (instancetype)initWithDescriptor:(CGVirtualDisplayDescriptor *)descriptor;
- (BOOL)applySettings:(CGVirtualDisplaySettings *)settings;
@property(nonatomic, readonly) unsigned int displayID;
@end

static uint32_t SRGreatestCommonDivisor(uint32_t a, uint32_t b) {
    while (b != 0) {
        uint32_t remainder = a % b;
        a = b;
        b = remainder;
    }
    return a;
}

static NSArray *SRBuildDisplayModes(Class modeClass, uint32_t width,
                                    uint32_t height, double refreshRate,
                                    const char **failureStage) {
    const double modeRefreshRate = isfinite(refreshRate)
        && refreshRate >= 30.0 && refreshRate <= 120.0
        ? refreshRate : 60.0;
    NSMutableArray *modes = [NSMutableArray array];
    CGVirtualDisplayMode *preferredMode = [[modeClass alloc]
        initWithWidth:width height:height refreshRate:modeRefreshRate];
    if (!preferredMode) {
        if (failureStage) *failureStage = "preferred mode init";
        return nil;
    }
    [modes addObject:preferredMode];

    const uint32_t aspectDivisor = SRGreatestCommonDivisor(width, height);
    const uint32_t aspectWidth = width / aspectDivisor;
    const uint32_t aspectHeight = height / aspectDivisor;
    const uint32_t minWidthMultiplier = (720 + aspectWidth * 2 - 1) / (aspectWidth * 2);
    const uint32_t minHeightMultiplier = (720 + aspectHeight * 2 - 1) / (aspectHeight * 2);
    const uint32_t minMultiplier = MAX(minWidthMultiplier, minHeightMultiplier);
    const double maxMultiplierByArea = sqrt((double)SR_MAXIMUM_BACKING_AREA /
                                            (4.0 * (double)aspectWidth * aspectHeight));
    const uint32_t maxMultiplier = (uint32_t)floor(maxMultiplierByArea);
    if (maxMultiplier >= minMultiplier) {
        const uint32_t multiplierCount = maxMultiplier - minMultiplier + 1;
        const uint32_t multiplierStride = MAX(1, (multiplierCount + 255) / 256);
        for (uint32_t multiplier = minMultiplier; multiplier <= maxMultiplier;
             multiplier += multiplierStride) {
            const uint32_t modeWidth = aspectWidth * multiplier * 2;
            const uint32_t modeHeight = aspectHeight * multiplier * 2;
            if (modeWidth == width && modeHeight == height) continue;
            CGVirtualDisplayMode *mode = [[modeClass alloc]
                initWithWidth:modeWidth height:modeHeight refreshRate:modeRefreshRate];
            if (!mode) {
                if (failureStage) *failureStage = "mode init";
                return nil;
            }
            [modes addObject:mode];
        }
    }
    return modes;
}

void *SRCreateDisplay(uint32_t width, uint32_t height, double refreshRate,
                      uint32_t *displayID, const char **failureStage) {
    @autoreleasepool {
        *displayID = 0;
        if (width < 320 || height < 200 || (uint64_t)width * height > SR_MAXIMUM_BACKING_AREA) {
            *failureStage = "backing dimensions exceed area limit";
            return NULL;
        }
        *failureStage = "private class lookup";
        Class descriptorClass = NSClassFromString(@"CGVirtualDisplayDescriptor");
        Class displayClass = NSClassFromString(@"CGVirtualDisplay");
        Class modeClass = NSClassFromString(@"CGVirtualDisplayMode");
        Class settingsClass = NSClassFromString(@"CGVirtualDisplaySettings");
        if (!descriptorClass || !displayClass || !modeClass || !settingsClass) return NULL;

        CGVirtualDisplayDescriptor *descriptor = [[descriptorClass alloc] init];
        *failureStage = "descriptor init";
        if (!descriptor) return NULL;
        // Keep enough headroom to switch between portrait and landscape modes
        // on this same display object after a client rotation. The descriptor
        // covers every canvas allowed by the backing-area policy.
        descriptor.maxPixelsWide = (unsigned int)(SR_MAXIMUM_BACKING_AREA / 200U);
        descriptor.maxPixelsHigh = (unsigned int)(SR_MAXIMUM_BACKING_AREA / 200U);
        // Mode geometry may change; hardware identity must not. Random serials
        // or aspect-derived products accumulate persistent ColorSync records.
        descriptor.vendorID = SRDisplayVendorID;
        descriptor.productID = SRDisplayProductID;
        descriptor.serialNum = SRDisplaySerialNumber;
        if ([descriptor respondsToSelector:@selector(setSerialNumber:)]) {
            descriptor.serialNumber = SRDisplaySerialNumber;
        }
        descriptor.name = @"RemoteMAC";
        descriptor.queue = dispatch_get_global_queue(QOS_CLASS_USER_INTERACTIVE, 0);
        const uint32_t aspectDivisor = SRGreatestCommonDivisor(width, height);
        const uint32_t aspectWidth = width / aspectDivisor;
        const uint32_t aspectHeight = height / aspectDivisor;
        const double diagonalSizeRatio = (24.0 * 25.4) /
            sqrt((double)aspectWidth * aspectWidth + (double)aspectHeight * aspectHeight);
        descriptor.sizeInMillimeters = CGSizeMake(aspectWidth * diagonalSizeRatio,
                                                   aspectHeight * diagonalSizeRatio);
        // CIE 1931 xy coordinates for standard sRGB / D65.
        descriptor.whitePoint = CGPointMake(0.3127, 0.3290);
        descriptor.redPrimary = CGPointMake(0.640, 0.330);
        descriptor.greenPrimary = CGPointMake(0.300, 0.600);
        descriptor.bluePrimary = CGPointMake(0.150, 0.060);

        // Preserve nearby usable 2x modes if WindowServer rejects the exact
        // requested backing mode.
        NSArray *modes = SRBuildDisplayModes(modeClass, width, height,
                                              refreshRate, failureStage);
        if (!modes) return NULL;
        CGVirtualDisplaySettings *settings = [[settingsClass alloc] init];
        *failureStage = "settings init";
        if (!settings) return NULL;
        settings.hiDPI = 1;
        if ([settings respondsToSelector:@selector(setRotation:)]) settings.rotation = 0;
        settings.modes = modes;
        CGVirtualDisplay *display = [[displayClass alloc] initWithDescriptor:descriptor];
        *failureStage = "initWithDescriptor returned nil";
        if (!display) return NULL;
        *displayID = display.displayID;
        *failureStage = "applySettings returned NO";
        if (![display applySettings:settings]) return NULL;
        *failureStage = "displayID returned zero";
        if (display.displayID == 0) return NULL;
        *displayID = display.displayID;
        *failureStage = NULL;
        return (__bridge_retained void *)display;
    }
}

bool SRUpdateDisplayMode(void *display, uint32_t width, uint32_t height,
                         double refreshRate, const char **failureStage) {
    @autoreleasepool {
        if (failureStage) *failureStage = "invalid arguments";
        if (!display || width < 320 || height < 200 || (uint64_t)width * height > SR_MAXIMUM_BACKING_AREA) {
            return false;
        }

        Class modeClass = NSClassFromString(@"CGVirtualDisplayMode");
        Class settingsClass = NSClassFromString(@"CGVirtualDisplaySettings");
        if (!modeClass || !settingsClass) {
            if (failureStage) *failureStage = "private class lookup";
            return false;
        }

        // Match display creation so every resize keeps nearby usable HiDPI
        // modes available when WindowServer rejects the exact mode.
        NSArray *modes = SRBuildDisplayModes(modeClass, width, height,
                                              refreshRate, failureStage);
        if (!modes) return false;

        CGVirtualDisplaySettings *settings = [[settingsClass alloc] init];
        if (!settings) {
            if (failureStage) *failureStage = "settings init";
            return false;
        }
        settings.hiDPI = 1;
        if ([settings respondsToSelector:@selector(setRotation:)]) settings.rotation = 0;
        settings.modes = modes;

        CGVirtualDisplay *virtualDisplay = (__bridge CGVirtualDisplay *)display;
        if (![virtualDisplay applySettings:settings]) {
            if (failureStage) *failureStage = "applySettings returned NO";
            return false;
        }
        if (failureStage) *failureStage = NULL;
        return true;
    }
}

void SRDestroyDisplay(void *display) {
    @autoreleasepool {
        if (display) CFRelease(display);
    }
}

void SRSelectDisplayMode(uint32_t displayID, uint32_t logicalWidth,
                         uint32_t logicalHeight, float density,
                         SRDisplayModeResult *result) {
    if (!result) return;
    memset(result, 0, sizeof(*result));
    result->status = -1;
    result->targetIndex = -1;
    result->activeIndex = -1;
    if (!displayID || !logicalWidth || !logicalHeight || !isfinite(density) || density <= 0) return;

    @autoreleasepool {
        NSDictionary *options = @{
            (__bridge NSString *)kCGDisplayShowDuplicateLowResolutionModes: @YES,
        };
        CFArrayRef copiedModes = CGDisplayCopyAllDisplayModes(
            displayID, (__bridge CFDictionaryRef)options);
        if (!copiedModes) {
            result->status = 0;
            return;
        }
        CFIndex modeCount = CFArrayGetCount(copiedModes);
        if (modeCount > INT32_MAX) {
            result->status = -4;
            CFRelease(copiedModes);
            return;
        }
        result->modeCount = (int32_t)modeCount;
        const size_t targetPixelWidth = (size_t)llround((double)logicalWidth * density);
        const size_t targetPixelHeight = (size_t)llround((double)logicalHeight * density);
        CGDisplayModeRef targetMode = NULL;
        const double targetAspect = targetPixelHeight > 0
            ? (double)targetPixelWidth / (double)targetPixelHeight : 0;
        for (CFIndex index = 0; index < modeCount; index++) {
            CGDisplayModeRef mode = (CGDisplayModeRef)CFArrayGetValueAtIndex(copiedModes, index);
            if (CGDisplayModeGetWidth(mode) != logicalWidth
                || CGDisplayModeGetHeight(mode) != logicalHeight
                || CGDisplayModeGetPixelWidth(mode) != targetPixelWidth
                || CGDisplayModeGetPixelHeight(mode) != targetPixelHeight) continue;
            if (!CGDisplayModeIsUsableForDesktopGUI(mode)) continue;
            targetMode = mode;
            result->targetIndex = (int32_t)index;
            result->targetIsUsable = 1;
            result->targetIOFlags = CGDisplayModeGetIOFlags(mode);
            break;
        }

        // If the exact 2x mode is unsafe, choose the smallest usable HiDPI
        // backing that still covers the requested capture dimensions. This
        // keeps the aspect ratio while allowing macOS to use a nearby logical
        // desktop size, such as 1656x1038 for a 2208x1384 capture.
        if (!targetMode && density >= 1.99f && targetAspect > 0) {
            uint64_t bestBackingArea = UINT64_MAX;
            for (CFIndex index = 0; index < modeCount; index++) {
                CGDisplayModeRef mode = (CGDisplayModeRef)CFArrayGetValueAtIndex(copiedModes, index);
                if (!CGDisplayModeIsUsableForDesktopGUI(mode)) continue;
                const size_t modeWidth = CGDisplayModeGetWidth(mode);
                const size_t modeHeight = CGDisplayModeGetHeight(mode);
                const size_t modePixelWidth = CGDisplayModeGetPixelWidth(mode);
                const size_t modePixelHeight = CGDisplayModeGetPixelHeight(mode);
                if (!modeWidth || !modeHeight || !modePixelHeight
                    || modePixelWidth < targetPixelWidth || modePixelHeight < targetPixelHeight
                    || modeWidth > targetPixelWidth || modeHeight > targetPixelHeight) continue;
                const double densityX = (double)modePixelWidth / (double)modeWidth;
                const double densityY = (double)modePixelHeight / (double)modeHeight;
                const double modeAspect = (double)modePixelWidth / (double)modePixelHeight;
                if (densityX < 1.99 || densityY < 1.99
                    || fabs(modeAspect / targetAspect - 1.0) > 0.005) continue;
                const uint64_t backingArea = (uint64_t)modePixelWidth * modePixelHeight;
                if (backingArea >= bestBackingArea) continue;
                bestBackingArea = backingArea;
                targetMode = mode;
                result->targetIndex = (int32_t)index;
                result->targetIsUsable = 1;
                result->targetIOFlags = CGDisplayModeGetIOFlags(mode);
            }
        }

        CGDisplayModeRef currentMode = CGDisplayCopyDisplayMode(displayID);
        if (!currentMode) {
            uint32_t onlineCount = 0;
            if (CGGetOnlineDisplayList(0, NULL, &onlineCount) == kCGErrorSuccess) {
                CGDirectDisplayID *onlineIDs = calloc(onlineCount ?: 1, sizeof(*onlineIDs));
                if (onlineIDs && CGGetOnlineDisplayList(onlineCount, onlineIDs, &onlineCount) == kCGErrorSuccess) {
                    for (uint32_t index = 0; index < onlineCount; index++) {
                        if (CGDisplayMirrorsDisplay(onlineIDs[index]) == displayID) {
                            currentMode = CGDisplayCopyDisplayMode(onlineIDs[index]);
                            if (currentMode) break;
                        }
                    }
                }
                free(onlineIDs);
            }
        }
        if (currentMode) {
            const size_t activeWidth = CGDisplayModeGetWidth(currentMode);
            const size_t activeHeight = CGDisplayModeGetHeight(currentMode);
            const size_t activePixelWidth = CGDisplayModeGetPixelWidth(currentMode);
            const size_t activePixelHeight = CGDisplayModeGetPixelHeight(currentMode);
            result->activeWidth = (uint32_t)activeWidth;
            result->activeHeight = (uint32_t)activeHeight;
            result->activePixelWidth = (uint32_t)activePixelWidth;
            result->activePixelHeight = (uint32_t)activePixelHeight;
            result->activeDensity = activeWidth > 0
                ? (float)activePixelWidth / (float)activeWidth : 0;
            for (CFIndex index = 0; index < modeCount; index++) {
                CGDisplayModeRef mode = (CGDisplayModeRef)CFArrayGetValueAtIndex(copiedModes, index);
                if (CGDisplayModeGetWidth(mode) == activeWidth
                    && CGDisplayModeGetHeight(mode) == activeHeight
                    && CGDisplayModeGetPixelWidth(mode) == activePixelWidth
                    && CGDisplayModeGetPixelHeight(mode) == activePixelHeight) {
                    result->activeIndex = (int32_t)index;
                    break;
                }
            }
            if (activeWidth == logicalWidth && activeHeight == logicalHeight
                && activePixelWidth == targetPixelWidth && activePixelHeight == targetPixelHeight) {
                result->status = 1;
                CFRelease(currentMode);
                CFRelease(copiedModes);
                return;
            }
            CFRelease(currentMode);
        }
        if (!targetMode) {
            result->status = 0;
            CFRelease(copiedModes);
            return;
        }

        CGDisplayConfigRef config = NULL;
        CGError beginStatus = CGBeginDisplayConfiguration(&config);
        result->configurationStatus = beginStatus;
        if (beginStatus != kCGErrorSuccess || !config) {
            result->status = -2;
            CFRelease(copiedModes);
            return;
        }
        CGError configureStatus = CGConfigureDisplayWithDisplayMode(
            config, displayID, targetMode, NULL);
        if (configureStatus != kCGErrorSuccess) {
            CGCancelDisplayConfiguration(config);
            result->configurationStatus = configureStatus;
            result->status = -3;
            CFRelease(copiedModes);
            return;
        }
        CGError completeStatus = CGCompleteDisplayConfiguration(config, kCGConfigureForSession);
        result->configurationStatus = completeStatus;
        CFRelease(copiedModes);
        result->status = completeStatus == kCGErrorSuccess ? 2 : -3;
    }
}
