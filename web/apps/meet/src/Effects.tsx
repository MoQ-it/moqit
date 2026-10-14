import { TrackSource } from "@moqit/client";
import { useLocalParticipant } from "@moqit/react";
import { useEffect, useRef, useState } from "react";
import { MenuButton } from "./MenuButton.tsx";
import { backgrounds, CameraEffects, hasVideoEffects, NO_EFFECTS, NoiseRemoval, type Background, type VideoEffects } from "./effects.ts";

export interface EffectChoices extends VideoEffects {
  noiseRemoval: boolean;
}

const EFFECTS_KEY = "moqit-effects";

export function savedEffects(): EffectChoices {
  try {
    return { ...NO_EFFECTS, noiseRemoval: false, ...JSON.parse(localStorage.getItem(EFFECTS_KEY) ?? "{}") };
  } catch {
    return { ...NO_EFFECTS, noiseRemoval: false };
  }
}

export function saveEffects(e: EffectChoices) {
  localStorage.setItem(EFFECTS_KEY, JSON.stringify(e));
}

export const videoEffects = ({ background, touchup, reframe }: EffectChoices): VideoEffects => ({ background, touchup, reframe });

/** Background, touch-up, auto-frame and noise-removal fields. */
export function EffectsFields({ value, onChange, disabled }: { value: EffectChoices; onChange: (e: EffectChoices) => void; disabled?: boolean }) {
  const set = (patch: Partial<EffectChoices>) => onChange({ ...value, ...patch });
  return (
    <div className="effects-fields">
      <label className="moqit-device-select">
        <span>Background</span>
        <select aria-label="background" value={value.background} disabled={disabled} onChange={(e) => set({ background: e.target.value as Background })}>
          {backgrounds.map((b) => <option key={b.id} value={b.id}>{b.label}</option>)}
        </select>
      </label>
      <div className="effects-checks">
        <label>
          <input type="checkbox" aria-label="touch-up" checked={value.touchup} disabled={disabled} onChange={(e) => set({ touchup: e.target.checked })} /> Touch-up
        </label>
        <label>
          <input type="checkbox" aria-label="auto-frame" checked={value.reframe} disabled={disabled} onChange={(e) => set({ reframe: e.target.checked })} /> Auto-frame
        </label>
        <label>
          <input type="checkbox" aria-label="noise removal" checked={value.noiseRemoval} disabled={disabled} onChange={(e) => set({ noiseRemoval: e.target.checked })} /> Noise removal
        </label>
      </div>
    </div>
  );
}

/**
 * The track to preview: the camera with effects applied once they are
 * running, the plain camera until then or without effects.
 */
export function usePreviewEffects(camera: MediaStreamTrack | undefined, effects: VideoEffects): { track?: MediaStreamTrack; starting: boolean; error: string } {
  const [processed, setProcessed] = useState<MediaStreamTrack>();
  const [starting, setStarting] = useState(false);
  const [error, setError] = useState("");
  const proc = useRef<CameraEffects | undefined>(undefined);
  const latest = useRef(effects);
  latest.current = effects;
  const on = hasVideoEffects(effects);

  useEffect(() => {
    if (!camera || !on) return;
    const p = new CameraEffects(latest.current);
    let cancelled = false;
    setStarting(true);
    setError("");
    p.start(camera)
      .then((t) => {
        if (cancelled) return;
        proc.current = p;
        setProcessed(t);
        void p.update(latest.current); // anything changed while it started
      })
      .catch((e) => !cancelled && setError(String(e?.message ?? e)))
      .finally(() => !cancelled && setStarting(false));
    return () => {
      cancelled = true;
      p.stop();
      proc.current = undefined;
      setProcessed(undefined);
      setStarting(false);
    };
  }, [camera, on]);

  useEffect(() => {
    void proc.current?.update(effects).catch((e) => setError(String(e?.message ?? e)));
  }, [effects.background, effects.touchup, effects.reframe]);

  return { track: on && processed ? processed : camera, starting, error };
}

/** The in-meeting effects menu: changes apply to the live camera and microphone. */
export function EffectsMenu() {
  const local = useLocalParticipant();
  const [value, setValue] = useState(savedEffects);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  if (!local) return null;

  const apply = async (next: EffectChoices) => {
    const prev = value;
    setValue(next);
    setBusy(true);
    setError("");
    try {
      const fx = videoEffects(next);
      const current = local.getProcessor(TrackSource.CAMERA);
      if (!hasVideoEffects(fx)) {
        if (current) await local.setProcessor(TrackSource.CAMERA, undefined);
      } else if (current instanceof CameraEffects) {
        await current.update(fx);
      } else {
        // Until the effects run, the camera keeps sending what it sent.
        await local.setProcessor(TrackSource.CAMERA, new CameraEffects(fx));
      }
      if (next.noiseRemoval !== prev.noiseRemoval) {
        await local.setProcessor(TrackSource.MICROPHONE, next.noiseRemoval ? new NoiseRemoval() : undefined);
      }
      saveEffects(next);
    } catch (e) {
      setError(String((e as Error)?.message ?? e));
      setValue(prev);
    } finally {
      setBusy(false);
    }
  };

  const active = hasVideoEffects(videoEffects(value)) || value.noiseRemoval;
  return (
    <MenuButton label="✨ Effects" name="effects" active={active}>
      <EffectsFields value={value} onChange={(e) => void apply(e)} disabled={busy} />
      {busy && <div className="effects-status">Applying…</div>}
      {error && <div className="moqit-device-error">{error}</div>}
      <div className="effects-credit">
        Effects by <a href="https://github.com/sb2702/longpipe" target="_blank" rel="noreferrer">Longpipe</a>
      </div>
    </MenuButton>
  );
}
