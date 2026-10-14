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
import { createContext, type ReactNode, useContext, useEffect, useRef, useState } from "react";
import {
  RoomContext, useActiveDevice, useAudioPlayback, useChat, useIsSpeaking, useLocalParticipant, useMediaDevices, useParticipants, useRoomUpdates,
  useScreenShares, useVideoTrack,
} from "./hooks.ts";

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
  const [lines, setLines] = useState<{ text: string; rendition?: string; resolution?: string; bitrate: number }[]>([]);
  useEffect(() => {
    const read = () => {
      if (participant.isLocal) {
        const local = track as LocalVideoTrack;
        const { width, height } = local.capturedTrack.getSettings();
        const captured = width && height ? [{ text: `● captured ${width}×${height}`, bitrate: 0 }] : [];
        const sending = local.sending().filter((r) => r.width > 0);
        if (sending.length === 0) return [...captured, { text: "not sent: nobody watching", bitrate: 0 }];
        return [...captured, ...sending.map((r) => ({ text: `↑ ${r.rendition} ${r.width}×${r.height} · ${formatBitrate(r.bitrate)}`, rendition: r.rendition, resolution: `${r.width}x${r.height}`, bitrate: r.bitrate }))];
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
      {lines.map((l, i) => <div key={l.rendition ?? i} data-rendition={l.rendition} data-bitrate={Math.round(l.bitrate)}>{l.text}</div>)}
    </div>
  );
}

/** The meeting's total media bitrate sent (↑) and received (↓). */
export function MeetingStats() {
  const room = useContext(RoomContext);
  const [stats, setStats] = useState({ sendBitrate: 0, receiveBitrate: 0 });
  useEffect(() => {
    if (!room) return;
    const id = setInterval(() => setStats(room.getStats()), 1000);
    return () => clearInterval(id);
  }, [room]);
  return (
    <span className="moqit-meeting-stats" data-send={Math.round(stats.sendBitrate)} data-receive={Math.round(stats.receiveBitrate)} title="media bitrate sent and received">
      ↑ {formatBitrate(stats.sendBitrate)} ↓ {formatBitrate(stats.receiveBitrate)}
    </span>
  );
}

function initials(name: string): string {
  return name.split(/\s+/).map((w) => w[0]?.toUpperCase() ?? "").slice(0, 2).join("");
}

export function ParticipantTile({ participant, source = TrackSource.CAMERA }: { participant: Participant; source?: TrackSource }) {
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
    <div className={`moqit-tile${speaking && !isScreen ? " speaking" : ""}${isScreen ? " screen" : ""}`} data-identity={participant.identity} data-source={isScreen ? "screen" : "camera"}>
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
    </div>
  );
}

/** Every participant's camera tile in a grid. */
export function GridLayout() {
  const participants = useParticipants();
  const cols = Math.ceil(Math.sqrt(participants.length));
  return (
    <div className="moqit-grid" style={{ gridTemplateColumns: `repeat(${cols}, minmax(0, 1fr))` }}>
      {participants.map((p) => <ParticipantTile key={p.identity} participant={p} />)}
    </div>
  );
}

/**
 * A shared screen on the stage with everyone's camera in a strip beside
 * it. The presenter sees a "you are presenting" card rather than a mirror
 * of their own screen.
 */
export function FocusLayout({ presenter }: { presenter: Participant }) {
  const participants = useParticipants();
  return (
    <div className="moqit-focus">
      <div className="moqit-stage" data-presenter={presenter.identity}>
        {presenter.isLocal ? <PresentingCard /> : <ParticipantTile participant={presenter} source={TrackSource.SCREEN} />}
      </div>
      <div className="moqit-strip">
        {participants.map((p) => <ParticipantTile key={p.identity} participant={p} />)}
      </div>
    </div>
  );
}

/** Focus on a screen share when there is one, otherwise the grid. Remote shares win over your own. */
export function ConferenceLayout() {
  const shares = useScreenShares();
  const presenter = shares.find((p) => !p.isLocal) ?? shares[0];
  return presenter ? <FocusLayout presenter={presenter} /> : <GridLayout />;
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
