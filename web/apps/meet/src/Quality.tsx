import { CameraPresets, type CodecSupport, TrackSource, type VideoCodecPreference, VideoCodecNames, type VideoQuality, type VideoResolution } from "@moqit/client";
import { useLocalParticipant, useRoom, useRoomUpdates } from "@moqit/react";
import { useEffect, useState } from "react";
import { MenuButton } from "./MenuButton.tsx";

export interface QualityChoices {
  /** Largest camera resolution sent. */
  send: VideoResolution;
  /** Layer received of others' video; "auto" fits each video's size on screen. */
  receive: VideoQuality | "auto";
  /** Codec video is sent in first; "auto" picks from what this device encodes well. */
  codec: VideoCodecPreference;
  /** Show resolution and bitrate on tiles and the totals in the header. */
  // (Renamed from "stats", which was saved as false while that was the
  // default; the old value is ignored so stats now show by default.)
  showStats: boolean;
}

const QUALITY_KEY = "moqit-quality";
const DEFAULT_QUALITY: QualityChoices = { send: "720p", receive: "auto", codec: "auto", showStats: true };

export function savedQuality(): QualityChoices {
  try {
    const { stats: _old, ...saved } = JSON.parse(localStorage.getItem(QUALITY_KEY) ?? "{}");
    return { ...DEFAULT_QUALITY, ...saved };
  } catch {
    return DEFAULT_QUALITY;
  }
}

export function saveQuality(q: QualityChoices) {
  localStorage.setItem(QUALITY_KEY, JSON.stringify(q));
}

const sendOptions: { id: VideoResolution; label: string }[] = [
  { id: "1080p", label: "1080p (Full HD)" },
  { id: "720p", label: "720p (HD)" },
  { id: "360p", label: "360p" },
  { id: "180p", label: "180p (data saver)" },
];

const receiveOptions: { id: QualityChoices["receive"]; label: string }[] = [
  { id: "auto", label: "Auto (fit to tile)" },
  { id: "high", label: "High (largest sent)" },
  { id: "medium", label: "Medium" },
  { id: "low", label: "Low" },
];

export function SendSelect({ value, onChange, disabled }: { value: VideoResolution; onChange: (r: VideoResolution) => void; disabled?: boolean }) {
  return (
    <label className="moqit-device-select">
      <span>Send video</span>
      <select aria-label="send resolution" value={value} disabled={disabled} onChange={(e) => onChange(e.target.value as VideoResolution)}>
        {sendOptions.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
      </select>
    </label>
  );
}

/**
 * Send and receive resolution for the meeting. Changes apply live: the
 * camera is not republished, and others' video switches layer.
 */
export function QualityMenu({ value, onChange }: { value: QualityChoices; onChange: (q: QualityChoices) => void }) {
  const room = useRoom();
  const local = useLocalParticipant();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [limited, setLimited] = useState("");
  const [codecs, setCodecs] = useState<(CodecSupport & { allowed: boolean; failed: boolean })[]>([]);
  const [fallback, setFallback] = useState("");
  useRoomUpdates();
  const allowedKey = room.videoCodecs.join();
  useEffect(() => {
    if (!local) return;
    void local.videoCodecs().then(setCodecs).catch(() => setCodecs([]));
  }, [local, value.send, allowedKey, fallback]);
  useEffect(() => {
    const onFallback = (codec: keyof typeof VideoCodecNames, reason: string) => setFallback(`${VideoCodecNames[codec]} stopped working here (${reason}), so video moved to another codec.`);
    room.on("videoCodecFallback", onFallback);
    return () => void room.off("videoCodecFallback", onFallback);
  }, [room]);
  if (!local) return null;
  const sending = local.videoCodec;

  // The camera may not reach the chosen size (or another tab holds it at a lower one).
  const checkCamera = (send: VideoResolution) => {
    const s = local.getTrack(TrackSource.CAMERA)?.capturedTrack.getSettings();
    setLimited(s?.height && s.width && s.height < CameraPresets[send].height ? `Your camera is capturing ${s.width}×${s.height}, so that is the most sent.` : "");
  };

  const apply = async (next: QualityChoices) => {
    onChange(next);
    saveQuality(next);
    setBusy(true);
    setError("");
    try {
      if (next.send !== value.send) {
        await local.setCameraResolution(next.send);
        checkCamera(next.send);
      }
      if (next.receive !== value.receive) await room.setVideoQuality(next.receive === "auto" ? undefined : next.receive);
      if (next.codec !== value.codec) {
        setFallback("");
        await local.setVideoCodec(next.codec);
      }
    } catch (e) {
      setError(String((e as Error)?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <MenuButton label="🎚 Quality" name="quality">
      <SendSelect value={value.send} disabled={busy} onChange={(send) => void apply({ ...value, send })} />
      <label className="moqit-device-select">
        <span>Receive video</span>
        <select aria-label="receive quality" value={value.receive} disabled={busy}
          onChange={(e) => void apply({ ...value, receive: e.target.value as QualityChoices["receive"] })}>
          {receiveOptions.map((o) => <option key={o.id} value={o.id}>{o.label}</option>)}
        </select>
      </label>
      <label className="moqit-device-select">
        <span>Send codec</span>
        <select aria-label="send codec" value={value.codec} disabled={busy}
          onChange={(e) => void apply({ ...value, codec: e.target.value as VideoCodecPreference })}>
          <option value="auto">Auto{value.codec === "auto" && sending ? ` (${VideoCodecNames[sending]})` : ""}</option>
          {codecs.filter((c) => (c.encode && c.allowed) || c.codec === value.codec).map((c) => (
            <option key={c.codec} value={c.codec} disabled={!c.encode || !c.allowed}>
              {VideoCodecNames[c.codec]}{c.hardware ? " (hardware)" : ""}{c.failed ? " (failed)" : ""}{!c.allowed ? " (not allowed here)" : ""}
            </option>
          ))}
        </select>
      </label>
      <label className="quality-stats">
        <input type="checkbox" aria-label="show stats" checked={value.showStats} onChange={(e) => void apply({ ...value, showStats: e.target.checked })} /> Show resolution and bitrate
      </label>
      {limited && <div className="quality-note">{limited}</div>}
      {fallback && <div className="quality-note">{fallback}</div>}
      {error && <div className="moqit-device-error">{error}</div>}
    </MenuButton>
  );
}
