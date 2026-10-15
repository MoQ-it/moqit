import {
  AudioOutput,
  type LocalParticipant,
  type LocalVideoTrack,
  type Participant,
  type RemoteParticipant,
  type RemoteVideoTrack,
  type Room,
  TrackSource,
} from "@moqit/client";
import { createContext, type CSSProperties, type ReactNode, useContext, useEffect, useRef, useState } from "react";
import {
  RoomContext, useActiveDevice, useAudioPlayback, useChat, useIsSpeaking, useLocalParticipant, useMediaDevices, useParticipants, useRoom, useRoomUpdates,
  useScreenShares, useTimeline, useVideoTrack,
} from "./hooks.ts";
import { gridFit } from "./layout.ts";
import { formatClock } from "./time.ts";

export function RoomProvider({ room, children }: { room: Room; children: ReactNode }) {
  return <RoomContext.Provider value={room}>{children}</RoomContext.Provider>;
}

/** Shows a local or remote video track. */
export function VideoView({ track, mirror, className }: { track: LocalVideoTrack | RemoteVideoTrack; mirror?: boolean; className?: string }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = track.attach();
    el.className = "moqit-video";
    if (mirror) el.style.transform = "scaleX(-1)";
    ref.current?.replaceChildren(el);
    if (el instanceof HTMLVideoElement) void el.play().catch(() => {});
    return () => el.remove();
  }, [track, mirror]);
  return <div ref={ref} className={className ?? "moqit-video-box"} />;
}

/** Provide true to show each tile's video resolution and simulcast layer. */
export const VideoStatsContext = createContext(false);

/** "1.4 Mbps", "320 kbps". */
export function formatBitrate(bps: number): string {
  if (bps >= 1_000_000) return `${(bps / 1_000_000).toFixed(1)} Mbps`;
  if (bps >= 1000) return `${Math.round(bps / 1000)} kbps`;
  return `${Math.round(bps)} bps`;
}

/**
 * Remote: the size, layer and bitrate received. Local: each layer being
 * sent, with its size and bitrate (a layer nobody watches is not sent).
 */
function VideoStats({ participant, source, track }: { participant: Participant; source: TrackSource; track: LocalVideoTrack | RemoteVideoTrack }) {
  const [lines, setLines] = useState<{ text: string; rendition?: string; resolution?: string; bitrate: number; limited?: boolean; target?: number }[]>([]);
  useEffect(() => {
    const read = () => {
      if (participant.isLocal) {
        const local = track as LocalVideoTrack;
        const { width, height } = local.capturedTrack.getSettings();
        const captured = width && height ? [{ text: `● captured ${width}×${height}`, bitrate: 0 }] : [];
        const sending = local.sending().filter((r) => r.width > 0);
        const suspended = local.suspended().map((name) => ({ text: `${name} paused: uplink too slow`, bitrate: 0, limited: true }));
        if (sending.length === 0) return [...captured, ...suspended, { text: "not sent: nobody watching", bitrate: 0 }];
        return [...captured, ...sending.map((r) => ({
          text: `↑ ${r.rendition} ${r.width}×${r.height} · ${formatBitrate(r.bitrate)}${r.limited ? ` · limited to ${formatBitrate(r.targetBitrate)}${r.framerate < 20 ? ` at ${Math.round(r.framerate)} fps` : ""}` : ""}`,
          rendition: r.rendition, resolution: `${r.width}x${r.height}`, bitrate: r.bitrate, limited: r.limited, target: r.targetBitrate,
        })), ...suspended];
      }
      const r = track as RemoteVideoTrack;
      if (!r.width) return [];
      const layer = (participant as RemoteParticipant).getTrackPublication(source)?.currentRendition;
      return [{ text: `↓ ${r.width}×${r.height}${layer ? ` · ${layer}` : ""} · ${formatBitrate(r.bitrate)}`, rendition: layer, resolution: `${r.width}x${r.height}`, bitrate: r.bitrate }];
    };
    setLines(read());
    const id = setInterval(() => setLines(read()), 1000);
    return () => clearInterval(id);
  }, [participant, source, track]);
  if (lines.length === 0) return null;
  return (
    <div className="moqit-stats" data-resolution={lines.find((l) => l.resolution)?.resolution} data-bitrate={Math.round(lines.reduce((s, l) => s + l.bitrate, 0))}>
      {lines.map((l, i) => (
        <div key={l.rendition ?? i} data-rendition={l.rendition} data-bitrate={Math.round(l.bitrate)} data-limited={l.limited || undefined} data-target={l.target && Math.round(l.target)}>
          {l.text}
        </div>
      ))}
    </div>
  );
}

