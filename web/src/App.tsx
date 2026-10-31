import { useCallback, useEffect, useId, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type PointerEvent as ReactPointerEvent } from "react";
import { advertiseH264DecodeLimits, detectH264DecodeLimits } from "./h264";

type Signal = {
  type: string;
  sdp?: string;
  candidate?: string;
  sdpMid?: string | null;
  sdpMLineIndex?: number | null;
  state?: string;
  error?: string;
  fatal?: boolean;
  retryable?: boolean;
  width?: number;
  height?: number;
  displayWidth?: number;
  displayHeight?: number;
  encodeWidth?: number;
  encodeHeight?: number;
  fps?: number;
  bitrate?: number;
  mapVersion?: number;
};

type ConnectionStats = {
  bitrate?: number;
  displayWidth?: number;
  displayHeight?: number;
  rtt?: number;
  decodeFPS?: number;
  jitterBufferMs?: number;
  decodeMs?: number;
};
type FloatingMetric = "display" | "fps" | "bitrate" | "playout";

const authorizedStatus = "访问已授权，可连接桌面";
const usesNativeIOSSelect = typeof navigator !== "undefined" &&
  (/iPhone|iPad|iPod/.test(navigator.userAgent) || (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1));

type VideoPointerPlane = {
  width: number;
  height: number;
  left: number;
  top: number;
  drawWidth: number;
  drawHeight: number;
};
type PointerMode = "touch" | "mouse";

type ViewportMetrics = { width: number; height: number };

const maximumDisplayArea = 25_000_000;

function getViewportMetrics(): ViewportMetrics {
  const stage = document.querySelector(".remote-stage")?.getBoundingClientRect();
  if (stage && stage.width > 0 && stage.height > 0) {
    return { width: stage.width, height: stage.height };
  }
  const viewport = window.visualViewport;
  return {
    width: Math.max(1, viewport?.width || window.innerWidth || document.documentElement.clientWidth),
    height: Math.max(1, viewport?.height || window.innerHeight || document.documentElement.clientHeight),
  };
}

function videoPointerPlane(video: HTMLVideoElement): VideoPointerPlane {
  const bounds = video.getBoundingClientRect();
  const width = video.videoWidth || Math.max(1, Math.round(bounds.width));
  const height = video.videoHeight || Math.max(1, Math.round(bounds.height));
  const scale = Math.min(bounds.width / width, bounds.height / height);
  const drawWidth = width * scale;
  const drawHeight = height * scale;
  return {
    width,
    height,
    left: bounds.left + (bounds.width - drawWidth) / 2,
    top: bounds.top + (bounds.height - drawHeight) / 2,
    drawWidth,
    drawHeight,
  };
}

function displayPixelsFor(element: Element | null, resolutionScale: number): { width: number; height: number } {
  const scaleFactor = Math.max(0.1, Math.min(1, resolutionScale / 100));
  const rect = element?.getBoundingClientRect();
  if (!rect || rect.width <= 0 || rect.height <= 0) {
    const scale = scaleFactor;
    return { width: Math.max(320, Math.floor(1920 * scale / 4) * 4), height: Math.max(200, Math.floor(1080 * scale / 4) * 4) };
  }

  // Match the virtual display to the actual video stage after safe-area insets.
  const screenWidth = rect.width;
  const screenHeight = rect.height;
  const ratio = screenWidth / screenHeight;
  const deviceScale = Math.min(window.devicePixelRatio || 1, 2);
  const scale = deviceScale * scaleFactor;
  let width = Math.floor(screenWidth * scale);
  let height = Math.floor(screenHeight * scale);
  if (width < 320) { width = 320; height = Math.floor(width / ratio); }
  if (height < 200) { height = 200; width = Math.floor(height * ratio); }

  const area = width * height;
  if (area > maximumDisplayArea) {
    const areaScale = Math.sqrt(maximumDisplayArea / area);
    width = Math.floor(width * areaScale / 4) * 4;
    height = Math.floor(height * areaScale / 4) * 4;
    if (width < 320) {
      width = 320;
      height = Math.floor(maximumDisplayArea / width / 4) * 4;
    }
    if (height < 200) {
      height = 200;
      width = Math.floor(maximumDisplayArea / height / 4) * 4;
    }
  }
  // Keep each backing dimension divisible by four so the corresponding 2x
  // logical mode has even dimensions and remains selectable by WindowServer.
  width = Math.floor(width / 4) * 4;
  height = Math.floor(height / 4) * 4;
  return { width: Math.max(320, width), height: Math.max(200, height) };
}

function formatBitrate(bitrate: number): string {
  if (bitrate <= 0) return "0 kbps";
  if (bitrate < 1_000_000) {
    const kilobits = bitrate / 1_000;
    return `${kilobits < 1 ? "<1" : Math.round(kilobits)} kbps`;
  }
  return `${(bitrate / 1_000_000).toFixed(1)} Mbps`;
}

type GlassSelectOption = { value: number; label: string };

