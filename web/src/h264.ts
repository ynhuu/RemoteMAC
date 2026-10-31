export type H264DecodeLimits = {
  maxFS: number;
  maxMBPS: number;
};

type DecodeTarget = H264DecodeLimits & {
  width: number;
  height: number;
  fps: number;
  bitrate: number;
};

const decodeTargets: DecodeTarget[] = [
  { width: 4096, height: 2160, fps: 60, bitrate: 8_000_000, maxFS: 36_864, maxMBPS: 2_073_600 },
  { width: 3840, height: 2160, fps: 60, bitrate: 5_000_000, maxFS: 32_400, maxMBPS: 1_944_000 },
  { width: 2732, height: 2048, fps: 60, bitrate: 5_000_000, maxFS: 21_888, maxMBPS: 1_313_280 },
  { width: 2560, height: 1440, fps: 60, bitrate: 5_000_000, maxFS: 14_400, maxMBPS: 864_000 },
  { width: 2560, height: 1440, fps: 30, bitrate: 4_000_000, maxFS: 14_400, maxMBPS: 432_000 },
  { width: 1920, height: 1080, fps: 60, bitrate: 4_000_000, maxFS: 8_160, maxMBPS: 489_600 },
  { width: 1920, height: 1080, fps: 30, bitrate: 3_000_000, maxFS: 8_160, maxMBPS: 244_800 },
  { width: 1280, height: 720, fps: 60, bitrate: 2_000_000, maxFS: 3_600, maxMBPS: 216_000 },
  { width: 1280, height: 720, fps: 30, bitrate: 1_500_000, maxFS: 3_600, maxMBPS: 108_000 },
];

async function queryDecodeTarget(target: DecodeTarget): Promise<H264DecodeLimits | null> {
  const mediaCapabilities = navigator.mediaCapabilities;
  if (!mediaCapabilities || typeof mediaCapabilities.decodingInfo !== "function") return null;

  const contentType = [
    "video/H264",
    "level-asymmetry-allowed=1",
    "packetization-mode=1",
    "profile-level-id=42e01f",
    `max-fs=${target.maxFS}`,
    `max-mbps=${target.maxMBPS}`,
  ].join(";");
  let timeout = 0;
  const timedOut = new Promise<null>((resolve) => {
    timeout = window.setTimeout(() => resolve(null), 650);
  });

  try {
    const result = await Promise.race([
      mediaCapabilities.decodingInfo({
        type: "webrtc",
        video: {
          contentType,
          width: target.width,
          height: target.height,
          bitrate: target.bitrate,
          framerate: target.fps,
        },
      }),
      timedOut,
    ]);
    return result?.supported && result.smooth ? { maxFS: target.maxFS, maxMBPS: target.maxMBPS } : null;
  } catch {
    return null;
  } finally {
    window.clearTimeout(timeout);
  }
}

// Test larger real-time decode configurations before advertising H.264
// max-fs/max-mbps extensions in the WebRTC offer.
export async function detectH264DecodeLimits(): Promise<H264DecodeLimits | null> {
  if (!navigator.mediaCapabilities || typeof navigator.mediaCapabilities.decodingInfo !== "function") return null;
  const results = await Promise.all(decodeTargets.map(queryDecodeTarget));
  return results.find((result): result is H264DecodeLimits => result !== null) ?? null;
}

function raiseFormatLimit(parameters: string, name: "max-fs" | "max-mbps", value: number): string {
  const parts = parameters.split(";").map((part) => part.trim());
  const prefix = `${name}=`;
  const index = parts.findIndex((part) => part.toLowerCase().startsWith(prefix));
  if (index < 0) {
    parts.push(`${name}=${value}`);
    return parts.join(";");
  }

  const existing = Number(parts[index].slice(prefix.length));
  parts[index] = `${name}=${Number.isFinite(existing) && existing > value ? existing : value}`;
  return parts.join(";");
}

// H.264 receiver capabilities are carried in offer-side fmtp parameters. Keep
// the browser's profile level and only add limits verified by MediaCapabilities.
export function advertiseH264DecodeLimits(sdp: string, limits: H264DecodeLimits): string {
  return sdp.split(/(?=^m=)/m).map((section) => {
    if (!/^m=video\s/m.test(section)) return section;
    const h264Payloads = new Set(
      [...section.matchAll(/^a=rtpmap:(\d+)\s+H264\/\d+(?:\/\d+)?\s*$/gim)].map((match) => match[1]),
    );
    if (h264Payloads.size === 0) return section;

    return section.replace(/^a=fmtp:(\d+)\s+([^\r\n]*)/gim, (line, payload: string, rawParameters: string) => {
      if (!h264Payloads.has(payload)) return line;
      if (!/(?:^|;)\s*packetization-mode\s*=\s*1(?:;|$)/i.test(rawParameters)) return line;
      const profileLevel = /(?:^|;)\s*profile-level-id\s*=\s*([0-9a-f]{6})(?:;|$)/i.exec(rawParameters)?.[1];
      if (!profileLevel || !profileLevel.toLowerCase().startsWith("42e0")) return line;
      let parameters = raiseFormatLimit(rawParameters, "max-fs", limits.maxFS);
      parameters = raiseFormatLimit(parameters, "max-mbps", limits.maxMBPS);
      return `a=fmtp:${payload} ${parameters}`;
    });
  }).join("");
}