/** The meeting's total media bitrate sent (↑) and received (↓). */
export function MeetingStats() {
  const room = useContext(RoomContext);
  const [stats, setStats] = useState({ sendBitrate: 0, receiveBitrate: 0, sendLimited: false, sendTarget: 0 });
  useEffect(() => {
    if (!room) return;
    const id = setInterval(() => setStats(room.getStats()), 1000);
    return () => clearInterval(id);
  }, [room]);
  return (
    <span className="moqit-meeting-stats" data-send={Math.round(stats.sendBitrate)} data-receive={Math.round(stats.receiveBitrate)} data-limited={stats.sendLimited || undefined}
      title={stats.sendLimited ? `sending less video: your connection carries about ${formatBitrate(stats.sendTarget)}` : "media bitrate sent and received"}>
      ↑ {formatBitrate(stats.sendBitrate)}{stats.sendLimited ? " (limited)" : ""} ↓ {formatBitrate(stats.receiveBitrate)}
    </span>
  );
}

function initials(name: string): string {
  return name.split(/\s+/).map((w) => w[0]?.toUpperCase() ?? "").slice(0, 2).join("");
}

/**
 * A participant's camera or screen. With onPin, a button (and a double
 * click) asks to pin it to the stage, or to unpin it when pinned.
 */
export function ParticipantTile({ participant, source = TrackSource.CAMERA, onPin, pinned = false }: {
  participant: Participant; source?: TrackSource; onPin?: () => void; pinned?: boolean;
}) {
  const video = useVideoTrack(participant, source);
  const speaking = useIsSpeaking(participant);
  useRoomUpdates();
  const name = participant.name || participant.identity;
  const micMuted = participant.isLocal
    ? !(participant as LocalParticipant).getTrack(TrackSource.MICROPHONE) || (participant as LocalParticipant).getTrack(TrackSource.MICROPHONE)!.muted
    : !(participant as RemoteParticipant).getTrackPublication(TrackSource.MICROPHONE) || (participant as RemoteParticipant).getTrackPublication(TrackSource.MICROPHONE)!.muted;
  const videoMuted = participant.isLocal
    ? (participant as LocalParticipant).getTrack(source)?.muted
    : (participant as RemoteParticipant).getTrackPublication(source)?.muted;
  const isScreen = source === TrackSource.SCREEN;
  const showStats = useContext(VideoStatsContext);
  return (
    <div className={`moqit-tile${speaking && !isScreen ? " speaking" : ""}${isScreen ? " screen" : ""}${pinned ? " pinned" : ""}`} data-identity={participant.identity} data-source={isScreen ? "screen" : "camera"}
      onDoubleClick={onPin}>
      {video && !videoMuted ? (
        <VideoView track={video} mirror={participant.isLocal && !isScreen} />
      ) : (
        <div className="moqit-avatar">{initials(name)}</div>
      )}
      {showStats && video && !videoMuted && <VideoStats participant={participant} source={source} track={video} />}
      <div className="moqit-name">
        {!isScreen && micMuted && <span className="moqit-muted" title="microphone off">🔇</span>}
        {isScreen ? `${name} (screen)` : name}
        {participant.isLocal && !isScreen && " (you)"}
      </div>
      {onPin && (
        <button className="moqit-pin" onClick={onPin} aria-label={`${pinned ? "unpin" : "pin"} ${name}${isScreen ? " (screen)" : ""}`} title={pinned ? "Unpin" : "Pin to the stage"}>
          {pinned ? "✕" : "⤢"}
        </button>
      )}
    </div>
  );
}

/**
 * Every participant's camera tile in a grid sized to the element: as large
 * as they can be while all fit, the last row centred. Tiles are offered
 * for pinning with onPin.
 */