function GlassSelect({ label, value, options, onChange }: {
  label: string;
  value: number;
  options: GlassSelectOption[];
  onChange: (value: number) => void;
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const id = useId();
  const selected = options.find((option) => option.value === value) ?? options[0];

  useEffect(() => {
    if (!open) return;
    rootRef.current?.querySelector<HTMLButtonElement>('.glass-select-option[aria-selected="true"]')?.focus();
    const closeOutside = (event: PointerEvent | TouchEvent) => {
      if (event.target instanceof Node && !rootRef.current?.contains(event.target)) setOpen(false);
    };
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      setOpen(false);
      rootRef.current?.querySelector<HTMLButtonElement>(".glass-select-trigger")?.focus();
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("touchstart", closeOutside, { passive: true });
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("touchstart", closeOutside);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [open]);

  const moveOptionFocus = (event: ReactKeyboardEvent<HTMLButtonElement>) => {
    const optionButtons = event.currentTarget.parentElement?.querySelectorAll<HTMLButtonElement>(".glass-select-option");
    if (!optionButtons?.length) return;
    const current = Array.from(optionButtons).indexOf(event.currentTarget);
    const next = event.key === "ArrowDown" ? Math.min(optionButtons.length - 1, current + 1)
      : event.key === "ArrowUp" ? Math.max(0, current - 1)
        : event.key === "Home" ? 0
          : event.key === "End" ? optionButtons.length - 1 : -1;
    if (next >= 0) {
      event.preventDefault();
      optionButtons[next].focus();
    }
  };

  if (usesNativeIOSSelect) return <div className="glass-select native-ios-select">
    <select aria-label={label} value={value} onChange={(event) => onChange(Number(event.currentTarget.value))}>
      {options.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}
    </select>
    <svg viewBox="0 0 12 8" aria-hidden="true"><path d="m1 1 5 5 5-5" /></svg>
  </div>;

  return <div className={`glass-select${open ? " open" : ""}`} ref={rootRef} onBlur={(event) => {
    const nextTarget = event.relatedTarget;
    if (nextTarget instanceof Node && !event.currentTarget.contains(nextTarget)) setOpen(false);
  }}>
    <button type="button" className="glass-select-trigger" aria-label={label} aria-haspopup="listbox" aria-expanded={open} aria-controls={`${id}-options`} onClick={() => setOpen((current) => !current)} onKeyDown={(event) => {
      if (event.key === "ArrowDown" || event.key === "ArrowUp") {
        event.preventDefault();
        setOpen(true);
      }
    }}>
      <span>{selected?.label}</span>
      <svg viewBox="0 0 12 8" aria-hidden="true"><path d="m1 1 5 5 5-5" /></svg>
    </button>
    {open && <div className="glass-select-options" id={`${id}-options`} role="listbox" aria-label={`${label}选项`}>
      {options.map((option) => <button key={option.value} type="button" className="glass-select-option" role="option" aria-selected={option.value === value} onKeyDown={moveOptionFocus} onClick={() => {
        onChange(option.value);
        setOpen(false);
        rootRef.current?.querySelector<HTMLButtonElement>(".glass-select-trigger")?.focus();
      }}>
        <span>{option.label}</span>{option.value === value && <span className="glass-select-check" aria-hidden="true">✓</span>}
      </button>)}
    </div>}
  </div>;
}

const KEY_CODES: Record<string, number> = {
  KeyA: 0, KeyS: 1, KeyD: 2, KeyF: 3, KeyH: 4, KeyG: 5, KeyZ: 6, KeyX: 7,
  KeyC: 8, KeyV: 9, KeyB: 11, KeyQ: 12, KeyW: 13, KeyE: 14, KeyR: 15, KeyY: 16,
  KeyT: 17, Digit1: 18, Digit2: 19, Digit3: 20, Digit4: 21, Digit6: 22, Digit5: 23,
  Equal: 24, Digit9: 25, Digit7: 26, Minus: 27, Digit8: 28, Digit0: 29, BracketRight: 30,
  KeyO: 31, BracketLeft: 33, KeyI: 34, KeyP: 35, Enter: 36, KeyL: 37, KeyJ: 38,
  Quote: 39, KeyK: 40, Semicolon: 41, Backslash: 42, Comma: 43, Slash: 44, KeyN: 45,
  KeyM: 46, Period: 47, Tab: 48, Space: 49, Backquote: 50, Backspace: 51, Escape: 53,
  MetaLeft: 55, ShiftLeft: 56, CapsLock: 57, AltLeft: 58, ControlLeft: 59,
  ShiftRight: 60, AltRight: 61, ControlRight: 62, MetaRight: 54, ArrowLeft: 123,
  ArrowRight: 124, ArrowDown: 125, ArrowUp: 126, Delete: 117, Home: 115, End: 119,
  PageUp: 116, PageDown: 121, F1: 122, F2: 120, F3: 99, F4: 118, F5: 96,
  F6: 97, F7: 98, F8: 100, F9: 101, F10: 109, F11: 103, F12: 111,
  NumpadDecimal: 65, NumpadMultiply: 67, NumpadAdd: 69, NumpadClear: 71,
  NumpadDivide: 75, NumpadEnter: 76, NumpadSubtract: 78, NumpadEqual: 81,
  Numpad0: 82, Numpad1: 83, Numpad2: 84, Numpad3: 85, Numpad4: 86,
  Numpad5: 87, Numpad6: 88, Numpad7: 89, Numpad8: 91, Numpad9: 92,
  F13: 105, F14: 107, F15: 113, F16: 106, F17: 64, F18: 79, F19: 80, F20: 90,
  Help: 114, ContextMenu: 110, AudioVolumeUp: 72, AudioVolumeDown: 73, AudioVolumeMute: 74,
  IntlBackslash: 10, IntlYen: 93, IntlRo: 94, NumpadComma: 95, Lang2: 102, Lang1: 104,
};

const SOFT_MODIFIER_CODES = [55, 59, 58, 56] as const;

type SoftKeyboardKey = { label: string; keyCode: number; modifier?: boolean; missionControl?: boolean; icon?: "backspace" | "return" };

const SOFT_PRIMARY_KEYS: SoftKeyboardKey[] = [
  { label: "Esc", keyCode: 53 },
  { label: "Tab", keyCode: 48 },
  { label: "Shift", keyCode: 56, modifier: true },
  { label: "Home", keyCode: 115 },
  { label: "↑", keyCode: 126 },
  { label: "End", keyCode: 119 },
  { label: "PgUp", keyCode: 116 },
  { label: "退格键", keyCode: 51, icon: "backspace" },
  { label: "App", keyCode: 126, missionControl: true },
];

const SOFT_SECONDARY_KEYS: SoftKeyboardKey[] = [
  { label: "⌘", keyCode: 55, modifier: true },
  { label: "Ctrl", keyCode: 59, modifier: true },
  { label: "Alt", keyCode: 58, modifier: true },
  { label: "←", keyCode: 123 },
  { label: "↓", keyCode: 125 },
  { label: "→", keyCode: 124 },
  { label: "PgDn", keyCode: 121 },
  { label: "回车键", keyCode: 36, icon: "return" },
];

function SoftKeyIcon({ kind }: { kind: "backspace" | "return" }) {
  if (kind === "backspace") return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M8 5H20a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H8L2 12z"/><path d="m11 9 6 6m0-6-6 6"/></svg>;
  return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M20 5v5a4 4 0 0 1-4 4H4"/><path d="m8 10-4 4 4 4"/></svg>;
}

type FloatingMenuEdge = "top" | "left" | "right";

type FloatingMenuDrag = {
  pointerId: number;
  startX: number;
  startY: number;
  originLeft: number;
  originCenterY: number;
  moved: boolean;
};

function FloatingControlsToggle({
  expanded,
  onToggle,
}: {
  expanded: boolean;
  onToggle: () => void;
}) {
  const [edge, setEdge] = useState<FloatingMenuEdge>("right");
  const [topPercent, setTopPercent] = useState<number | null>(60);
  const [topCenterPercent, setTopCenterPercent] = useState<number | null>(null);
  const [dragPosition, setDragPosition] = useState<{ left: number; centerY: number } | null>(null);
  const drag = useRef<FloatingMenuDrag | null>(null);
  const suppressClick = useRef(false);
  const pointerDown = (event: ReactPointerEvent<HTMLButtonElement>) => {
    if (event.button !== 0) return;
    const rect = event.currentTarget.getBoundingClientRect();
    drag.current = {
      pointerId: event.pointerId,
      startX: event.clientX,
      startY: event.clientY,
      originLeft: rect.left,
      originCenterY: rect.top + rect.height / 2,
      moved: false,
    };
    event.currentTarget.setPointerCapture(event.pointerId);
  };

  const pointerMove = (event: ReactPointerEvent<HTMLButtonElement>) => {
    const current = drag.current;
    if (!current || current.pointerId !== event.pointerId) return;
    const deltaX = event.clientX - current.startX;
    const deltaY = event.clientY - current.startY;
    if (!current.moved && Math.hypot(deltaX, deltaY) < 4) return;
    current.moved = true;
    const rect = event.currentTarget.getBoundingClientRect();
    const leftEdge = Math.max(0, Math.min(window.innerWidth - rect.width, current.originLeft + deltaX));
    const left = edge === "top" ? leftEdge + rect.width / 2 : leftEdge;
    const centerY = Math.max(rect.height / 2 + 8, Math.min(window.innerHeight - rect.height / 2 - 8, current.originCenterY + deltaY));
    setDragPosition({ left, centerY });
    event.preventDefault();
  };

  const finishPointer = (event: ReactPointerEvent<HTMLButtonElement>) => {
    const current = drag.current;
    if (!current || current.pointerId !== event.pointerId) return;
    if (current.moved) {
      const rect = event.currentTarget.getBoundingClientRect();
      const finalLeft = Math.max(0, Math.min(window.innerWidth - rect.width, current.originLeft + event.clientX - current.startX));
      const centerY = Math.max(rect.height / 2 + 8, Math.min(window.innerHeight - rect.height / 2 - 8, current.originCenterY + event.clientY - current.startY));
      const finalTop = centerY - rect.height / 2;
      const topSnapDistance = Math.max(72, window.innerHeight * 0.18);
      if (finalTop <= topSnapDistance) {
        setEdge("top");
        setTopPercent(null);
        setTopCenterPercent(((finalLeft + rect.width / 2) / Math.max(1, window.innerWidth)) * 100);
      } else {
        const leftDistance = finalLeft;
        const rightDistance = window.innerWidth - finalLeft - rect.width;
        setEdge(leftDistance < rightDistance ? "left" : "right");
        setTopPercent((centerY / Math.max(1, window.innerHeight)) * 100);
        setTopCenterPercent(null);
      }
      suppressClick.current = true;
      window.setTimeout(() => { suppressClick.current = false; }, 250);
    }
    drag.current = null;
    setDragPosition(null);
  };

  return <button
    className={`floating-controls-toggle dock-${edge}${dragPosition ? " dragging" : ""}`}
    style={dragPosition
      ? { left: dragPosition.left, right: "auto", top: dragPosition.centerY }
      : {
        top: topPercent === null ? "env(safe-area-inset-top, 0px)" : `${topPercent}%`,
        ...(edge === "top" && topCenterPercent !== null ? { left: `${topCenterPercent}%`, right: "auto" } : {}),
      }}
    aria-controls="connection-panel"
    aria-expanded={expanded}
    aria-label={expanded ? "关闭控制面板" : "打开控制面板，可拖动移动并吸附到屏幕边缘"}
    onClick={() => { if (suppressClick.current) { suppressClick.current = false; return; } onToggle(); }}
    onPointerDown={pointerDown}
    onPointerMove={pointerMove}
    onPointerUp={finishPointer}
    onPointerCancel={finishPointer}
    onKeyDown={(event) => {
      if (event.key === "ArrowLeft" || event.key === "ArrowRight") {
        event.preventDefault();
        setEdge(event.key === "ArrowLeft" ? "left" : "right");
        setTopCenterPercent(null);
      } else if (event.key === "ArrowUp" || event.key === "ArrowDown") {
        event.preventDefault();
        const rect = event.currentTarget.getBoundingClientRect();
        const currentPercent = ((rect.top + rect.height / 2) / Math.max(1, window.innerHeight)) * 100;
        setTopPercent(Math.max(8, Math.min(92, currentPercent + (event.key === "ArrowUp" ? -4 : 4))));
      }
    }}
  >
  </button>;
}

function App() {
  const videoRef = useRef<HTMLVideoElement>(null);
  const lastRequestedDisplaySize = useRef<{ width: number; height: number } | null>(null);
  const keyboardRef = useRef<HTMLInputElement>(null);
  const keyboardViewportHeight = useRef<number | null>(null);
  const keyboardTogglePointer = useRef(false);
  const keyboardInputValue = useRef("");
  const keyboardComposing = useRef(false);
  const infoModalRef = useRef<HTMLElement | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const peerRef = useRef<RTCPeerConnection | null>(null);
  const inputRef = useRef<RTCDataChannel | null>(null);
  const pointerRef = useRef<RTCDataChannel | null>(null);
  const pendingCandidates = useRef<RTCIceCandidateInit[]>([]);
  const pendingLocalCandidates = useRef<RTCIceCandidateInit[]>([]);
  const offerSent = useRef(false);
  const mapVersion = useRef(0);
  const reconnectTimer = useRef<number | undefined>(undefined);
  const reconnectStableTimer = useRef<number | undefined>(undefined);
  const reconnectAttempts = useRef(0);
  const manualDisconnect = useRef(false);
  const connectRef = useRef<(() => Promise<void>) | null>(null);
  const longPressTimer = useRef<number | undefined>(undefined);
  const longPressPointer = useRef<number | null>(null);
  const longPressOrigin = useRef<{ pointerId: number; x: number; y: number } | null>(null);
  const touchPointers = useRef(new Map<number, { x: number; y: number }>());
  const touchScrollAnchor = useRef<{ x: number; y: number } | null>(null);
  const touchScrollRemainder = useRef({ x: 0, y: 0 });
  const touchScrollActive = useRef(false);
  const trackpadGesture = useRef<{ pointerId: number; startX: number; startY: number; moved: boolean; dragging: boolean } | null>(null);
  const twoFingerGesture = useRef<{ startedAt: number; startX: number; startY: number; moved: boolean } | null>(null);
  const virtualCursorRef = useRef<{ x: number; y: number } | null>(null);
  const lastBytes = useRef(0);
  const lastDecodedFrames = useRef(0);
  const lastJitterBufferDelay = useRef(0);
  const lastJitterBufferFrames = useRef(0);
  const lastDecodeTime = useRef(0);
  const lastStatsAt = useRef(performance.now());
  const [token, setToken] = useState("");
  const [authenticated, setAuthenticated] = useState(false);
  const [sameHostClient, setSameHostClient] = useState(false);
  const [serverVersion, setServerVersion] = useState("dev");
  const [connecting, setConnecting] = useState(false);
  const [connected, setConnected] = useState(false);
  const [videoFrameReady, setVideoFrameReady] = useState(false);
  const [status, setStatus] = useState("需要访问口令才能连接到这台 Mac");
  const [error, setError] = useState("");
  const [stats, setStats] = useState<ConnectionStats>({});
  const [quality, setQuality] = useState(5_000_000);
  const [fpsCap, setFpsCap] = useState(30);
  const [resolutionScale, setResolutionScale] = useState(100);
  const [showKeyboard, setShowKeyboard] = useState(false);
  const [viewportSize, setViewportSize] = useState(() => getViewportMetrics());
  const [showSoftTextEntry, setShowSoftTextEntry] = useState(false);
  const [showInfoModal, setShowInfoModal] = useState(false);
  const [isFullscreen, setIsFullscreen] = useState(() => Boolean(document.fullscreenElement));
  const [floatingMetric, setFloatingMetric] = useState<FloatingMetric | null>(null);
  const [activeSoftModifiers, setActiveSoftModifiers] = useState<number[]>([]);
  const softModifiersRef = useRef(new Set<number>());
  const suppressedRemoteKeyUps = useRef(new Set<string>());
  const softChordInFlight = useRef(false);
  const softChordGeneration = useRef(0);
  const softKeyboardPointerActive = useRef(false);
  const softKeyboardPointerTimer = useRef<number | undefined>(undefined);
  const backspaceRepeatDelay = useRef<number | undefined>(undefined);
  const backspaceRepeatInterval = useRef<number | undefined>(undefined);
  const backspaceClickReset = useRef<number | undefined>(undefined);
  const backspaceLongPressed = useRef(false);
  const suppressBackspaceClick = useRef(false);
  const [pointerMode, setPointerMode] = useState<PointerMode>("mouse");

  useEffect(() => () => {
    if (backspaceRepeatDelay.current !== undefined) window.clearTimeout(backspaceRepeatDelay.current);
    if (backspaceRepeatInterval.current !== undefined) window.clearInterval(backspaceRepeatInterval.current);
    if (backspaceClickReset.current !== undefined) window.clearTimeout(backspaceClickReset.current);
  }, []);

  useEffect(() => {
    if (!showInfoModal) return;
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") setShowInfoModal(false);
    };
    window.addEventListener("keydown", closeOnEscape);
    return () => window.removeEventListener("keydown", closeOnEscape);
  }, [showInfoModal]);

  useEffect(() => {
    const fullscreenDocument = document as Document & { webkitFullscreenElement?: Element | null };
    const syncFullscreen = () => setIsFullscreen(Boolean(document.fullscreenElement || fullscreenDocument.webkitFullscreenElement));
    document.addEventListener("fullscreenchange", syncFullscreen);
    document.addEventListener("webkitfullscreenchange", syncFullscreen);
    syncFullscreen();
    return () => {
      document.removeEventListener("fullscreenchange", syncFullscreen);
      document.removeEventListener("webkitfullscreenchange", syncFullscreen);
    };
  }, []);

  useEffect(() => {
    let frame = 0;
    const visualViewport = window.visualViewport;
    const syncKeyboardShift = () => {
      const baseHeight = keyboardViewportHeight.current;
      const shift = baseHeight === null ? 0 : Math.max(0, baseHeight - (visualViewport?.height ?? window.innerHeight));
      if (shift < 32) keyboardViewportHeight.current = null;
      document.documentElement.style.setProperty("--keyboard-shift", `${shift < 32 ? 0 : shift}px`);
    };
    const syncViewportSize = () => {
      syncKeyboardShift();
      window.cancelAnimationFrame(frame);
      frame = window.requestAnimationFrame(() => {
        setViewportSize(getViewportMetrics());
        syncKeyboardShift();
      });
    };
    const orientation = window.screen.orientation;
    const stage = videoRef.current?.parentElement;
    const stageObserver = stage && typeof ResizeObserver !== "undefined"
      ? new ResizeObserver(syncViewportSize)
      : undefined;
    if (stage) stageObserver?.observe(stage);
    window.addEventListener("resize", syncViewportSize);
    window.addEventListener("orientationchange", syncViewportSize);
    orientation?.addEventListener("change", syncViewportSize);
    visualViewport?.addEventListener("resize", syncViewportSize);
    visualViewport?.addEventListener("scroll", syncViewportSize);
    return () => {
      window.cancelAnimationFrame(frame);
      stageObserver?.disconnect();
      window.removeEventListener("resize", syncViewportSize);
      window.removeEventListener("orientationchange", syncViewportSize);
      orientation?.removeEventListener("change", syncViewportSize);
      visualViewport?.removeEventListener("resize", syncViewportSize);
      visualViewport?.removeEventListener("scroll", syncViewportSize);
      document.documentElement.style.setProperty("--keyboard-shift", "0px");
    };
  }, [authenticated]);

  useEffect(() => {
    if (!showInfoModal) return;
    const panel = infoModalRef.current;
    const backdrop = panel?.parentElement;
    if (!panel || !backdrop) return;

    let touchStartedInPanel = false;
    let lastTouchY: number | null = null;
    const onTouchStart = (event: TouchEvent) => {
      if (event.touches.length !== 1) {
        touchStartedInPanel = false;
        lastTouchY = null;
        return;
      }
      touchStartedInPanel = event.target instanceof Node && panel.contains(event.target);
      lastTouchY = event.touches[0].clientY;
    };
    const onTouchMove = (event: TouchEvent) => {
      if (event.touches.length !== 1) {
        event.preventDefault();
        return;
      }
      const currentY = event.touches[0].clientY;
      const deltaY = lastTouchY === null ? 0 : currentY - lastTouchY;
      lastTouchY = currentY;
      if (!touchStartedInPanel) {
        event.preventDefault();
        return;
      }
      const canScroll = panel.scrollHeight > panel.clientHeight;
      const atTop = panel.scrollTop <= 0;
      const atBottom = panel.scrollTop + panel.clientHeight >= panel.scrollHeight - 1;
      if (!canScroll || (atTop && deltaY > 0) || (atBottom && deltaY < 0)) event.preventDefault();
    };
    const finishTouch = () => {
      touchStartedInPanel = false;
      lastTouchY = null;
    };

    backdrop.addEventListener("touchstart", onTouchStart, { passive: true });
    backdrop.addEventListener("touchmove", onTouchMove, { passive: false });
    backdrop.addEventListener("touchend", finishTouch, { passive: true });
    backdrop.addEventListener("touchcancel", finishTouch, { passive: true });
    return () => {
      backdrop.removeEventListener("touchstart", onTouchStart);
      backdrop.removeEventListener("touchmove", onTouchMove);
      backdrop.removeEventListener("touchend", finishTouch);
      backdrop.removeEventListener("touchcancel", finishTouch);
    };
  }, [showInfoModal]);

  useEffect(() => {
    const preventSafariGestureZoom = (event: Event) => event.preventDefault();
    for (const name of ["gesturestart", "gesturechange", "gestureend"]) {
      document.addEventListener(name, preventSafariGestureZoom, { capture: true, passive: false });
    }
    return () => {
      for (const name of ["gesturestart", "gesturechange", "gestureend"]) {
        document.removeEventListener(name, preventSafariGestureZoom, true);
      }
    };
  }, []);

  const sendSignal = useCallback((message: Signal) => {
    const ws = wsRef.current;
    if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify(message));
  }, []);

  const sendInput = useCallback((payload: Record<string, unknown>, pointer = false) => {
    const channel = pointer ? pointerRef.current : inputRef.current;
    if (channel?.readyState !== "open") return;
    if (pointer && channel.bufferedAmount > 64_000) return;
    channel.send(JSON.stringify(payload));
  }, []);

  const releaseSoftModifiers = useCallback(() => {
    if (softChordInFlight.current) softChordGeneration.current += 1;
    for (const keyCode of softModifiersRef.current) {
      sendInput({ type: "key", keyCode, down: false, mapVersion: mapVersion.current });
    }
    softModifiersRef.current.clear();
    setActiveSoftModifiers([]);
  }, [sendInput]);

  const openMissionControl = useCallback(() => {
    releaseSoftModifiers();
    sendInput({ type: "missionControl", mapVersion: mapVersion.current });
  }, [releaseSoftModifiers, sendInput]);

  const toggleSoftModifier = useCallback((keyCode: number) => {
    if (softChordInFlight.current) return;
    const modifiers = softModifiersRef.current;
    if (modifiers.has(keyCode)) {
      modifiers.delete(keyCode);
      sendInput({ type: "key", keyCode, down: false, mapVersion: mapVersion.current });
    } else {
      modifiers.add(keyCode);
      sendInput({ type: "key", keyCode, down: true, mapVersion: mapVersion.current });
    }
    setActiveSoftModifiers([...modifiers]);
  }, [sendInput]);

  const sendSoftKey = useCallback((keyCode: number) => {
    if (softChordInFlight.current) return;
    if (keyCode === 126 && (softModifiersRef.current.has(59) || softModifiersRef.current.has(62))) {
      openMissionControl();
      return;
    }
    sendInput({ type: "key", keyCode, down: true, mapVersion: mapVersion.current });
    sendInput({ type: "key", keyCode, down: false, mapVersion: mapVersion.current });
    if (softModifiersRef.current.size) releaseSoftModifiers();
  }, [openMissionControl, releaseSoftModifiers, sendInput]);

  const startBackspaceRepeat = (event: ReactPointerEvent<HTMLButtonElement>) => {
    if (event.button !== 0 && event.pointerType !== "touch") return;
    event.currentTarget.setPointerCapture(event.pointerId);
    if (backspaceClickReset.current !== undefined) window.clearTimeout(backspaceClickReset.current);
    backspaceLongPressed.current = false;
    suppressBackspaceClick.current = false;
    backspaceRepeatDelay.current = window.setTimeout(() => {
      backspaceLongPressed.current = true;
      suppressBackspaceClick.current = true;
      sendSoftKey(51);
      backspaceRepeatInterval.current = window.setInterval(() => sendSoftKey(51), 75);
    }, 380);
  };

  const stopBackspaceRepeat = () => {
    if (backspaceRepeatDelay.current !== undefined) {
      window.clearTimeout(backspaceRepeatDelay.current);
      backspaceRepeatDelay.current = undefined;
    }
    if (backspaceRepeatInterval.current !== undefined) {
      window.clearInterval(backspaceRepeatInterval.current);
      backspaceRepeatInterval.current = undefined;
    }
    if (backspaceLongPressed.current) {
      backspaceClickReset.current = window.setTimeout(() => {
        suppressBackspaceClick.current = false;
        backspaceLongPressed.current = false;
      }, 500);
    }
  };

  const clickBackspace = () => {
    if (suppressBackspaceClick.current) {
      suppressBackspaceClick.current = false;
      backspaceLongPressed.current = false;
      if (backspaceClickReset.current !== undefined) {
        window.clearTimeout(backspaceClickReset.current);
        backspaceClickReset.current = undefined;
      }
      return;
    }
    sendSoftKey(51);
  };

  const sendSoftChord = useCallback(async (modifierKeyCode: number, keyCode: number) => {
    if (softChordInFlight.current || inputRef.current?.readyState !== "open") return;
    softChordInFlight.current = true;
    setActiveSoftModifiers([]);
    const generation = ++softChordGeneration.current;
    let modifierDown = softModifiersRef.current.has(modifierKeyCode);
    let keyDown = false;

    const wait = (duration: number) => new Promise<void>((resolve) => window.setTimeout(resolve, duration));

    try {
      for (const heldKeyCode of softModifiersRef.current) {
        if (heldKeyCode !== modifierKeyCode) {
          sendInput({ type: "key", keyCode: heldKeyCode, down: false, mapVersion: mapVersion.current });
          softModifiersRef.current.delete(heldKeyCode);
        }
      }
      if (!modifierDown) {
        sendInput({ type: "key", keyCode: modifierKeyCode, down: true, mapVersion: mapVersion.current });
        softModifiersRef.current.add(modifierKeyCode);
        modifierDown = true;
      }
      await wait(40);
      if (generation !== softChordGeneration.current) return;
      sendInput({ type: "key", keyCode, down: true, mapVersion: mapVersion.current });
      keyDown = true;
      await wait(70);
      if (generation !== softChordGeneration.current) return;
      sendInput({ type: "key", keyCode, down: false, mapVersion: mapVersion.current });
      keyDown = false;
      await wait(30);
      if (generation !== softChordGeneration.current) return;
      sendInput({ type: "key", keyCode: modifierKeyCode, down: false, mapVersion: mapVersion.current });
      modifierDown = false;
    } finally {
      if (keyDown) sendInput({ type: "key", keyCode, down: false, mapVersion: mapVersion.current });
      if (modifierDown) sendInput({ type: "key", keyCode: modifierKeyCode, down: false, mapVersion: mapVersion.current });
      softModifiersRef.current.clear();
      setActiveSoftModifiers([]);
      softChordInFlight.current = false;
    }
  }, [sendInput]);

  const ensureVirtualCursor = useCallback(() => {
    const cursor = virtualCursorRef.current ?? { x: 0.5, y: 0.5 };
    virtualCursorRef.current = cursor;
    return cursor;
  }, []);

  const moveVirtualCursor = useCallback((deltaX: number, deltaY: number, button = -1) => {
    const video = videoRef.current;
    if (!video) return;
    const plane = videoPointerPlane(video);
    const current = virtualCursorRef.current ?? { x: 0.5, y: 0.5 };
    const next = {
      x: Math.max(0, Math.min(1, current.x + deltaX * 0.95 / Math.max(1, plane.drawWidth))),
      y: Math.max(0, Math.min(1, current.y + deltaY * 0.95 / Math.max(1, plane.drawHeight))),
    };
    virtualCursorRef.current = next;
    sendInput({ type: "pointer", action: "move", x: next.x * plane.width, y: next.y * plane.height,
      button, width: plane.width, height: plane.height, mapVersion: mapVersion.current }, true);
  }, [sendInput]);

  const sendTrackpadPointer = useCallback((action: string, button: number) => {
    const video = videoRef.current;
    const cursor = virtualCursorRef.current;
    if (!video || !cursor) return;
    const plane = videoPointerPlane(video);
    sendInput({ type: "pointer", action, x: cursor.x * plane.width, y: cursor.y * plane.height,
      button, width: plane.width, height: plane.height, mapVersion: mapVersion.current }, action === "move");
  }, [sendInput]);

  const resetPointerGestures = useCallback(() => {
    window.clearTimeout(longPressTimer.current);
    longPressTimer.current = undefined;
    longPressPointer.current = null;
    longPressOrigin.current = null;
    touchPointers.current.clear();
    touchScrollAnchor.current = null;
    touchScrollRemainder.current = { x: 0, y: 0 };
    touchScrollActive.current = false;
    trackpadGesture.current = null;
    twoFingerGesture.current = null;
  }, []);

  const changePointerMode = useCallback((mode: PointerMode) => {
    if (mode === pointerMode) return;
    sendInput({ type: "releaseAll" });
    resetPointerGestures();
    const cursor = mode === "mouse" ? { x: 0.5, y: 0.5 } : null;
    virtualCursorRef.current = cursor;
    setPointerMode(mode);
  }, [pointerMode, resetPointerGestures, sendInput]);

  const resumePlayback = useCallback(async () => {
    const video = videoRef.current;
    if (!video?.srcObject || !video.paused) return;
    try {
      await video.play();
      setStatus("已连接");
    } catch {
      setStatus("点击画面开始播放");
    }
  }, []);

  const connect = useCallback(async () => {
    if (!authenticated || connected || wsRef.current) return;
    releaseSoftModifiers();
    resetPointerGestures();
    manualDisconnect.current = false;
    setConnecting(true);
    setVideoFrameReady(false);
    setError("");
    setStatus("正在连接…");
    const scheme = location.protocol === "https:" ? "wss:" : "ws:";
    let serverMessageError = "";
    const ws = new WebSocket(`${scheme}//${location.host}/api/rtc`);
    wsRef.current = ws;
    lastBytes.current = 0;
    lastDecodedFrames.current = 0;
    lastJitterBufferDelay.current = 0;
    lastJitterBufferFrames.current = 0;
    lastDecodeTime.current = 0;
    lastStatsAt.current = performance.now();
    offerSent.current = false;
    mapVersion.current = 0;
    setStats({});
    pendingCandidates.current = [];
    pendingLocalCandidates.current = [];
    ws.onopen = async () => {
      if (wsRef.current !== ws || manualDisconnect.current) { ws.close(); return; }
      try {
        const pc = new RTCPeerConnection({ bundlePolicy: "max-bundle" });
        peerRef.current = pc;
        const videoReceiver = pc.addTransceiver("video", { direction: "recvonly" }).receiver;
        if (/Android/i.test(navigator.userAgent)) {
          try {
            if (Reflect.has(videoReceiver, "jitterBufferTarget")) Reflect.set(videoReceiver, "jitterBufferTarget", 50);
            else if (Reflect.has(videoReceiver, "playoutDelayHint")) Reflect.set(videoReceiver, "playoutDelayHint", 0);
          } catch { /* The browser may expose but reject the hint. */ }
        }
        const input = pc.createDataChannel("input", { ordered: true });
        const pointer = pc.createDataChannel("pointer", { ordered: false, maxRetransmits: 0 });
        inputRef.current = input;
        pointerRef.current = pointer;
        input.onopen = () => setStatus("已连接");
        pc.ontrack = (event) => {
          if (videoRef.current) {
            setVideoFrameReady(false);
            videoRef.current.srcObject = event.streams[0] ?? new MediaStream([event.track]);
            void resumePlayback();
          }
        };
        pc.onconnectionstatechange = () => {
          if (peerRef.current !== pc) return;
          const state = pc.connectionState;
          setConnected(state === "connected");
          if (state !== "connected") setVideoFrameReady(false);
          if (state === "connected") {
            setConnecting(false);
            window.clearTimeout(reconnectStableTimer.current);
            reconnectStableTimer.current = window.setTimeout(() => {
              reconnectAttempts.current = 0;
              reconnectStableTimer.current = undefined;
            }, 30_000);
            window.clearTimeout(reconnectTimer.current);
            reconnectTimer.current = undefined;
            setStatus("已连接");
          } else if (state === "failed") {
            releaseSoftModifiers();
            sendInput({ type: "releaseAll" });
            resetPointerGestures();
            window.clearTimeout(reconnectStableTimer.current);
            reconnectStableTimer.current = undefined;
            setConnecting(false);
            setStatus("连接中断，可重新连接");
            sendSignal({ type: "close" });
            ws.close();
          } else if (state === "disconnected") {
            releaseSoftModifiers();
            sendInput({ type: "releaseAll" });
            resetPointerGestures();
            window.clearTimeout(reconnectStableTimer.current);
            reconnectStableTimer.current = undefined;
            setStatus("网络暂时中断，正在恢复…");
            window.clearTimeout(reconnectTimer.current);
            reconnectTimer.current = window.setTimeout(() => {
              if (peerRef.current === pc && pc.connectionState === "disconnected") ws.close();
            }, 5000);
          }
        };
        pc.onicecandidate = (event) => {
          if (!event.candidate) return;
          const candidate = event.candidate.toJSON();
          if (offerSent.current) sendSignal({ type: "candidate", ...candidate });
          else pendingLocalCandidates.current.push(candidate);
        };
        const [offer, decodeLimits] = await Promise.all([pc.createOffer(), detectH264DecodeLimits()]);
        const originalSDP = offer.sdp ?? "";
        const advertisedSDP = decodeLimits ? advertiseH264DecodeLimits(originalSDP, decodeLimits) : originalSDP;
        if (advertisedSDP && advertisedSDP !== originalSDP) {
          try {
            await pc.setLocalDescription({ type: offer.type, sdp: advertisedSDP });
          } catch {
            // Keep the browser's original offer if it rejects SDP capability extensions.
            await pc.setLocalDescription(offer);
          }
        } else {
          await pc.setLocalDescription(offer);
        }
        const initialDisplay = sameHostClient
          ? { width: 1920, height: 1080 }
          : displayPixelsFor(videoRef.current?.parentElement ?? null, resolutionScale);
        lastRequestedDisplaySize.current = initialDisplay;
        sendSignal({ type: "offer", sdp: pc.localDescription?.sdp ?? originalSDP, ...initialDisplay, bitrate: quality, fps: fpsCap });
        offerSent.current = true;
        for (const candidate of pendingLocalCandidates.current.splice(0)) sendSignal({ type: "candidate", ...candidate });
        setStatus("正在连接桌面…");
      } catch {
        setConnecting(false);
        setError("无法创建远程连接");
        ws.close();
      }
    };
    ws.onmessage = async (event) => {
      if (wsRef.current !== ws) return;
      let message: Signal;
      try { message = JSON.parse(event.data) as Signal; } catch { return; }
      const pc = peerRef.current;
      if (message.type === "answer" && message.sdp && pc) {
        try {
          await pc.setRemoteDescription({ type: "answer", sdp: message.sdp });
          if (peerRef.current !== pc) return;
          for (const candidate of pendingCandidates.current.splice(0)) await pc.addIceCandidate(candidate);
        }
        catch { setError("连接协商失败"); }
      } else if (message.type === "candidate" && message.candidate && pc) {
        const candidate = { candidate: message.candidate, sdpMid: message.sdpMid, sdpMLineIndex: message.sdpMLineIndex };
        if (pc.remoteDescription) {
          try { await pc.addIceCandidate(candidate); }
          catch { setError("网络连接失败"); }
        } else pendingCandidates.current.push(candidate);
      } else if (message.type === "error") {
        const reason = message.error ?? "远程桌面连接失败";
        serverMessageError = reason;
        const serviceRestarting = reason === "RemoteMAC 正在关闭";
        if (message.retryable) {
          setError("");
          setConnecting(false);
          setStatus("上一会话仍在回收，稍后自动重连…");
          ws.close();
        } else if (serviceRestarting) {
          setError("");
          setConnecting(false);
          setStatus("Mac 服务正在重启，准备自动重连…");
          ws.close();
        } else if (message.fatal) {
          setError(reason);
          setConnecting(false);
          manualDisconnect.current = true;
          ws.close();
        } else {
          setError("");
          setStatus(`操作未完成，连接保持：${reason}`);
        }
      } else if (message.type === "state") {
        setStatus(message.state ?? "连接状态已更新");
      } else if (message.type === "quality") {
        if (message.state) setStatus(message.state);
        setStats((current) => ({ ...current,
          displayWidth: message.displayWidth ?? message.width ?? current.displayWidth,
          displayHeight: message.displayHeight ?? message.height ?? current.displayHeight,
        }));
      } else if (message.type === "mapping") {
        mapVersion.current = message.mapVersion ?? mapVersion.current;
        setStats((current) => ({ ...current,
          displayWidth: message.displayWidth ?? message.width,
          displayHeight: message.displayHeight ?? message.height }));
      }
    };
    ws.onerror = () => {
      setConnecting(false);
      setError(serverMessageError || "连接失败，请检查地址和网络");
    };
    ws.onclose = () => {
      if (wsRef.current !== ws) return;
      releaseSoftModifiers();
      resetPointerGestures();
      window.clearTimeout(reconnectStableTimer.current);
      reconnectStableTimer.current = undefined;
      wsRef.current = null;
      setConnecting(false);
      setConnected(false);
      setVideoFrameReady(false);
      peerRef.current?.close();
      peerRef.current = null;
      setStats({});
      pendingCandidates.current = [];
      pendingLocalCandidates.current = [];
      inputRef.current = null;
      pointerRef.current = null;
      if (videoRef.current) videoRef.current.srcObject = null;
      if (!manualDisconnect.current && authenticated) {
        const attempt = Math.min(reconnectAttempts.current, 5);
        const delay = Math.min(30_000, 1_000 * 2 ** attempt);
        reconnectAttempts.current = Math.min(attempt + 1, 5);
        setConnecting(true);
        setStatus(`连接中断，${Math.ceil(delay / 1000)} 秒后自动重连…`);
        reconnectTimer.current = window.setTimeout(() => { void connectRef.current?.(); }, delay);
      }
    };
  }, [authenticated, connected, sameHostClient, quality, fpsCap, resolutionScale, resumePlayback, releaseSoftModifiers, resetPointerGestures, sendInput, sendSignal]);
  connectRef.current = connect;

  const disconnect = useCallback(() => {
    manualDisconnect.current = true;
    releaseSoftModifiers();
    resetPointerGestures();
    window.clearTimeout(reconnectTimer.current);
    reconnectTimer.current = undefined;
    window.clearTimeout(reconnectStableTimer.current);
    reconnectStableTimer.current = undefined;
    sendInput({ type: "releaseAll" });
    sendSignal({ type: "close" });
    peerRef.current?.close();
    wsRef.current?.close();
    peerRef.current = null;
    wsRef.current = null;
    inputRef.current = null;
    pointerRef.current = null;
    if (videoRef.current) videoRef.current.srcObject = null;
    setStats({});
    setConnected(false);
    setConnecting(false);
    setVideoFrameReady(false);
    setStatus("连接已断开");
  }, [releaseSoftModifiers, resetPointerGestures, sendInput, sendSignal]);

  const toggleFullscreen = async () => {
    const fullscreenDocument = document as Document & {
      webkitFullscreenElement?: Element | null;
      webkitExitFullscreen?: () => Promise<void> | void;
    };
    const fullscreenRoot = document.documentElement as HTMLElement & {
      webkitRequestFullscreen?: () => Promise<void> | void;
    };
    try {
      if (document.fullscreenElement || fullscreenDocument.webkitFullscreenElement) {
        if (document.exitFullscreen) await document.exitFullscreen();
        else if (fullscreenDocument.webkitExitFullscreen) await fullscreenDocument.webkitExitFullscreen();
        else setStatus("当前浏览器不支持退出全屏");
      } else if (fullscreenRoot.requestFullscreen) {
        await fullscreenRoot.requestFullscreen();
        setShowInfoModal(false);
      } else if (fullscreenRoot.webkitRequestFullscreen) {
        await fullscreenRoot.webkitRequestFullscreen();
        setShowInfoModal(false);
      } else {
        setStatus("当前浏览器不支持全屏");
      }
    } catch {
      setStatus("无法切换全屏模式");
    }
  };

  const login = useCallback(async (event: React.FormEvent) => {
    event.preventDefault();
    setError("");
    try {
      const response = await fetch("/api/login", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token }),
      });
      if (!response.ok) throw new Error("访问口令错误");
        try {
          const capabilities = await fetch("/api/capabilities");
          if (capabilities.ok) {
            const body = await capabilities.json() as Record<string, unknown>;
            setSameHostClient(body.sameHostClient === true);
            if (typeof body.version === "string" && body.version) setServerVersion(body.version);
          }
        } catch {}
      setAuthenticated(true);
      setToken("");
      setStatus(authorizedStatus);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "登录失败");
    }
  }, [token]);

  useEffect(() => {
    void fetch("/api/capabilities").then(async (response) => {
      if (!response.ok || !response.headers.get("content-type")?.includes("application/json")) return;
      const capabilities = await response.json() as Record<string, unknown>;
      if (capabilities.codec === "H264") {
        setAuthenticated(true);
        setStatus(authorizedStatus);
      }
      setSameHostClient(capabilities.sameHostClient === true);
      if (typeof capabilities.version === "string" && capabilities.version) setServerVersion(capabilities.version);
    }).catch(() => undefined);
    return () => {
      manualDisconnect.current = true;
      window.clearTimeout(reconnectTimer.current);
      window.clearTimeout(reconnectStableTimer.current);
      peerRef.current?.close();
      wsRef.current?.close();
      releaseSoftModifiers();
      resetPointerGestures();
    };
  }, [releaseSoftModifiers, resetPointerGestures]);

  useEffect(() => {
    if (!connected || sameHostClient) return;
    const timer = window.setTimeout(() => {
      const stage = videoRef.current?.parentElement;
      if (!stage) return;
      const { width, height } = displayPixelsFor(stage, resolutionScale);
      const previous = lastRequestedDisplaySize.current;
      if (previous && (previous.width >= previous.height) === (width >= height) &&
          Math.abs(width - previous.width) <= 64 && Math.abs(height - previous.height) <= 64) return;
      lastRequestedDisplaySize.current = { width, height };
      sendSignal({ type: "resize", width, height });
    }, 500);
    return () => window.clearTimeout(timer);
  }, [connected, sameHostClient, sendSignal, viewportSize.width, viewportSize.height, resolutionScale]);

  useEffect(() => {
    if (connected) sendSignal({ type: "quality", bitrate: quality, fps: fpsCap });
  }, [connected, quality, fpsCap, sendSignal]);

  useEffect(() => {
    if (!connected) return;
    const poll = window.setInterval(async () => {
      const pc = peerRef.current;
      if (!pc) return;
      let report: RTCStatsReport;
      try {
        report = await pc.getStats();
      } catch {
        setStats((previous) => ({ ...previous, bitrate: undefined, decodeFPS: undefined, rtt: undefined,
          jitterBufferMs: undefined, decodeMs: undefined }));
        return;
      }
      let inbound: RTCInboundRtpStreamStats | undefined;
      let nominatedPair: RTCIceCandidatePairStats | undefined;
      for (const item of report.values()) {
        if (item.type === "inbound-rtp" && (item as RTCInboundRtpStreamStats).kind === "video") {
          inbound = item as RTCInboundRtpStreamStats;
        } else if (item.type === "candidate-pair" && item.state === "succeeded" && item.nominated) {
          nominatedPair = item as RTCIceCandidatePairStats;
        }
      }
      if (!inbound) {
        setStats((previous) => ({ ...previous, bitrate: undefined, decodeFPS: undefined, rtt: undefined,
          jitterBufferMs: undefined, decodeMs: undefined }));
        return;
      }
      const transport = inbound.transportId ? report.get(inbound.transportId) as RTCTransportStats | undefined : undefined;
      const candidatePair = (transport?.selectedCandidatePairId
        ? report.get(transport.selectedCandidatePairId) as RTCIceCandidatePairStats | undefined
        : undefined) ?? nominatedPair;
      const now = performance.now();
      const elapsedMs = Math.max(1, now - lastStatsAt.current);
      const bytesReceived = inbound.bytesReceived ?? 0;
      const framesDecoded = inbound.framesDecoded ?? 0;
      const bitrate = Math.max(0, (bytesReceived - lastBytes.current) * 8_000 / elapsedMs);
      const observedDecodeFPS = Math.max(0, (framesDecoded - lastDecodedFrames.current) * 1_000 / elapsedMs);
      const jitterBufferDelay = inbound.jitterBufferDelay ?? lastJitterBufferDelay.current;
      const jitterBufferFrames = inbound.jitterBufferEmittedCount ?? lastJitterBufferFrames.current;
      const jitterBufferMs = jitterBufferFrames > lastJitterBufferFrames.current
        ? Math.max(0, (jitterBufferDelay - lastJitterBufferDelay.current) * 1_000 /
          (jitterBufferFrames - lastJitterBufferFrames.current))
        : undefined;
      const decodeTime = inbound.totalDecodeTime ?? lastDecodeTime.current;
      const decodedFrames = framesDecoded - lastDecodedFrames.current;
      const decodeMs = decodedFrames > 0 && inbound.totalDecodeTime !== undefined
        ? Math.max(0, (decodeTime - lastDecodeTime.current) * 1_000 / decodedFrames)
        : undefined;
      const decodeFPS = typeof inbound.framesPerSecond === "number" && inbound.framesPerSecond > 0
        ? inbound.framesPerSecond
        : observedDecodeFPS;
      lastBytes.current = bytesReceived;
      lastDecodedFrames.current = framesDecoded;
      lastJitterBufferDelay.current = jitterBufferDelay;
      lastJitterBufferFrames.current = jitterBufferFrames;
      lastDecodeTime.current = decodeTime;
      lastStatsAt.current = now;
      setStats((previous) => ({
        ...previous,
        bitrate,
        decodeFPS,
        jitterBufferMs: jitterBufferMs ?? previous.jitterBufferMs,
        decodeMs: decodeMs ?? previous.decodeMs,
        rtt: candidatePair?.currentRoundTripTime ? candidatePair.currentRoundTripTime * 1_000 : previous.rtt,
      }));
    }, 1_000);
    return () => window.clearInterval(poll);
  }, [connected]);

  const pointerEvent = (event: React.PointerEvent<HTMLVideoElement>, action: string, button = event.button) => {
    sendInput(pointerPayload(event, action, button), action === "move");
  };

  const pointerPayload = (event: React.PointerEvent<HTMLVideoElement>, action: string, button: number): Record<string, unknown> => {
    const video = event.currentTarget;
    const plane = videoPointerPlane(video);
    const localX = event.clientX - plane.left;
    const localY = event.clientY - plane.top;
    const x = Math.max(0, Math.min(plane.width, localX * plane.width / Math.max(1, plane.drawWidth)));
    const y = Math.max(0, Math.min(plane.height, localY * plane.height / Math.max(1, plane.drawHeight)));
    return { type: "pointer", action, x, y, button, width: plane.width, height: plane.height, mapVersion: mapVersion.current };
  };

  const touchCentroid = () => {
    const points = [...touchPointers.current.values()];
    if (!points.length) return null;
    return points.reduce((center, point) => ({ x: center.x + point.x / points.length, y: center.y + point.y / points.length }), { x: 0, y: 0 });
  };

  const pointerMove = (event: React.PointerEvent<HTMLVideoElement>) => {
    if (event.pointerType !== "touch") {
      virtualCursorRef.current = null;
      const button = longPressPointer.current === event.pointerId ? 2 : event.buttons & 2 ? 2 : event.buttons & 4 ? 1 : event.buttons & 1 ? 0 : -1;
      pointerEvent(event, "move", button);
      return;
    }
    if (!touchPointers.current.has(event.pointerId)) return;
    const previous = touchPointers.current.get(event.pointerId)!;
    touchPointers.current.set(event.pointerId, { x: event.clientX, y: event.clientY });
    if (touchScrollActive.current) {
      const center = touchCentroid();
      if (center && touchPointers.current.size >= 2 && touchScrollAnchor.current) {
        const deltaX = touchScrollAnchor.current.x - center.x;
        const deltaY = touchScrollAnchor.current.y - center.y;
        if (twoFingerGesture.current && Math.hypot(center.x - twoFingerGesture.current.startX, center.y - twoFingerGesture.current.startY) > 4) {
          twoFingerGesture.current.moved = true;
        }
        touchScrollRemainder.current.x += deltaX;
        touchScrollRemainder.current.y += deltaY;
        const rawScrollX = Math.trunc(touchScrollRemainder.current.x);
        const rawScrollY = Math.trunc(touchScrollRemainder.current.y);
        touchScrollRemainder.current.x -= rawScrollX;
        touchScrollRemainder.current.y -= rawScrollY;
        const plane = videoPointerPlane(event.currentTarget);
        if (rawScrollX !== 0 || rawScrollY !== 0) {
          sendInput({ type: "pointer", action: "wheel", x: rawScrollX, y: rawScrollY,
            button: 0, width: plane.width, height: plane.height, mapVersion: mapVersion.current });
        }
        touchScrollAnchor.current = center;
      }
      return;
    }
    if (pointerMode === "mouse") {
      const gesture = trackpadGesture.current;
      if (!gesture || gesture.pointerId !== event.pointerId || touchPointers.current.size !== 1) return;
      const distance = Math.hypot(event.clientX - gesture.startX, event.clientY - gesture.startY);
      if (distance > 8) gesture.moved = true;
      if (distance > 10 && !gesture.dragging) {
        window.clearTimeout(longPressTimer.current);
        longPressTimer.current = undefined;
      }
      const deltaX = event.clientX - previous.x;
      const deltaY = event.clientY - previous.y;
      if (Math.abs(deltaX) + Math.abs(deltaY) > 0) moveVirtualCursor(deltaX, deltaY, gesture.dragging ? 0 : -1);
      return;
    }
    const origin = longPressOrigin.current;
    if (origin?.pointerId === event.pointerId && Math.hypot(event.clientX - origin.x, event.clientY - origin.y) > 10) {
      window.clearTimeout(longPressTimer.current);
      longPressTimer.current = undefined;
      longPressOrigin.current = null;
    }
    if (touchPointers.current.size === 1) {
      pointerEvent(event, "move", longPressPointer.current === event.pointerId ? 2 : 0);
    }
  };

  const keyDown = (event: React.KeyboardEvent<HTMLVideoElement>) => {
    if (event.metaKey && event.code === "KeyV") return;
    if (event.code === "ArrowUp" && event.ctrlKey && !event.metaKey && !event.altKey && !event.shiftKey) {
      event.preventDefault();
      suppressedRemoteKeyUps.current.add(event.code);
      if (!event.repeat) openMissionControl();
      return;
    }
    const keyCode = KEY_CODES[event.code];
    if (keyCode === undefined) return;
    event.preventDefault();
    sendInput({ type: "key", keyCode, down: true, mapVersion: mapVersion.current });
  };

  const keyUp = (event: React.KeyboardEvent<HTMLVideoElement>) => {
    if (suppressedRemoteKeyUps.current.delete(event.code)) {
      event.preventDefault();
      return;
    }
    const keyCode = KEY_CODES[event.code];
    if (keyCode === undefined) return;
    event.preventDefault();
    sendInput({ type: "key", keyCode, down: false, mapVersion: mapVersion.current });
    if (softModifiersRef.current.size && !SOFT_MODIFIER_CODES.includes(keyCode as typeof SOFT_MODIFIER_CODES[number])) releaseSoftModifiers();
  };

  const sendText = (value: string) => {
    if (value) {
      sendInput({ type: "text", text: value, mapVersion: mapVersion.current });
      releaseSoftModifiers();
    }
  };

  const forwardKeyboardValue = (input: HTMLInputElement) => {
    const previous = keyboardInputValue.current;
    const next = input.value;
    let common = 0;
    while (common < previous.length && common < next.length && previous[common] === next[common]) common++;
    for (const _ of previous.slice(common)) sendSoftKey(51);
    sendText(next.slice(common));
    keyboardInputValue.current = next;
    if (next.length > 256) {
      input.value = "";
      keyboardInputValue.current = "";
    }
  };

  const pointerDown = (event: React.PointerEvent<HTMLVideoElement>) => {
    event.currentTarget.focus();
    event.currentTarget.setPointerCapture(event.pointerId);
    if (event.pointerType !== "touch") {
      virtualCursorRef.current = null;
      pointerEvent(event, "down");
      return;
    }
    touchPointers.current.set(event.pointerId, { x: event.clientX, y: event.clientY });
    if (touchPointers.current.size >= 2) {
      window.clearTimeout(longPressTimer.current);
      longPressTimer.current = undefined;
      longPressOrigin.current = null;
      longPressPointer.current = null;
      if (pointerMode === "mouse") {
        window.clearTimeout(longPressTimer.current);
        trackpadGesture.current = null;
        const center = touchCentroid();
        twoFingerGesture.current = { startedAt: performance.now(), startX: center?.x ?? event.clientX,
          startY: center?.y ?? event.clientY, moved: false };
      }
      touchScrollActive.current = true;
      touchScrollAnchor.current = touchCentroid();
      touchScrollRemainder.current = { x: 0, y: 0 };
      sendInput({ type: "releaseAll" });
      return;
    }
    if (pointerMode === "mouse") {
      ensureVirtualCursor();
      const gesture = { pointerId: event.pointerId, startX: event.clientX, startY: event.clientY,
        moved: false, dragging: false };
      trackpadGesture.current = gesture;
      window.clearTimeout(longPressTimer.current);
      longPressTimer.current = window.setTimeout(() => {
        longPressTimer.current = undefined;
        if (touchPointers.current.size !== 1 || !touchPointers.current.has(event.pointerId) || trackpadGesture.current !== gesture || gesture.moved) return;
        gesture.dragging = true;
        sendTrackpadPointer("down", 0);
      }, 550);
      return;
    }
    const leftDown = pointerPayload(event, "down", 0);
    sendInput(leftDown);
    window.clearTimeout(longPressTimer.current);
    longPressPointer.current = null;
    longPressOrigin.current = { pointerId: event.pointerId, x: event.clientX, y: event.clientY };
    longPressTimer.current = window.setTimeout(() => {
      longPressTimer.current = undefined;
      const origin = longPressOrigin.current;
      longPressOrigin.current = null;
      if (touchPointers.current.size !== 1 || !touchPointers.current.has(event.pointerId) || origin?.pointerId !== event.pointerId) return;
      sendInput({ ...leftDown, action: "up" });
      sendInput({ ...leftDown, action: "down", button: 2 });
      longPressPointer.current = event.pointerId;
    }, 550);
  };

  const pointerUp = (event: React.PointerEvent<HTMLVideoElement>) => {
    window.clearTimeout(longPressTimer.current);
    longPressTimer.current = undefined;
    longPressOrigin.current = null;
    if (event.pointerType === "touch") {
      touchPointers.current.delete(event.pointerId);
      if (touchScrollActive.current) {
        sendInput({ type: "releaseAll" });
        if (touchPointers.current.size === 0) {
          const gesture = twoFingerGesture.current;
          if (pointerMode === "mouse" && gesture && !gesture.moved && performance.now() - gesture.startedAt < 500) {
            sendTrackpadPointer("down", 2);
            sendTrackpadPointer("up", 2);
          }
          twoFingerGesture.current = null;
          touchScrollActive.current = false;
          touchScrollAnchor.current = null;
          touchScrollRemainder.current = { x: 0, y: 0 };
        }
        return;
      }
      if (pointerMode === "mouse") {
        const gesture = trackpadGesture.current;
        if (gesture?.pointerId === event.pointerId) {
          if (gesture.dragging) {
            sendTrackpadPointer("up", 0);
          } else if (!gesture.moved) {
            sendTrackpadPointer("down", 0);
            sendTrackpadPointer("up", 0);
          }
          trackpadGesture.current = null;
        }
        return;
      }
    }
    if (longPressPointer.current === event.pointerId) {
      pointerEvent(event, "up", 2);
      longPressPointer.current = null;
    } else {
      pointerEvent(event, "up");
    }
  };

  const pointerWheel = (event: React.WheelEvent<HTMLVideoElement>) => {
    event.preventDefault();
    const plane = videoPointerPlane(event.currentTarget);
    const scale = event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? plane.height : 1;
    sendInput({ type: "pointer", action: "wheel", x: event.deltaX * scale, y: event.deltaY * scale,
      button: 0, width: plane.width, height: plane.height, mapVersion: mapVersion.current });
  };

  const playbackTiming = [stats.jitterBufferMs, stats.decodeMs, stats.rtt];
  const hasPlaybackTiming = playbackTiming.some((value) => value !== undefined);
  const playbackTimingValue = hasPlaybackTiming
    ? playbackTiming.map((value) => value === undefined ? "—" : Math.round(value)).join(" / ")
    : "—";
  const liveMetrics: { id: FloatingMetric; label: string; value: string; unit?: string }[] = [
    { id: "display", label: "桌面", value: stats.displayWidth && stats.displayHeight ? `${stats.displayWidth}×${stats.displayHeight}` : "—" },
    { id: "fps", label: "帧率", value: stats.decodeFPS !== undefined ? `${Math.round(stats.decodeFPS)}` : "—", unit: stats.decodeFPS !== undefined ? "FPS" : undefined },
    { id: "bitrate", label: "码率", value: stats.bitrate !== undefined ? formatBitrate(stats.bitrate) : "—" },
    { id: "playout", label: "缓冲/解码/延迟", value: playbackTimingValue, unit: hasPlaybackTiming ? "ms" : undefined },
  ];
  const activeMetric = floatingMetric ? liveMetrics.find((metric) => metric.id === floatingMetric) : undefined;

  if (!authenticated) {
    return <main className="login-page grid min-h-screen place-items-center p-5">
      <div className="login-card">
        <div className="brand-mark">R</div>
        <p className="eyebrow">REMOTE DESKTOP · PRIVATE NETWORK</p>
        <h1>连接你的 Mac</h1>
        <form onSubmit={login} className="login-form">
          <label htmlFor="token">访问口令</label>
          <input id="token" type="password" lang="en" inputMode="text" autoComplete="current-password" autoCapitalize="off" autoCorrect="off" spellCheck={false} value={token} onChange={(event) => setToken(event.target.value)} placeholder="输入访问口令" required />
          <button className="primary-button" type="submit">登录</button>
        </form>
        {error && <p className="error-message">{error}</p>}
        <div className="network-note"><span className="network-dot" /> HTTP 信令未加密，请仅在可信局域网使用</div>
      </div>
    </main>;
  }

  return <main className="app-shell flex min-h-screen flex-col" onContextMenu={(event) => event.preventDefault()}>
    {error && <div className="connection-error-toast" role="alert" aria-live="assertive">{error}</div>}
    {activeMetric && <button type="button" className="bitrate-float" aria-label={`隐藏${activeMetric.label}浮条`} onClick={() => setFloatingMetric(null)}>
      <span>{activeMetric.label}</span>
      <strong aria-live="polite">{activeMetric.value}{activeMetric.unit && <> <small>{activeMetric.unit}</small></>}</strong>
    </button>}
    <section className="workspace">
      <div className="remote-stage" onContextMenu={(event) => event.preventDefault()}>
        <video
          ref={videoRef}
          className={`remote-video ${connected ? "visible" : ""}`}
          autoPlay muted playsInline controls={false} poster="data:image/gif;base64,R0lGODlhAQABAAD/ACwAAAAAAQABAAACADs=" tabIndex={0}
          onLoadedData={() => setVideoFrameReady(true)}
          onPlaying={() => setVideoFrameReady(true)}
          onClick={() => void resumePlayback()}
          onPointerMove={pointerMove}
          onPointerDown={pointerDown}
          onPointerUp={pointerUp}
          onPointerCancel={() => { resetPointerGestures(); sendInput({ type: "releaseAll" }); }}
          onWheel={pointerWheel}
          onKeyDown={keyDown} onKeyUp={keyUp} onBlur={(event) => {
            const nextTarget = event.relatedTarget;
            if (softKeyboardPointerActive.current || (nextTarget instanceof Element && nextTarget.closest(".soft-keyboard"))) return;
            suppressedRemoteKeyUps.current.clear();
            releaseSoftModifiers();
            sendInput({ type: "releaseAll" });
          }}
        />
        {connected && !videoFrameReady && <div className="video-loading" role="status" aria-live="polite">
          <div className="video-loading-card">
            <span className="video-loading-spinner" aria-hidden="true" />
            <span><strong>正在加载远程画面</strong><small>连接已建立，正在接收桌面</small></span>
          </div>
        </div>}
        {!connected && <div className="stage-placeholder">
          <div className="display-outline"><div className="screen-glow"><div className="screen-orbit" /><div className="screen-orbit orbit-two" /><div className="screen-core"><span>R</span></div><div className="screen-spark spark-one" /><div className="screen-spark spark-two" /></div><div className="display-stand" /><div className="display-base" /></div>
          <p className="stage-kicker">MAC DESKTOP · H.264 / WEBRTC</p>
          <h2>{connecting ? "正在连接 Mac" : "你的桌面，随时可达"}</h2>
          <button className="primary-button stage-connect" onClick={() => { reconnectAttempts.current = 0; void connect(); }} disabled={connecting}>{connecting ? "正在连接…" : "连接桌面"}</button>
        </div>}
      </div>
    </section>
    <FloatingControlsToggle expanded={showInfoModal} onToggle={() => setShowInfoModal((value) => !value)} />
    {showInfoModal && <div className="diagnostics-backdrop" role="presentation" onClick={() => setShowInfoModal(false)}>
      <aside ref={infoModalRef} id="connection-panel" className="side-panel info-modal" role="dialog" aria-modal="true" aria-label="控制面板与诊断数据" tabIndex={-1} onClick={(event) => event.stopPropagation()}>
        <div className="panel-heading"><div><p className="eyebrow">REMOTE CONTROLS</p><h2>控制面板</h2></div></div>
        <div className="panel-control-column">
          {status && !error && <div className={`panel-notice-banner${connecting ? " pending" : ""}`} role="status" aria-live="polite">{status}</div>}
          <div className="panel-control-grid">
            <div className="panel-control-row connection-row">
              {connected || connecting
                ? <button type="button" className="panel-action disconnect" onClick={disconnect}>{connecting ? "取消连接" : "断开连接"}</button>
                : <button type="button" className="panel-action primary" onClick={() => { reconnectAttempts.current = 0; void connect(); }} disabled={connecting}>{connecting ? "连接中…" : "连接桌面"}</button>}
            </div>
            <div className="panel-control-row secondary-control-row">
              <button type="button" className="panel-action" onClick={() => { void toggleFullscreen(); }} aria-pressed={isFullscreen}>{isFullscreen ? "退出全屏" : "全屏"}</button>
              <button type="button" className="panel-action" onClick={() => { setShowKeyboard((value) => !value); setShowInfoModal(false); }}>软键盘</button>
            </div>
          </div>
          <div className="stream-settings-grid" aria-label="视频参数">
            <div className="stream-setting">
              <span>画面</span>
              <GlassSelect label="画面" value={resolutionScale} onChange={setResolutionScale} options={[
                { value: 50, label: "流畅 · 50%" },
                { value: 67, label: "均衡 · 67%" },
                { value: 83, label: "清晰 · 83%" },
                { value: 100, label: "完整 · 100%" },
              ]} />
            </div>
            <div className="stream-setting" role="group" aria-label="鼠标操作模式">
              <span>鼠标</span>
              <GlassSelect label="鼠标" value={pointerMode === "mouse" ? 0 : 1} onChange={(value) => changePointerMode(value === 0 ? "mouse" : "touch")} options={[
                { value: 0, label: "鼠标模式" },
                { value: 1, label: "直接触屏" },
              ]} />
            </div>
            <div className="stream-setting">
              <span>帧率</span>
              <GlassSelect label="帧率" value={fpsCap} onChange={setFpsCap} options={[
                { value: 15, label: "15 FPS" },
                { value: 30, label: "30 FPS" },
                { value: 60, label: "60 FPS" },
              ]} />
            </div>
            <div className="stream-setting">
              <span>码率</span>
              <GlassSelect label="码率" value={quality} onChange={setQuality} options={[
                { value: 2_000_000, label: "2 Mbps" },
                { value: 5_000_000, label: "5 Mbps" },
                { value: 10_000_000, label: "10 Mbps" },
                { value: 25_000_000, label: "25 Mbps" },
              ]} />
            </div>
          </div>
        </div>
        <div className="panel-live-column">
          <div className="metric-grid">
            {liveMetrics.map((metric) => <button key={metric.id} type="button" className="metric-card metric-card-toggle" aria-pressed={floatingMetric === metric.id} aria-label={`显示或隐藏${metric.label}浮条`} onClick={() => setFloatingMetric((current) => current === metric.id ? null : metric.id)}>
              <span>{metric.label}</span>
              <strong>{metric.value}{metric.unit && <> <small>{metric.unit}</small></>}</strong>
            </button>)}
          </div>
        </div>
        <div className="panel-footer"><span className="network-dot" />RemoteMAC {serverVersion} · WebRTC 媒体加密 · HTTP 信令 · 单用户会话</div>
      </aside>
    </div>}

    {showKeyboard && <div className="soft-keyboard"
      onPointerDownCapture={() => {
        if (softKeyboardPointerTimer.current !== undefined) window.clearTimeout(softKeyboardPointerTimer.current);
        softKeyboardPointerActive.current = true;
      }}
      onPointerUpCapture={() => {
        softKeyboardPointerTimer.current = window.setTimeout(() => {
          softKeyboardPointerActive.current = false;
          softKeyboardPointerTimer.current = undefined;
        }, 250);
      }}
      onPointerCancelCapture={() => {
        softKeyboardPointerTimer.current = window.setTimeout(() => {
          softKeyboardPointerActive.current = false;
          softKeyboardPointerTimer.current = undefined;
        }, 250);
      }}>
      <div className="soft-keyboard-input-sink" aria-hidden={!showSoftTextEntry}>
        <input ref={keyboardRef} type="text" aria-label="远程键盘" tabIndex={showSoftTextEntry ? 0 : -1}
          inputMode="text" enterKeyHint="enter" autoComplete="off" autoCapitalize="off" autoCorrect="off" spellCheck={false}
          onFocus={() => { keyboardViewportHeight.current = window.visualViewport?.height ?? window.innerHeight; }}
          onInput={(event) => {
            if (keyboardComposing.current || (event.nativeEvent as InputEvent).isComposing) return;
            forwardKeyboardValue(event.currentTarget);
          }}
          onCompositionStart={() => { keyboardComposing.current = true; }}
          onCompositionEnd={(event) => { keyboardComposing.current = false; forwardKeyboardValue(event.currentTarget); }}
          onKeyDown={(event) => {
            if (keyboardComposing.current || event.nativeEvent.isComposing) return;
            if (event.key === "Enter") {
              event.preventDefault();
              sendSoftKey(36);
              event.currentTarget.value = "";
              keyboardInputValue.current = "";
            } else if (event.key === "Backspace" && !event.currentTarget.value) {
              event.preventDefault();
              sendSoftKey(51);
            }
          }}
          onBlur={(event) => {
            event.currentTarget.value = "";
            keyboardInputValue.current = "";
            keyboardComposing.current = false;
            if (!keyboardTogglePointer.current) setShowSoftTextEntry(false);
          }} />
      </div>
      <div className="soft-keyboard-key-row primary-row" role="group" aria-label="快捷键第一排">
        <button type="button" className="soft-keyboard-key keyboard-toggle" aria-label={showSoftTextEntry ? "关闭系统键盘" : "打开系统键盘"} aria-expanded={showSoftTextEntry} onPointerDown={() => {
          keyboardTogglePointer.current = true;
        }} onPointerUp={() => {
          window.setTimeout(() => {
            keyboardTogglePointer.current = false;
            if (document.activeElement !== keyboardRef.current) setShowSoftTextEntry(false);
          }, 0);
        }} onPointerCancel={() => {
          window.setTimeout(() => {
            keyboardTogglePointer.current = false;
            if (document.activeElement !== keyboardRef.current) setShowSoftTextEntry(false);
          }, 0);
        }} onClick={() => {
          keyboardTogglePointer.current = false;
          if (showSoftTextEntry) {
            keyboardRef.current?.blur();
            setShowSoftTextEntry(false);
          } else {
            keyboardRef.current?.focus();
            setShowSoftTextEntry(true);
          }
        }}>
          <svg viewBox="0 0 36 30" aria-hidden="true"><rect x="1.5" y="1.5" width="33" height="27" rx="4" fill="none" stroke="currentColor" strokeWidth="2"/><g fill="currentColor"><rect x="5" y="6" width="4" height="4" rx=".7"/><rect x="12" y="6" width="4" height="4" rx=".7"/><rect x="19" y="6" width="4" height="4" rx=".7"/><rect x="26" y="6" width="4" height="4" rx=".7"/><rect x="5" y="13" width="4" height="4" rx=".7"/><rect x="12" y="13" width="4" height="4" rx=".7"/><rect x="19" y="13" width="4" height="4" rx=".7"/><rect x="26" y="13" width="4" height="4" rx=".7"/><rect x="9" y="21" width="18" height="3" rx="1.2"/></g></svg>
        </button>
        {SOFT_PRIMARY_KEYS.map((key) => <button key={key.missionControl ? "mission-control" : key.keyCode} type="button" className={`soft-keyboard-key${key.modifier ? " modifier" : ""}${key.modifier && activeSoftModifiers.includes(key.keyCode) ? " active" : ""}${key.missionControl ? " mission-control" : ""}${key.icon ? " shortcut-icon" : ""}`} aria-label={key.missionControl ? "打开调度中心" : key.label} aria-pressed={key.modifier ? activeSoftModifiers.includes(key.keyCode) : undefined} title={key.missionControl ? "打开 macOS 调度中心" : key.modifier ? "点按锁定，再点一次取消" : key.keyCode === 51 ? "退格键（删除光标前的字符）" : undefined} onClick={() => key.missionControl ? openMissionControl() : key.modifier ? toggleSoftModifier(key.keyCode) : sendSoftKey(key.keyCode)}>
          {key.icon ? <SoftKeyIcon kind={key.icon} /> : key.missionControl ? <svg viewBox="0 0 48 36" aria-hidden="true">
            <g fill="none" stroke="currentColor" strokeWidth="2.8" strokeLinejoin="round">
              <rect x="3.5" y="4.5" width="17" height="13" rx="2.5" />
              <rect x="7.5" y="22" width="13" height="9" rx="2" />
              <rect x="25" y="5" width="19" height="26" rx="3.5" />
            </g>
          </svg> : key.label}
        </button>)}
      </div>
      <div className="soft-keyboard-key-row secondary-row" role="group" aria-label="快捷键第二排">
        <button type="button" className="soft-keyboard-key keyboard-close" aria-label="关闭辅助键盘" onClick={() => { keyboardRef.current?.blur(); releaseSoftModifiers(); setShowSoftTextEntry(false); setShowKeyboard(false); }}><span aria-hidden="true">×</span></button>
        {SOFT_SECONDARY_KEYS.map((key) => <button key={key.keyCode} type="button" className={`soft-keyboard-key${key.modifier ? " modifier" : ""}${key.modifier && activeSoftModifiers.includes(key.keyCode) ? " active" : ""}${key.icon ? " shortcut-icon" : ""}`} aria-label={key.label} aria-pressed={key.modifier ? activeSoftModifiers.includes(key.keyCode) : undefined} title={key.icon === "return" ? "回车键" : key.keyCode === 51 ? "长按连续删除" : key.modifier ? "点按锁定，再点一次取消" : undefined} onPointerDown={key.keyCode === 51 ? startBackspaceRepeat : undefined} onPointerUp={key.keyCode === 51 ? stopBackspaceRepeat : undefined} onPointerCancel={key.keyCode === 51 ? stopBackspaceRepeat : undefined} onContextMenu={key.keyCode === 51 ? (event) => event.preventDefault() : undefined} onClick={() => key.modifier ? toggleSoftModifier(key.keyCode) : key.keyCode === 51 ? clickBackspace() : sendSoftKey(key.keyCode)}>{key.icon ? <SoftKeyIcon kind={key.icon} /> : key.label}</button>)}
        <button type="button" className="soft-keyboard-key shortcut-space" aria-label="Command 加空格键" title="发送 ⌘ + 空格键" onClick={() => sendSoftChord(55, 49)}><strong>⌘</strong><small>Space</small></button>
      </div>
    </div>}
  </main>;
}

export default App;
