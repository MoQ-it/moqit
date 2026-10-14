import { ConnectionState, Room, TrackSource } from "@moqit/client";
import { Chat, ConferenceLayout, ControlBar, DeviceSelect, MeetingStats, RoomProvider, StartAudio, useChat, useParticipants, useScreenShares, useViewerCount, VideoStatsContext } from "@moqit/react";
import { useEffect, useRef, useState } from "react";
import { navigate } from "./main.tsx";
import { CameraEffects, hasVideoEffects, NoiseRemoval } from "./effects.ts";
import { type EffectChoices, EffectsFields, EffectsMenu, saveEffects, savedEffects, usePreviewEffects, videoEffects } from "./Effects.tsx";
import { type QualityChoices, QualityMenu, saveQuality, savedQuality, SendSelect } from "./Quality.tsx";

type Devices = Partial<Record<MediaDeviceKind, string>>;

interface Choices {
  name: string;
  camera: boolean;
  microphone: boolean;
  devices: Devices;
  effects: EffectChoices;
  quality: QualityChoices;
}

const DEVICES_KEY = "moqit-devices";

function savedDevices(): Devices {
  try {
    return JSON.parse(localStorage.getItem(DEVICES_KEY) ?? "{}");
  } catch {
    return {};
  }
}

function saveDevice(kind: MediaDeviceKind, id: string) {
  localStorage.setItem(DEVICES_KEY, JSON.stringify({ ...savedDevices(), [kind]: id }));
}

export interface ConnectionDetails {
  url: string;
  token: string;
  certHash?: string; // development servers with a self-signed certificate
}

// After a reload, this tab rejoins as the same participant: the server then
// replaces the old session at once rather than showing it frozen until it
// times out. The identity is stashed only when the page goes away, so a
// duplicated tab (which copies sessionStorage) does not take it over.
const REJOIN_KEY = "moqit-rejoin";
const rejoin: { room: string; suffix: string } | undefined = (() => {
  try {
    const v = JSON.parse(sessionStorage.getItem(REJOIN_KEY) ?? "null");
    sessionStorage.removeItem(REJOIN_KEY);
    return v ?? undefined;
  } catch {
    return undefined;
  }
})();

