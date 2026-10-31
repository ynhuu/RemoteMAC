#ifndef REMOTEMAC_VIRTUAL_DISPLAY_BRIDGE_H
#define REMOTEMAC_VIRTUAL_DISPLAY_BRIDGE_H

#include <stdint.h>
#include <stdbool.h>

#define SR_MAXIMUM_BACKING_AREA 25000000ULL

// RemoteMAC keeps a stable identity so dimensions do not create new display records.
static const uint32_t SRDisplayVendorID = 0xF0F0;
static const uint32_t SRDisplayProductID = 0x524D;
static const uint32_t SRDisplaySerialNumber = 1;

// Returns one owned opaque display reference. The caller must hold its display lock
// until the operating system has removed the display after SRDestroyDisplay.
void *SRCreateDisplay(uint32_t width, uint32_t height, double refreshRate,
                      uint32_t *displayID, const char **failureStage);
bool SRUpdateDisplayMode(void *display, uint32_t width, uint32_t height,
                         double refreshRate, const char **failureStage);
void SRDestroyDisplay(void *display);

typedef struct {
    int32_t status;
    int32_t modeCount;
    int32_t targetIndex;
    int32_t activeIndex;
    uint32_t activeWidth;
    uint32_t activeHeight;
    uint32_t activePixelWidth;
    uint32_t activePixelHeight;
    float activeDensity;
    int32_t configurationStatus;
    uint32_t targetIsUsable;
    uint32_t targetIOFlags;
} SRDisplayModeResult;

void SRSelectDisplayMode(uint32_t displayID, uint32_t logicalWidth,
                         uint32_t logicalHeight, float density,
                         SRDisplayModeResult *result);

#endif
