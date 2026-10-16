import { create } from "@bufbuild/protobuf";
import { encodeMessage, MsgType } from "@moq-it/moqt";
import { afterEach, expect, test, vi } from "vitest";
import { ParticipantInfoSchema, TrackInfoSchema, TrackKind, TrackSource } from "./gen/moqit/v1/models_pb.ts";
import { RemoteAudioTrack, type RemoteVideoTrack } from "./remote.ts";
import { ConnectionState, RemoteParticipant, Room } from "./room.ts";

afterEach(() => vi.unstubAllGlobals());

const never = () => new ReadableStream<Uint8Array>();
const token = (claims: object) => `e30.${btoa(JSON.stringify(claims)).replace(/=+$/, "")}.sig`;

// A server that completes SETUP but never answers a request.
class SilentServer {
  ready = Promise.resolve();
  closed = new Promise(() => {});
  datagrams = { readable: never(), writable: new WritableStream() };
  incomingBidirectionalStreams = never();
  incomingUnidirectionalStreams = new ReadableStream({
    start(c) {
      c.enqueue(new ReadableStream({ start: (s) => s.enqueue(encodeMessage({ type: MsgType.Setup, options: [] })) }));
    },
  });
  async createUnidirectionalStream() {
    return new WritableStream();
  }
  async createBidirectionalStream() {
    return { readable: never(), writable: new WritableStream() };
  }
  close() {
    SilentServer.closes++;
  }
  static closes = 0;
}

test("connect names the step that timed out", async () => {
  vi.stubGlobal("WebTransport", SilentServer);
  const room = new Room({ disconnectOnPageLeave: false });
  const t = token({ sub: "alice", grants: { room: { room: "r1" } } });
  await expect(room.connect("https://node.test/moq", t, { timeoutMs: 50 })).rejects.toThrow("timed out announcing the participant");
  expect(room.state).toBe(ConnectionState.Disconnected);
  expect(SilentServer.closes).toBe(1);
});

test("a viewer connects without announcing itself, and cannot publish", async () => {
  vi.stubGlobal("WebTransport", SilentServer);
  const room = new Room({ disconnectOnPageLeave: false });
  const t = token({ grants: { room: { room: "r1", viewer: true } } });
  // It waits for the room state rather than announcing a participant.
  await expect(room.connect("https://node.test/moq", t, { timeoutMs: 50 })).rejects.toThrow("timed out waiting for the room state");
  expect(room.isViewer).toBe(true);
  expect(room.name).toBe("r1");
  await expect(room.localParticipant.setMicrophoneEnabled(true)).rejects.toThrow("viewers cannot publish");
  await expect(room.localParticipant.publishData(new Uint8Array([1]))).rejects.toThrow("viewers cannot publish");
  await expect(room.localParticipant.setMetadata("x")).rejects.toThrow("viewers cannot publish");
});

test("a participant's volume applies to its audio, only that source, and survives a reconnect", () => {
  const room = new Room({ disconnectOnPageLeave: false, autoSubscribe: false });
  const mic = create(TrackInfoSchema, { sid: "t1", kind: TrackKind.AUDIO, source: TrackSource.MICROPHONE });
  const screenAudio = create(TrackInfoSchema, { sid: "t2", kind: TrackKind.AUDIO, source: TrackSource.SCREEN_AUDIO });
  const camera = create(TrackInfoSchema, { sid: "t3", kind: TrackKind.VIDEO, source: TrackSource.CAMERA });
  const info = create(ParticipantInfoSchema, { identity: "bob", sid: "s1", tracks: [mic, screenAudio, camera] });
  const bob = new RemoteParticipant(info, room);
  bob.syncTracks(info);
  const micTrack = new RemoteAudioTrack(mic, room.audio);
  const screenTrack = new RemoteAudioTrack(screenAudio, room.audio);
  bob.getTrackPublication(TrackSource.MICROPHONE)!.track = micTrack;
  bob.getTrackPublication(TrackSource.SCREEN_AUDIO)!.track = screenTrack;
  bob.getTrackPublication(TrackSource.CAMERA)!.track = { kind: TrackKind.VIDEO } as RemoteVideoTrack; // no setVolume
  const setMic = vi.spyOn(micTrack, "setVolume");
  const setScreen = vi.spyOn(screenTrack, "setVolume");
  const changed: [string, TrackSource, number][] = [];
  room.on("participantVolumeChanged", (p, source, v) => changed.push([p.identity, source, v]));

  expect(bob.getVolume()).toBe(1);
  bob.setVolume(0);
  bob.setVolume(0); // unchanged: no event
  expect(setMic).toHaveBeenCalledWith(0);
  expect(setScreen).not.toHaveBeenCalled();
  expect(bob.getVolume(TrackSource.SCREEN_AUDIO)).toBe(1);
  expect(changed).toEqual([["bob", TrackSource.MICROPHONE, 0]]);
  expect(() => bob.setVolume(-1)).toThrow(RangeError);
  expect(() => bob.setVolume(0.5, TrackSource.CAMERA)).not.toThrow();

  // The same identity in a new session is still muted.
  const again = new RemoteParticipant(create(ParticipantInfoSchema, { identity: "bob", sid: "s2" }), room);
  expect(again.getVolume()).toBe(0);
  expect(new RemoteParticipant(create(ParticipantInfoSchema, { identity: "carol", sid: "s3" }), room).getVolume()).toBe(1);
  again.setVolume(1);
  expect(bob.getVolume()).toBe(1);
});