export function GridLayout({ onPin }: { onPin?: (participant: Participant) => void } = {}) {
  const participants = useParticipants();
  const ref = useRef<HTMLDivElement>(null);
  const [size, setSize] = useState({ width: 0, height: 0, gap: 10 });
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver(() => setSize({ width: el.clientWidth, height: el.clientHeight, gap: parseFloat(getComputedStyle(el).columnGap) || 0 }));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  const { cols, tileWidth } = gridFit(size.width, size.height, participants.length, { gap: size.gap });
  return (
    <div ref={ref} className="moqit-grid" data-cols={cols} style={{ "--moqit-tile-width": `${tileWidth}px` } as CSSProperties}>
      {participants.map((p) => <ParticipantTile key={p.identity} participant={p} onPin={onPin && (() => onPin(p))} />)}
    </div>
  );
}

/**
 * One participant's screen (by default) or camera on the stage, with every
 * other camera and shared screen in a strip beside it. The presenter sees
 * a "you are presenting" card rather than a mirror of their own screen.
 * With onPin, strip tiles can be pinned; with onUnpin, the stage shows it
 * is pinned and offers to unpin. The stage can go full screen.
 */
export function FocusLayout({ presenter, source = TrackSource.SCREEN, onPin, onUnpin }: {
  presenter: Participant; source?: TrackSource; onPin?: (participant: Participant, source: TrackSource) => void; onUnpin?: () => void;
}) {
  const participants = useParticipants();
  const shares = useScreenShares();
  const stage = useRef<HTMLDivElement>(null);
  const [full, setFull] = useState(false);
  useEffect(() => {
    const on = () => setFull(!!stage.current && document.fullscreenElement === stage.current);
    document.addEventListener("fullscreenchange", on);
    return () => document.removeEventListener("fullscreenchange", on);
  }, []);
  const card = presenter.isLocal && source === TrackSource.SCREEN;
  const onStage = (p: Participant, s: TrackSource) => p.identity === presenter.identity && s === source;
  const cameras = participants.filter((p) => !onStage(p, TrackSource.CAMERA));
  // Your own screen is never shown back to you.
  const screens = shares.filter((p) => !p.isLocal && !onStage(p, TrackSource.SCREEN));
  const toggleFull = () => {
    if (document.fullscreenElement) void document.exitFullscreen();
    else void stage.current?.requestFullscreen().catch(() => {});
  };
  return (
    <div className="moqit-focus">
      <div ref={stage} className="moqit-stage" data-presenter={presenter.identity} data-source={source === TrackSource.SCREEN ? "screen" : "camera"}>
        {card ? <PresentingCard /> : <ParticipantTile participant={presenter} source={source} onPin={onUnpin} pinned={!!onUnpin} />}
        {!card && document.fullscreenEnabled && (
          <button className="moqit-fullscreen" onClick={toggleFull} aria-label={full ? "exit full screen" : "full screen"} title={full ? "Exit full screen" : "Full screen"}>
            {full ? "⤡" : "⛶"}
          </button>
        )}
      </div>
      <div className="moqit-strip">
        {screens.map((p) => <ParticipantTile key={`${p.identity}/screen`} participant={p} source={TrackSource.SCREEN} onPin={onPin && (() => onPin(p, TrackSource.SCREEN))} />)}
        {cameras.map((p) => <ParticipantTile key={p.identity} participant={p} onPin={onPin && (() => onPin(p, TrackSource.CAMERA))} />)}
      </div>
    </div>
  );
}

/** A participant's camera or screen chosen for the stage. */
export interface Pin {
  identity: string;
  source: TrackSource;
}

/**
 * The grid, or one tile on the stage: the one the user pinned, else a
 * shared screen (remote shares win over your own). Any tile can be pinned,
 * and a pin ends when its participant leaves or stops sharing.
 */
export function ConferenceLayout() {
  const participants = useParticipants();
  const shares = useScreenShares();
  const [pin, setPin] = useState<Pin>();
  const pinned = pin && (pin.source === TrackSource.SCREEN ? shares : participants).find((p) => p.identity === pin.identity);
  useEffect(() => {
    if (pin && !pinned) setPin(undefined);
  }, [pin, pinned]);
  const onPin = (p: Participant, source: TrackSource) => setPin({ identity: p.identity, source });
  if (pin && pinned) return <FocusLayout presenter={pinned} source={pin.source} onPin={onPin} onUnpin={() => setPin(undefined)} />;
  const presenter = shares.find((p) => !p.isLocal) ?? shares[0];
  if (presenter) return <FocusLayout presenter={presenter} onPin={onPin} />;
  return <GridLayout onPin={(p) => onPin(p, TrackSource.CAMERA)} />;
}

