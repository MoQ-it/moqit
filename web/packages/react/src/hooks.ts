import {
  ConnectionState,
  type LocalParticipant,
  type LocalVideoTrack,
  type Participant,
  type RemoteAudioTrack,
  type RemoteParticipant,
  type RemoteVideoTrack,
  type RoomEvents,
  Room,
  TrackSource,
} from "@moqit/client";
import { createContext, useCallback, useContext, useEffect, useMemo, useReducer, useState } from "react";

export const RoomContext = createContext<Room | undefined>(undefined);

export function useRoom(): Room {
  const room = useContext(RoomContext);
  if (!room) throw new Error("useRoom must be used inside <RoomProvider>");
  return room;
}

const roomEvents: (keyof RoomEvents)[] = [
  "connected", "disconnected", "connectionStateChanged", "participantConnected", "participantDisconnected",
  "participantMetadataChanged", "participantNameChanged", "participantAttributesChanged", "trackPublished",
  "trackUnpublished", "trackSubscribed", "trackUnsubscribed", "trackMuted", "trackUnmuted",
  "localTrackPublished", "localTrackUnpublished", "localTrackMutedByServer", "roomMetadataChanged", "activeSpeakersChanged",
  "activeDeviceChanged",
];

/** Re-renders whenever the room's state changes; read the Room directly. */
export function useRoomUpdates(room: Room = useRoom()): number {
  const [version, bump] = useReducer((n: number) => n + 1, 0);
  useEffect(() => {
    for (const e of roomEvents) room.on(e, bump as never);
    return () => {
      for (const e of roomEvents) room.off(e, bump as never);
    };
  }, [room]);
  return version;
}

export function useConnectionState(): ConnectionState {
  const room = useRoom();
  useRoomUpdates(room);
  return room.state;
}