async function fetchConnectionDetails(room: string, name: string): Promise<ConnectionDetails> {
  const body = { room, name, rejoin: rejoin?.room === room ? rejoin.suffix : undefined };
  const res = await fetch("/api/token", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

export function hexBytes(hex: string): Uint8Array {
  return Uint8Array.from(hex.match(/../g) ?? [], (h) => parseInt(h, 16));
}

export function Meeting({ room: roomName }: { room: string }) {
  const params = new URLSearchParams(location.search);
  const [choices, setChoices] = useState<Choices | undefined>(() =>
    params.get("autojoin") && params.get("name") ? { name: params.get("name")!, camera: params.get("camera") !== "0", microphone: params.get("mic") !== "0", devices: savedDevices(), effects: savedEffects(), quality: savedQuality() } : undefined,
  );
  if (!choices) return <PreJoin room={roomName} onJoin={setChoices} />;
  return <InRoom roomName={roomName} choices={choices} />;
}

// Opens the chosen device, or the default one if it is gone.
async function openDevice(kind: "audio" | "video", constraints: MediaTrackConstraints, deviceId?: string): Promise<MediaStream> {
  const get = (c: MediaTrackConstraints) => navigator.mediaDevices.getUserMedia({ [kind]: c });
  if (!deviceId) return get(constraints);
  try {
    return await get({ ...constraints, deviceId: { exact: deviceId } });
  } catch (e) {
    if (!["OverconstrainedError", "NotFoundError"].includes((e as DOMException).name)) throw e;
    return get(constraints);
  }
}

function PreJoin({ room, onJoin }: { room: string; onJoin: (c: Choices) => void }) {
  const [name, setName] = useState(() => localStorage.getItem("moqit-name") ?? "");
  const [camera, setCamera] = useState(true);
  const [microphone, setMicrophone] = useState(true);
  // What the user picked, and what the previews actually opened (the
  // picked device may be gone, or nothing picked yet).
  const [chosen, setChosen] = useState<Devices>(savedDevices);
  const [opened, setOpened] = useState<Devices>({});
  const [cameraError, setCameraError] = useState("");
  const [micError, setMicError] = useState("");
  const [permitted, setPermitted] = useState(0); // lists devices again once labels are visible
  const [level, setLevel] = useState(0);
  const [cameraTrack, setCameraTrack] = useState<MediaStreamTrack>();
  const [effects, setEffects] = useState(savedEffects);
  const [quality, setQuality] = useState(savedQuality);
  const preview = usePreviewEffects(cameraTrack, videoEffects(effects));
  const video = useRef<HTMLVideoElement>(null);

  useEffect(() => {
    if (video.current) video.current.srcObject = preview.track ? new MediaStream([preview.track]) : null;
  }, [preview.track, camera]);

  useEffect(() => {
    if (!camera) return;
    let stream: MediaStream | undefined;
    let cancelled = false;
    setCameraError("");
    openDevice("video", { width: 640, height: 360 }, chosen.videoinput)
      .then((s) => {
        if (cancelled) return s.getTracks().forEach((t) => t.stop());
        stream = s;
        setCameraTrack(s.getVideoTracks()[0]);
        setOpened((o) => ({ ...o, videoinput: s.getVideoTracks()[0].getSettings().deviceId }));
        setPermitted((n) => n + 1);
      })
      .catch((e) => setCameraError(`Camera unavailable: ${e.message}`));
    return () => {
      cancelled = true;
      setCameraTrack(undefined);
      stream?.getTracks().forEach((t) => t.stop());
    };
  }, [camera, chosen.videoinput]);

  // Microphone preview: a level meter, so you can see it hears you.
  useEffect(() => {
    if (!microphone) return setLevel(0);
    let stream: MediaStream | undefined;
    let ctx: AudioContext | undefined;
    let timer: ReturnType<typeof setInterval> | undefined;
    let cancelled = false;
    setMicError("");
    openDevice("audio", {}, chosen.audioinput)
      .then((s) => {
        if (cancelled) return s.getTracks().forEach((t) => t.stop());
        stream = s;
        setOpened((o) => ({ ...o, audioinput: s.getAudioTracks()[0].getSettings().deviceId }));
        setPermitted((n) => n + 1);
        ctx = new AudioContext();
        const analyser = ctx.createAnalyser();
        analyser.fftSize = 1024;
        ctx.createMediaStreamSource(s).connect(analyser);
        const buf = new Float32Array(analyser.fftSize);
        timer = setInterval(() => {
          analyser.getFloatTimeDomainData(buf);
          const rms = Math.sqrt(buf.reduce((a, x) => a + x * x, 0) / buf.length);
          setLevel(Math.max(0, Math.min(1, (60 + 20 * Math.log10(rms || 1e-6)) / 60)));
        }, 100);
      })
      .catch((e) => setMicError(`Microphone unavailable: ${e.message}`));
    return () => {
      cancelled = true;
      clearInterval(timer);
      void ctx?.close();
      stream?.getTracks().forEach((t) => t.stop());
    };
  }, [microphone, chosen.audioinput]);

  const pick = (kind: MediaDeviceKind) => (id: string) => setChosen((c) => ({ ...c, [kind]: id }));
  const error = cameraError || micError || (preview.error && `Effects unavailable: ${preview.error}`);

  return (
    <main className="prejoin">
      <h2>
        Join <code>{room}</code>
      </h2>
      <div className="preview">
        {camera ? <video ref={video} autoPlay muted playsInline data-effects={preview.track !== cameraTrack ? "on" : "off"} /> : <div className="moqit-avatar">Camera off</div>}
        {camera && preview.starting && <div className="preview-status">Starting effects…</div>}
      </div>
      <div className="devices">
        <div className="device-row">
          <label className="toggle">
            <input type="checkbox" checked={microphone} onChange={(e) => setMicrophone(e.target.checked)} /> Microphone
          </label>
          <DeviceSelect kind="audioinput" value={opened.audioinput ?? chosen.audioinput} onChange={pick("audioinput")} refresh={permitted} disabled={!microphone} />
          <div className="meter" aria-label="microphone level" data-level={level.toFixed(2)}>
            <div style={{ width: `${Math.round(level * 100)}%` }} />
          </div>
        </div>
        <div className="device-row">
          <label className="toggle">
            <input type="checkbox" checked={camera} onChange={(e) => setCamera(e.target.checked)} /> Camera
          </label>
          <DeviceSelect kind="videoinput" value={opened.videoinput ?? chosen.videoinput} onChange={pick("videoinput")} refresh={permitted} disabled={!camera} />
          <div className="send-quality">
            <SendSelect value={quality.send} onChange={(send) => setQuality((q) => ({ ...q, send }))} disabled={!camera} />
          </div>
        </div>
        <div className="device-row">
          <span className="toggle">Speaker</span>
          <DeviceSelect kind="audiooutput" value={chosen.audiooutput} onChange={pick("audiooutput")} refresh={permitted} />
        </div>
      </div>
      <div className="effects">
        <EffectsFields value={effects} onChange={setEffects} />
        <div className="effects-credit">
          Effects by <a href="https://github.com/sb2702/longpipe" target="_blank" rel="noreferrer">Longpipe</a>
        </div>
      </div>
      {error && <p className="error">{error}</p>}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!name.trim()) return;
          localStorage.setItem("moqit-name", name.trim());
          const devices: Devices = { ...chosen, ...opened, audiooutput: chosen.audiooutput };
          for (const [k, id] of Object.entries(devices)) if (id) saveDevice(k as MediaDeviceKind, id);
          saveEffects(effects);
          saveQuality(quality);
          onJoin({ name: name.trim(), camera, microphone, devices, effects, quality });
        }}
      >
        <input aria-label="your name" placeholder="Your name" value={name} onChange={(e) => setName(e.target.value)} autoFocus />
        <button className="primary" type="submit" disabled={!name.trim()}>
          Join meeting
        </button>
      </form>
    </main>
  );
}