/** Shown to the presenter in place of their own screen. */
export function PresentingCard() {
  const local = useLocalParticipant();
  const showStats = useContext(VideoStatsContext);
  const track = local?.getTrack(TrackSource.SCREEN) as LocalVideoTrack | undefined;
  if (!local || !track) return null;
  return (
    <div className="moqit-presenting">
      <div className="moqit-presenting-preview">
        <VideoView track={track} />
        {showStats && <VideoStats participant={local} source={TrackSource.SCREEN} track={track} />}
      </div>
      <p>You are presenting to everyone</p>
      <button className="leave" onClick={() => void local.setScreenShareEnabled(false)}>
        Stop presenting
      </button>
    </div>
  );
}

/** Microphone, camera, screen share, devices, chat and leave; `children` go before chat. */
export function ControlBar({ onLeave, onToggleChat, unread = 0, children }: { onLeave: () => void; onToggleChat?: () => void; unread?: number; children?: ReactNode }) {
  const local = useLocalParticipant();
  const [busy, setBusy] = useState(false);
  if (!local) return null;
  const mic = local.getTrack(TrackSource.MICROPHONE);
  const cam = local.getTrack(TrackSource.CAMERA);
  const screen = local.getTrack(TrackSource.SCREEN);
  const act = (f: () => Promise<unknown>) => async () => {
    setBusy(true);
    try {
      await f();
    } catch (e) {
      alert(String((e as Error).message ?? e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="moqit-controls">
      <button disabled={busy} aria-label="microphone" className={mic && !mic.muted ? "on" : "off"}
        onClick={act(() => (mic ? local.setTrackMuted("microphone", !mic.muted) : local.setMicrophoneEnabled(true)))}>
        {mic && !mic.muted ? "🎙 Mute" : "🎙 Unmute"}
      </button>
      <button disabled={busy} aria-label="camera" className={cam ? "on" : "off"} onClick={act(() => local.setCameraEnabled(!cam))}>
        {cam ? "📷 Stop video" : "📷 Start video"}
      </button>
      {"getDisplayMedia" in (navigator.mediaDevices ?? {}) && (
        <button disabled={busy} aria-label="screen share" className={screen ? "on" : "off"} onClick={act(() => local.setScreenShareEnabled(!screen))}>
          {screen ? "🖥 Stop sharing" : "🖥 Share screen"}
        </button>
      )}
      <DeviceMenu />
      {children}
      {onToggleChat && (
        <button aria-label="chat" onClick={onToggleChat}>
          💬 Chat{unread > 0 && <span className="moqit-badge">{unread}</span>}
        </button>
      )}
      <button className="leave" aria-label="leave" onClick={onLeave}>Leave</button>
    </div>
  );
}

const deviceNames: Record<MediaDeviceKind, string> = { audioinput: "Microphone", videoinput: "Camera", audiooutput: "Speaker" };

/**
 * A microphone, camera or speaker picker. `refresh` lists the devices
 * again, e.g. once permission has been granted and labels are known.
 */
export function DeviceSelect({ kind, value, onChange, refresh, disabled }: {
  kind: MediaDeviceKind; value: string | undefined; onChange: (deviceId: string) => void; refresh?: unknown; disabled?: boolean;
}) {
  const devices = useMediaDevices(kind, refresh);
  if (kind === "audiooutput" && !AudioOutput.canSelectOutput) return null;
  // No choice yet means the system default, which Chromium lists as "default".
  if (!value && devices.some((d) => d.deviceId === "default")) value = "default";
  const known = devices.some((d) => d.deviceId === value);
  return (
    <label className="moqit-device-select">
      <span>{deviceNames[kind]}</span>
      <select aria-label={deviceNames[kind].toLowerCase()} value={known ? value : ""} disabled={disabled || devices.length === 0} onChange={(e) => onChange(e.target.value)}>
        {!known && <option value="">{devices.length === 0 ? `No ${deviceNames[kind].toLowerCase()} found` : "Default"}</option>}
        {devices.map((d, i) => (
          <option key={d.deviceId} value={d.deviceId}>
            {d.label || `${deviceNames[kind]} ${i + 1}`}
          </option>
        ))}
      </select>
    </label>
  );
}

/** Picks the room's microphone, camera or speaker; a live track moves to the new device. */
export function ActiveDeviceSelect({ kind }: { kind: MediaDeviceKind }) {
  const { deviceId, setActiveDevice } = useActiveDevice(kind);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  return (
    <>
      <DeviceSelect kind={kind} value={deviceId} disabled={busy} onChange={(id) => {
        setBusy(true);
        setError("");
        setActiveDevice(id).catch((e) => setError(String(e?.message ?? e))).finally(() => setBusy(false));
      }} />
      {error && <div className="moqit-device-error">{error}</div>}
    </>
  );
}

/** A control-bar button opening microphone, camera and speaker pickers. */
export function DeviceMenu() {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: Event) => !ref.current?.contains(e.target as Node) && setOpen(false);
    const esc = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", esc);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", esc);
    };
  }, [open]);
  return (
    <div className="moqit-device-menu-anchor" ref={ref}>
      <button aria-label="devices" aria-expanded={open} className={open ? "on" : undefined} onClick={() => setOpen((o) => !o)}>
        ⚙ Devices
      </button>
      {open && (
        <div className="moqit-device-menu" role="dialog" aria-label="device settings">
          <ActiveDeviceSelect kind="audioinput" />
          <ActiveDeviceSelect kind="videoinput" />
          <ActiveDeviceSelect kind="audiooutput" />
        </div>
      )}
    </div>
  );
}