/** Local participant first, then remote ones in join order. */
export function useParticipants(): Participant[] {
  const room = useRoom();
  const v = useRoomUpdates(room);
  return useMemo(() => {
    if (room.state !== ConnectionState.Connected) return [];
    return [room.localParticipant, ...room.remoteParticipants.values()];
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [room, v]);
}

export function useLocalParticipant(): LocalParticipant | undefined {
  const room = useRoom();
  useRoomUpdates(room);
  return room.state === ConnectionState.Connected ? room.localParticipant : undefined;
}

/** The video track of a participant for a source, local or remote. */
export function useVideoTrack(p: Participant, source: TrackSource = TrackSource.CAMERA): LocalVideoTrack | RemoteVideoTrack | undefined {
  useRoomUpdates();
  if (p.isLocal) return (p as LocalParticipant).getTrack(source) as LocalVideoTrack | undefined;
  const t = (p as RemoteParticipant).getTrackPublication(source)?.track;
  return t && "attach" in t ? (t as RemoteVideoTrack) : undefined;
}

/** A participant's audio level (0..1), sampled ten times a second. */
export function useAudioLevel(p: Participant): number {
  const [level, setLevel] = useState(0);
  useEffect(() => {
    const id = setInterval(() => {
      let l = 0;
      if (p.isLocal) {
        const t = (p as LocalParticipant).getTrack(TrackSource.MICROPHONE) as { level?: number; muted?: boolean } | undefined;
        l = t && !t.muted ? (t.level ?? 0) : 0;
      } else {
        const pub = (p as RemoteParticipant).getTrackPublication(TrackSource.MICROPHONE);
        l = pub && !pub.muted ? ((pub.track as RemoteAudioTrack | undefined)?.level ?? 0) : 0;
      }
      setLevel((old) => (Math.abs(old - l) > 0.02 ? l : old));
    }, 100);
    return () => clearInterval(id);
  }, [p]);
  return level;
}

/** Participants the server hears speaking, loudest first. */
export function useActiveSpeakers(): Participant[] {
  const room = useRoom();
  useRoomUpdates(room);
  return room.activeSpeakers;
}

/**
 * Whether a participant is speaking: from the audio this client plays (most
 * responsive), or the server's active speakers when it does not receive
 * that participant's audio. Holds briefly to avoid flicker.
 */
export function useIsSpeaking(p: Participant, threshold = 0.35): boolean {
  const room = useRoom();
  useRoomUpdates(room);
  const server = room.activeSpeakers.includes(p);
  const level = Math.max(useAudioLevel(p), server ? 1 : 0);
  const [speaking, setSpeaking] = useState(false);
  useEffect(() => {
    if (level > threshold) {
      setSpeaking(true);
      return;
    }
    const t = setTimeout(() => setSpeaking(false), 600);
    return () => clearTimeout(t);
  }, [level > threshold]);
  return speaking;
}

export interface ChatMessage {
  id: string;
  from: string;
  fromName: string;
  text: string;
  at: number;
  local: boolean;
}

const enc = new TextEncoder();
const dec = new TextDecoder();

/** Room chat over the "chat" data topic. */
export function useChat(): { messages: ChatMessage[]; send(text: string): Promise<void> } {
  const room = useRoom();
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  useEffect(() => {
    const onData = (payload: Uint8Array, from: RemoteParticipant | undefined, topic: string) => {
      if (topic !== "chat" || !from) return;
      try {
        const m = JSON.parse(dec.decode(payload)) as { id: string; text: string; at: number };
        setMessages((ms) => [...ms, { ...m, from: from.identity, fromName: from.name || from.identity, local: false }]);
      } catch {
        // not a chat message
      }
    };
    room.on("dataReceived", onData);
    return () => void room.off("dataReceived", onData);
  }, [room]);
  const send = useCallback(async (text: string) => {
    const m = { id: crypto.randomUUID(), text, at: Date.now() };
    const me = room.localParticipant;
    setMessages((ms) => [...ms, { ...m, from: me.identity, fromName: me.name || me.identity, local: true }]);
    await me.publishData(enc.encode(JSON.stringify(m)), { topic: "chat" });
  }, [room]);
  return { messages, send };
}

/** Whether audio playback is blocked until a user gesture. */
export function useAudioPlayback(): { canPlay: boolean; start(): Promise<void> } {
  const room = useRoom();
  const [canPlay, setCanPlay] = useState(room.audio.canPlay);
  useEffect(() => {
    const id = setInterval(() => setCanPlay(room.audio.canPlay), 500);
    return () => clearInterval(id);
  }, [room]);
  const start = useCallback(async () => {
    await room.startAudio();
    setCanPlay(room.audio.canPlay);
  }, [room]);
  return { canPlay, start };
}

/** Participants currently sharing a screen, local first. */
export function useScreenShares(): Participant[] {
  const room = useRoom();
  useRoomUpdates(room);
  if (room.state !== ConnectionState.Connected) return [];
  const out: Participant[] = [];
  if (room.localParticipant.getTrack(TrackSource.SCREEN)) out.push(room.localParticipant);
  for (const p of room.remoteParticipants.values()) if (p.getTrackPublication(TrackSource.SCREEN)) out.push(p);
  return out;
}

// Start from the last list seen, so a reopened picker does not flash empty.
const lastDevices = new Map<MediaDeviceKind, MediaDeviceInfo[]>();

/**
 * Media devices of a kind, kept current as devices come and go. Labels
 * appear once the page has camera or microphone permission; change
 * `refresh` (e.g. after getUserMedia succeeds) to list them again.
 */
export function useMediaDevices(kind: MediaDeviceKind, refresh?: unknown): MediaDeviceInfo[] {
  const [devices, setDevices] = useState<MediaDeviceInfo[]>(() => lastDevices.get(kind) ?? []);
  useEffect(() => {
    if (!navigator.mediaDevices) return;
    let live = true;
    const update = () =>
      void Room.getLocalDevices(kind).then((d) => {
        lastDevices.set(kind, d);
        if (live) setDevices(d);
      }, () => {});
    update();
    navigator.mediaDevices.addEventListener("devicechange", update);
    return () => {
      live = false;
      navigator.mediaDevices.removeEventListener("devicechange", update);
    };
  }, [kind, refresh]);
  return devices;
}

/** The room's microphone, camera or speaker, and a way to change it. */
export function useActiveDevice(kind: MediaDeviceKind): { deviceId: string | undefined; setActiveDevice(id: string): Promise<void> } {
  const room = useRoom();
  useRoomUpdates(room);
  const setActiveDevice = useCallback((id: string) => room.switchActiveDevice(kind, id), [room, kind]);
  return { deviceId: room.getActiveDevice(kind), setActiveDevice };
}