function InRoom({ roomName, choices }: { roomName: string; choices: Choices }) {
  const [room] = useState(() => new Room());
  const [error, setError] = useState("");
  const [state, setState] = useState<ConnectionState>(ConnectionState.Connecting);
  const [chatOpen, setChatOpen] = useState(false);

  useEffect(() => {
    let cancelled = false;
    room.on("connectionStateChanged", setState);
    room.on("activeDeviceChanged", saveDevice);
    const onPageHide = () => {
      const id = room.state === ConnectionState.Connected ? room.localParticipant.identity : "";
      if (id) sessionStorage.setItem(REJOIN_KEY, JSON.stringify({ room: roomName, suffix: id.slice(id.lastIndexOf("-") + 1) }));
    };
    window.addEventListener("pagehide", onPageHide);
    (async () => {
      // Before connecting this only records the choice; tracks open on it.
      for (const [kind, id] of Object.entries(choices.devices)) {
        if (id) await room.switchActiveDevice(kind as MediaDeviceKind, id).catch(() => {});
      }
      const details = await fetchConnectionDetails(roomName, choices.name);
      if (cancelled) return;
      await room.connect(details.url, details.token, details.certHash ? { serverCertificateHashes: [hexBytes(details.certHash)] } : {});
      if (cancelled) return room.disconnect();
      void room.startAudio().catch(() => {});
      await room.localParticipant.setCameraResolution(choices.quality.send);
      if (choices.quality.receive !== "auto") await room.setVideoQuality(choices.quality.receive);
      // Effects go on before the tracks are published, so nothing goes out unprocessed.
      const fx = videoEffects(choices.effects);
      if (hasVideoEffects(fx)) await room.localParticipant.setProcessor(TrackSource.CAMERA, new CameraEffects(fx));
      if (choices.effects.noiseRemoval) await room.localParticipant.setProcessor(TrackSource.MICROPHONE, new NoiseRemoval());
      if (choices.microphone) await room.localParticipant.setMicrophoneEnabled(true).catch((e) => setError(`Microphone: ${e.message}`));
      if (choices.camera) await room.localParticipant.setCameraEnabled(true).catch((e) => setError(`Camera: ${e.message}`));
    })().catch((e) => setError(String(e?.message ?? e)));
    return () => {
      cancelled = true;
      room.off("connectionStateChanged", setState);
      room.off("activeDeviceChanged", saveDevice);
      window.removeEventListener("pagehide", onPageHide);
      void room.disconnect();
    };
  }, [room, roomName, choices]);

  const leave = () => {
    void room.disconnect();
    navigate("/");
  };

  if (error && state !== ConnectionState.Connected) {
    return (
      <main className="prejoin">
        <p className="error">Could not join: {error}</p>
        <button onClick={() => navigate("/")}>Back</button>
      </main>
    );
  }
  return (
    <RoomProvider room={room}>
      <MeetingView roomName={roomName} state={state} error={error} chatOpen={chatOpen} setChatOpen={setChatOpen} leave={leave} initialQuality={choices.quality} />
    </RoomProvider>
  );
}