/**
 * The chat panel. Pass `chat` from a useChat() call in a component that
 * stays mounted for the whole meeting, so messages that arrive while the
 * panel is closed are kept.
 */
export function Chat({ chat }: { chat?: ReturnType<typeof useChat> }) {
  const own = useChat();
  const { messages, send } = chat ?? own;
  const [text, setText] = useState("");
  const list = useRef<HTMLDivElement>(null);
  useEffect(() => {
    list.current?.scrollTo({ top: list.current.scrollHeight });
  }, [messages.length]);
  return (
    <div className="moqit-chat">
      <div className="moqit-chat-list" ref={list}>
        {messages.length === 0 && <div className="moqit-chat-empty">No messages yet</div>}
        {messages.map((m) => (
          <div key={m.id} className={`moqit-chat-msg${m.local ? " local" : ""}`}>
            <span className="from">{m.local ? "You" : m.fromName}</span>
            <span className="text">{m.text}</span>
          </div>
        ))}
      </div>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (text.trim()) void send(text.trim());
          setText("");
        }}
      >
        <input aria-label="chat message" value={text} onChange={(e) => setText(e.target.value)} placeholder="Message everyone" />
        <button type="submit">Send</button>
      </form>
    </div>
  );
}

/** Offers to start audio when the browser blocked autoplay. */
export function StartAudio() {
  const { canPlay, start } = useAudioPlayback();
  if (canPlay) return null;
  return <button className="moqit-start-audio" onClick={() => void start()}>🔊 Click to enable audio</button>;
}

/**
 * For viewers of a room that keeps history: a bar to rewind it (oldest on
 * the left, live on the right), a 10 seconds back button and a LIVE
 * button. Renders nothing otherwise.
 */
export function Scrubber() {
  const room = useRoom();
  const tl = useTimeline();
  const [drag, setDrag] = useState<number>();
  const commit = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(commit.current), []);
  if (!tl.available) return null;
  const span = Math.max(1, Math.ceil(tl.history));
  const value = drag ?? Math.max(0, span - Math.round(tl.behind));
  const seek = (behind: number) => void (behind < 2 ? room.goLive() : room.seek(Math.min(behind, span))).catch(() => {});
  const onChange = (v: number) => {
    setDrag(v);
    clearTimeout(commit.current);
    commit.current = setTimeout(() => {
      seek(span - v);
      setDrag(undefined);
    }, 250);
  };
  return (
    <div className="moqit-scrubber" data-live={tl.live} data-behind={Math.round(tl.behind)}>
      <button className="moqit-back" onClick={() => seek(tl.behind + 10)} title="Back 10 seconds" aria-label="back 10 seconds">
        −10s
      </button>
      <input
        type="range" min={0} max={span} step={1} value={Math.min(value, span)} aria-label="position in the meeting"
        onChange={(e) => onChange(Number(e.currentTarget.value))}
      />
      <span className="moqit-behind">{tl.live ? "" : `−${formatClock(span - value)}`}</span>
      <button className="moqit-live" data-live={tl.live} disabled={tl.live} onClick={() => seek(0)} title={tl.live ? "Live" : "Go live"}>
        LIVE
      </button>
    </div>
  );
}