function MeetingView({ roomName, state, error, chatOpen, setChatOpen, leave, initialQuality }: {
  roomName: string; state: ConnectionState; error: string; chatOpen: boolean;
  setChatOpen: (f: (o: boolean) => boolean) => void; leave: () => void; initialQuality: QualityChoices;
}) {
  const [quality, setQuality] = useState(initialQuality);
  // Chat lives as long as the meeting, not the panel, so nothing is missed
  // while the panel is closed.
  const chat = useChat();
  const [seen, setSeen] = useState(0);
  useEffect(() => {
    if (chatOpen) setSeen(chat.messages.length);
  }, [chatOpen, chat.messages.length]);
  const unread = chatOpen ? 0 : chat.messages.filter((m) => !m.local).length - chat.messages.slice(0, seen).filter((m) => !m.local).length;
  return (
      <div className={`meeting${chatOpen ? " with-chat" : ""}`}>
        <header>
          <span className="room-code">{roomName}</span>
          <button className="link" onClick={() => void navigator.clipboard?.writeText(location.origin + location.pathname)}>
            Copy link
          </button>
          <button className="link" onClick={() => void navigator.clipboard?.writeText(`${location.origin}/watch/${roomName}`)}>
            Copy watch link
          </button>
          <ParticipantCount />
          <ViewerCount />
          <Presenting />
          {quality.showStats && <MeetingStats />}
          {state !== ConnectionState.Connected && <span className="status">{state}…</span>}
          {error && <span className="error">{error}</span>}
          <StartAudio />
        </header>
        <section className="stage">
          <VideoStatsContext.Provider value={quality.showStats}>
            <ConferenceLayout />
          </VideoStatsContext.Provider>
        </section>
        {chatOpen && (
          <aside>
            <Chat chat={chat} />
          </aside>
        )}
        <footer>
          <ControlBar onLeave={leave} onToggleChat={() => setChatOpen((o) => !o)} unread={unread}>
            <EffectsMenu />
            <QualityMenu value={quality} onChange={setQuality} />
          </ControlBar>
        </footer>
      </div>
  );
}

function Presenting() {
  const shares = useScreenShares();
  if (shares.length === 0) return null;
  const names = shares.map((p) => (p.isLocal ? "You" : p.name || p.identity));
  return (
    <span className="presenting" data-presenters={shares.length}>
      🖥 {names.join(", ")} {names.length === 1 && names[0] === "You" ? "are" : names.length === 1 ? "is" : "are"} presenting
    </span>
  );
}

/** Read-only viewers, when there are any. */
export function ViewerCount() {
  const n = useViewerCount();
  if (n === 0) return null;
  return <span className="viewers" data-viewers={n}>{n} watching</span>;
}

export function ParticipantCount() {
  const n = useParticipants().length;
  return <span className="count" data-count={n}>{n} {n === 1 ? "participant" : "participants"}</span>;
}

