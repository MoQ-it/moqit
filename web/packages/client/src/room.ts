// Room, participants and their events.

import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { type OutgoingSubgroup, RequestError, Session, type SubscribeRequest, type Subscription, type TrackWriter } from "@moq-it/moqt";
import {
  type ParticipantInfo,
  ParticipantState,
  type Room as RoomInfo,
  TrackInfoSchema,
  TrackKind,
  TrackSource,
} from "./gen/moqit/v1/models_pb.ts";
import {
  type ClientSignal,
  ClientSignalSchema,
  type DataPacket,
  DataPacketSchema,
  InboxMessageSchema,
  RequestResult,
  RoomStateUpdateSchema,
} from "./gen/moqit/v1/signal_pb.ts";
import { Rewind, type Timeline } from "./dvr.ts";
import { LocalAudioTrack, type LocalTrack, LocalVideoTrack, type TrackProcessor } from "./local.ts";
import { allocate, type Feedback, SendController, Suspensions } from "./sendcontrol.ts";
import { AudioOutput, type RemoteAudioTrack, RemoteTrackPublication, type RemoteTrack } from "./remote.ts";
import {
  CameraPresets,
  eachObject,
  Emitter,
  inboxNamespace,
  jwtClaims,
  participantNamespace,
  pickVideoCodec,
  retryDelay,
  ScreenRenditions,
  Serial,
  serverNamespace,
  sleep,
  TrackName,
  type VideoQuality,
  type VideoResolution,
} from "./util.ts";

export const ConnectionState = {
  Disconnected: "disconnected",
  Connecting: "connecting",
  Connected: "connected",
} as const;
export type ConnectionState = (typeof ConnectionState)[keyof typeof ConnectionState];

export type RoomEvents = {
  connected: [];
  disconnected: [Error | undefined];
  connectionStateChanged: [ConnectionState];
  participantConnected: [RemoteParticipant];
  participantDisconnected: [RemoteParticipant];
  participantMetadataChanged: [string, Participant];
  participantNameChanged: [string, Participant];
  participantAttributesChanged: [Record<string, string>, Participant];
  trackPublished: [RemoteTrackPublication, RemoteParticipant];
  trackUnpublished: [RemoteTrackPublication, RemoteParticipant];
  trackSubscribed: [RemoteTrack, RemoteTrackPublication, RemoteParticipant];
  trackUnsubscribed: [RemoteTrack, RemoteTrackPublication, RemoteParticipant];
  trackMuted: [RemoteTrackPublication, RemoteParticipant];
  trackUnmuted: [RemoteTrackPublication, RemoteParticipant];
  localTrackPublished: [LocalTrack];
  localTrackUnpublished: [LocalTrack];
  /** The server (RoomService.MuteTrack) muted or unmuted one of our tracks. */
  localTrackMutedByServer: [LocalTrack, boolean];
  dataReceived: [Uint8Array, RemoteParticipant | undefined, string];
  /** The server's active speakers changed; loudest first. */
  activeSpeakersChanged: [Participant[]];
  roomMetadataChanged: [string];
  /** The number of read-only viewers of the room changed. */
  viewerCountChanged: [number];
  /** A microphone, camera or speaker was chosen with switchActiveDevice. */
  activeDeviceChanged: [MediaDeviceKind, string];
  /** A viewer's place in the room's history, or how much there is, changed. */
  timelineChanged: [Timeline];
  /** Video is sent below the quality asked for because the uplink can't carry it, or no longer is. */
  sendLimitedChanged: [boolean];
};

export interface RoomOptions {
  /** Subscribe to every remote track as it is published (default true). */
  autoSubscribe?: boolean;
  /** Rendition subscribed for video until the element's size is known (default "medium"). */
  videoQuality?: VideoQuality;
  /**
   * Choose each video's rendition from the size of its element, pause
   * forwarding while it is not visible, and step down when the relay drops
   * groups (default true).
   */
  adaptiveStream?: boolean;
  /**
   * Leave the room when the page is closed, reloaded or navigated away
   * (default true), so others do not see a frozen participant until the
   * connection times out.
   */
  disconnectOnPageLeave?: boolean;
  /**
   * Fit the video sent to the uplink (default true): lower bitrates, then
   * frame rates, then stop the largest layers while the network can't
   * carry them, using the server's reports on how media arrives.
   */
  adaptiveSend?: boolean;
}

export interface ConnectOptions {
  /** SHA-256 hashes of a development server's self-signed certificate. */
  serverCertificateHashes?: Uint8Array[];
  /** How long each step of joining may take: the handshake, SETUP, and the rest (default 10000ms). */
  timeoutMs?: number;
}

/** Common participant state. */
export abstract class Participant {
  info: ParticipantInfo;
  constructor(info: ParticipantInfo) {
    this.info = info;
  }
  get identity(): string {
    return this.info.identity;
  }
  get sid(): string {
    return this.info.sid;
  }
  get name(): string {
    return this.info.name;
  }
  get metadata(): string {
    return this.info.metadata;
  }
  get attributes(): Record<string, string> {
    return this.info.attributes;
  }
  abstract get isLocal(): boolean;
}

export class Room extends Emitter<RoomEvents> {
  state: ConnectionState = ConnectionState.Disconnected;
  name = "";
  info?: RoomInfo;
  localParticipant!: LocalParticipant;
  readonly remoteParticipants = new Map<string, RemoteParticipant>();
  /** Participants the server currently hears speaking, loudest first. */
  activeSpeakers: Participant[] = [];
  readonly audio = new AudioOutput();
  readonly options: Required<RoomOptions>;
  /**
   * Connected with a viewer token: the room is watched read-only, and
   * nothing can be published or sent.
   */
  isViewer = false;

  /** @internal */ session?: Session;
  private signal?: { sg: OutgoingSubgroup; next: number };
  private signalQueue: ClientSignal[] = [];
  private signalSerial = new Serial();
  private pending = new Map<bigint, { resolve: () => void; reject: (e: Error) => void }>();
  private nextRequest = 1n;
  private dataWriters = new Map<string, { w: TrackWriter; sg?: OutgoingSubgroup; next: number; group: number }>();
  private roomSub?: Subscription;
  private signalReady: () => void = () => {};
  private inboxSub?: Subscription;
  private devices = new Map<MediaDeviceKind, string>();
  private receiveQuality?: VideoQuality;
  private rewind?: Rewind;
  private rewindTimer?: ReturnType<typeof setInterval>;

  constructor(options: RoomOptions = {}) {
    super();
    this.options = { autoSubscribe: true, videoQuality: "medium", adaptiveStream: true, disconnectOnPageLeave: true, adaptiveSend: true, ...options };
  }

  /** Read-only viewers of the room, on every node; updated every few seconds. */
  get numViewers(): number {
    return this.info?.numViewers ?? 0;
  }

  /**
   * Joins the room the token grants, at a moqit node's WebTransport URL.
   * With a viewer token it watches the room instead, waiting for it to
   * start if nobody has joined yet.
   */
  async connect(url: string, token: string, opts: ConnectOptions = {}): Promise<void> {
    if (this.state !== ConnectionState.Disconnected) throw new Error("already connected");
    const claims = jwtClaims(token);
    this.isViewer = claims.grants?.room?.viewer === true;
    const identity = claims.sub || (this.isViewer ? "viewer" : "");
    this.name = claims.grants?.room?.room ?? "";
    if (!identity || !this.name) throw new Error("token has no identity or room");
    this.setState(ConnectionState.Connecting);
    const timeoutMs = opts.timeoutMs ?? 10000;
    let step = "";
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      const signalReady = new Promise<void>((resolve) => (this.signalReady = resolve));
      const connected = new Promise<void>((resolve) => this.once("connected", () => resolve()));
      this.session = await Session.connect({
        url,
        serverCertificateHashes: opts.serverCertificateHashes,
        authorizationToken: new TextEncoder().encode(token),
        handler: { onSubscribe: (req) => void this.handleSubscribe(req) },
        readyTimeoutMs: timeoutMs,
        setupTimeoutMs: timeoutMs,
      });
      void this.session.closed.then((e) => this.handleClosed(e));
      this.localParticipant = new LocalParticipant(create_info(identity), this);
      const join = async (session: Session) => {
        if (this.isViewer) {
          this.signalReady();
          void this.followRoom(session);
        } else {
          step = "announcing the participant";
          await session.publishNamespace(participantNamespace(this.name, identity));
          step = "subscribing to the inbox";
          this.inboxSub = await session.subscribe({ namespace: inboxNamespace(this.name, identity), name: TrackName.Inbox });
          void this.readInbox(this.inboxSub);
          step = "subscribing to the room";
          void this.followRoom(session, await this.subscribeRoom(session));
        }
        step = "waiting for the room state";
        await Promise.all([connected, signalReady]);
      };
      await Promise.race([
        join(this.session),
        new Promise<never>((_, reject) => {
          timer = setTimeout(() => reject(new Error(`timed out ${step}`)), timeoutMs);
        }),
      ]);
      if (this.options.disconnectOnPageLeave && typeof window !== "undefined") window.addEventListener("pagehide", this.onPageLeave);
      if (this.isViewer) {
        const session = this.session;
        this.rewind = new Rewind(session, (id) => participantNamespace(this.name, id));
        void this.refreshTimeline();
        this.rewindTimer = setInterval(() => void this.refreshTimeline(), 5000);
      }
    } catch (e) {
      this.session?.close();
      this.setState(ConnectionState.Disconnected);
      throw e;
    } finally {
      clearTimeout(timer);
    }
  }

  async disconnect(): Promise<void> {
    for (const t of this.localParticipant?.tracks.values() ?? []) t.stop();
    this.session?.close();
    this.handleClosed(undefined);
  }

  /**
   * Media devices of a kind. Labels are empty until the page has had
   * camera or microphone permission.
   */
  static async getLocalDevices(kind?: MediaDeviceKind): Promise<MediaDeviceInfo[]> {
    const all = await navigator.mediaDevices.enumerateDevices();
    return all.filter((d) => (!kind || d.kind === kind) && d.deviceId !== "");
  }

  /** The microphone, camera or speaker in use, or chosen for when it is enabled. */
  getActiveDevice(kind: MediaDeviceKind): string | undefined {
    if (kind === "audiooutput") return this.devices.get(kind);
    const track = this.localParticipant?.getTrack(kind === "audioinput" ? TrackSource.MICROPHONE : TrackSource.CAMERA);
    return track?.capturedTrack.getSettings().deviceId || this.devices.get(kind);
  }

  /**
   * Chooses the microphone, camera or speaker. A published microphone or
   * camera moves to the new device without being republished. Before
   * connecting, or while a track is off, the choice applies when it is
   * enabled.
   */
  async switchActiveDevice(kind: MediaDeviceKind, deviceId: string): Promise<void> {
    if (kind === "audiooutput") await this.audio.setSinkId(deviceId);
    else await this.localParticipant?.switchDevice(kind, deviceId);
    this.devices.set(kind, deviceId);
    this.emit("activeDeviceChanged", kind, deviceId);
  }

  /** @internal */
  chosenDevice(kind: MediaDeviceKind): string | undefined {
    return this.devices.get(kind);
  }

  /** Media bitrates now, in bits per second: everything sent, and everything received. */
  getStats(): { sendBitrate: number; receiveBitrate: number; sendLimited: boolean; sendTarget: number } {
    let sendBitrate = 0;
    for (const t of this.localParticipant?.tracks.values() ?? []) {
      sendBitrate += t instanceof LocalVideoTrack ? t.sending().reduce((sum, r) => sum + r.bitrate, 0) : t.bitrate;
    }
    let receiveBitrate = 0;
    for (const p of this.remoteParticipants.values()) for (const pub of p.trackPublications.values()) receiveBitrate += pub.track?.bitrate ?? 0;
    const lp = this.localParticipant;
    return { sendBitrate, receiveBitrate, sendLimited: lp?.sendLimited ?? false, sendTarget: lp?.sendTarget ?? 0 };
  }

  /** The receive quality set with setVideoQuality; undefined is automatic. */
  get videoQuality(): VideoQuality | undefined {
    return this.receiveQuality;
  }

  /**
   * Receive quality for every remote video, now and as it is published: a
   * fixed simulcast layer ("high" is the largest sent), or undefined to
   * choose by the size each video is shown at (adaptive stream).
   */
  async setVideoQuality(quality: VideoQuality | undefined): Promise<void> {
    this.receiveQuality = quality;
    const pubs = [...this.remoteParticipants.values()].flatMap((p) => [...p.trackPublications.values()]);
    await Promise.all(pubs.filter((pub) => pub.kind === TrackKind.VIDEO).map((pub) => pub.setVideoQuality(quality)));
  }

  /**
   * A viewer's place in the room's history. Rooms created with a
   * dvr_window keep that much history, which viewers can rewind.
   */
  get timeline(): Timeline {
    const available = this.isViewer && (this.info?.dvrWindow ?? 0) > 0;
    return this.rewind?.timeline(available) ?? { available, live: true, behind: 0, history: 0 };
  }

  /**
   * Viewers only: plays every participant from secondsBehindLive ago (as
   * far back as the history goes) and keeps playing from there; 0 goes
   * live.
   */
  async seek(secondsBehindLive: number): Promise<void> {
    if (!this.rewind || !this.timeline.available) throw new Error("rewind is for viewers of rooms that keep history");
    if (secondsBehindLive <= 0) return this.goLive();
    const tracks = [...this.remoteParticipants.values()].flatMap((p) =>
      [...p.trackPublications.values()].filter((pub) => pub.track).map((pub) => ({ identity: p.identity, source: pub.source, track: pub.track! })));
    this.rewind.seek(secondsBehindLive, tracks);
    this.emit("timelineChanged", this.timeline);
  }

  /** Viewers only: returns from history to the live room. */
  async goLive(): Promise<void> {
    if (!this.rewind?.active) return;
    await this.rewind.goLive();
    this.emit("timelineChanged", this.timeline);
  }

  private async refreshTimeline() {
    if (!this.rewind || !this.timeline.available) return;
    await this.rewind.refresh([...this.remoteParticipants.keys()]).catch(() => {});
    this.emit("timelineChanged", this.timeline);
  }

  /** Starts audio playback; call from a user gesture if the browser blocked autoplay. */
  startAudio(): Promise<void> {
    return this.audio.start();
  }

  private setState(s: ConnectionState) {
    if (this.state === s) return;
    this.state = s;
    this.emit("connectionStateChanged", s);
  }

  private onPageLeave = () => void this.disconnect();

  private handleClosed(err: Error | undefined) {
    if (this.state === ConnectionState.Disconnected) return;
    clearInterval(this.rewindTimer);
    this.localParticipant?.stopSendControl();
    this.rewind?.close();
    if (typeof window !== "undefined") window.removeEventListener("pagehide", this.onPageLeave);
    this.setState(ConnectionState.Disconnected);
    for (const p of this.remoteParticipants.values()) p.close();
    this.remoteParticipants.clear();
    for (const { reject } of this.pending.values()) reject(new Error("disconnected"));
    this.pending.clear();
    this.audio.close();
    this.emit("disconnected", err);
  }

  // Requests from the relay for this participant's tracks.
  private async handleSubscribe(req: SubscribeRequest) {
    const own = participantNamespace(this.name, this.localParticipant?.identity ?? "");
    if (req.track.namespace.join("/") !== own.join("/")) return req.reject(0x10, "not here");
    const name = req.track.name;
    if (name === TrackName.Signal) {
      const w = await req.accept();
      this.signal = { sg: w.openSubgroup(0, 0), next: 0 };
      const queued = this.signalQueue;
      this.signalQueue = [];
      for (const cs of queued) void this.writeSignal(cs);
      this.signalReady();
      return;
    }
    if (name === TrackName.Data || name === TrackName.DataLossy) {
      const w = await req.accept();
      this.dataWriters.set(name, { w, next: 0, group: 0 });
      w.signal.addEventListener("abort", () => this.dataWriters.delete(name));
      return;
    }
    const track = this.localParticipant?.trackServing(name);
    if (!track) return req.reject(0x10, "not published");
    await track.serve(req);
  }

  /** @internal Sends a ClientSignal and resolves when the server accepts it. */
  request(signal: ClientSignal["signal"]): Promise<void> {
    if (this.isViewer) return Promise.reject(new Error(VIEWER_ERROR));
    const id = this.nextRequest++;
    const cs = create(ClientSignalSchema, { requestId: id, signal });
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      if (this.signal) void this.writeSignal(cs);
      else this.signalQueue.push(cs);
    });
  }

  private writeSignal(cs: ClientSignal) {
    return this.signalSerial.run(async () => {
      const s = this.signal!;
      await s.sg.writeObject({ id: s.next++, status: 0, payload: toBinary(ClientSignalSchema, cs) });
    });
  }

  /** @internal Broadcasts on this participant's data track. */
  async writeData(pkt: DataPacket, reliable: boolean) {
    const d = this.dataWriters.get(reliable ? TrackName.Data : TrackName.DataLossy);
    if (!d) return; // nobody subscribed
    const payload = toBinary(DataPacketSchema, pkt);
    if (!reliable) {
      await d.w.writeDatagram({ group: d.group++, object: 0, status: 0, payload });
      return;
    }
    d.sg ??= d.w.openSubgroup(d.group++, 0);
    await d.sg.writeObject({ id: d.next++, status: 0, payload });
  }

  private async readInbox(sub: Subscription) {
    for await (const g of sub) {
      await eachObject(g, (o) => {
        const m = fromBinary(InboxMessageSchema, o.payload);
        if (m.message.case === "response") {
          const r = m.message.value;
          const p = this.pending.get(r.requestId);
          this.pending.delete(r.requestId);
          if (r.result === RequestResult.OK) p?.resolve();
          else p?.reject(new Error(r.message || RequestResult[r.result]));
        } else if (m.message.case === "data") {
          const d = m.message.value;
          this.emit("dataReceived", d.payload, this.remoteParticipants.get(d.fromIdentity), d.topic);
        } else if (m.message.case === "muteTrack") {
          const { trackSid, muted } = m.message.value;
          const name = this.localParticipant.info.tracks.find((t) => t.sid === trackSid)?.name;
          const track = name ? this.localParticipant.tracks.get(name) : undefined;
          if (name && track && track.muted !== muted) {
            void this.localParticipant.setTrackMuted(name, muted).then(() => this.emit("localTrackMutedByServer", track, muted));
          }
        } else if (m.message.case === "sendFeedback") {
          this.localParticipant.sendFeedback(m.message.value);
        }
      });
    }
  }

  private subscribeRoom(session: Session): Promise<Subscription> {
    return session.subscribe({ namespace: serverNamespace(this.name), name: TrackName.Room }, { locationFilter: { type: 1, startGroup: 1 } });
  }

  // Follows the room track while connected, subscribing again when it
  // ends or can't be had yet: a viewer's node may fetch it from a node
  // that later closes the room, or that hasn't opened it yet.
  private async followRoom(session: Session, first?: Subscription) {
    const live = () => this.session === session && this.state !== ConnectionState.Disconnected;
    for (let attempt = 0; live(); ) {
      const sub = first ??
        (await this.subscribeRoom(session).catch((e) => {
          if (!(e instanceof RequestError) && live()) console.warn("moqit: room track", e);
          return undefined;
        }));
      first = undefined;
      if (sub) {
        this.roomSub = sub;
        await this.readRoom(sub).catch((e) => console.warn("moqit: room track", e));
        attempt = 0;
      }
      if (!live()) return;
      await sleep(retryDelay(attempt++));
    }
  }

  private setInfo(info: RoomInfo | undefined) {
    const old = this.info;
    this.info = info;
    if (old?.metadata !== undefined && old.metadata !== info?.metadata) this.emit("roomMetadataChanged", info?.metadata ?? "");
    if ((old?.numViewers ?? 0) !== (info?.numViewers ?? 0)) this.emit("viewerCountChanged", info?.numViewers ?? 0);
  }

  private async readRoom(sub: Subscription) {
    for await (const g of sub) {
      await eachObject(g, (o) => {
        const u = fromBinary(RoomStateUpdateSchema, o.payload);
        switch (u.update.case) {
          case "snapshot": {
            this.setInfo(u.update.value.room);
            const present = new Set<string>();
            for (const p of u.update.value.participants) {
              present.add(p.identity);
              this.applyParticipant(p);
            }
            for (const [id, p] of this.remoteParticipants) if (!present.has(id)) this.removeParticipant(p);
            this.setSpeakers(u.update.value.speakers.map((sp) => sp.identity));
            if (this.state === ConnectionState.Connecting) {
              this.setState(ConnectionState.Connected);
              this.emit("connected");
            }
            break;
          }
          case "participantUpdated":
            this.applyParticipant(u.update.value);
            break;
          case "participantLeft": {
            const p = this.remoteParticipants.get(u.update.value.identity);
            if (p && p.sid === u.update.value.sid) this.removeParticipant(p);
            break;
          }
          case "speakersChanged":
            this.setSpeakers(u.update.value.speakers.map((sp) => sp.identity));
            break;
          case "roomUpdated":
            this.setInfo(u.update.value);
            break;
        }
      });
    }
  }

  private setSpeakers(identities: string[]) {
    const next = identities
      .map((id) => (id === this.localParticipant.identity && !this.isViewer ? this.localParticipant : this.remoteParticipants.get(id)))
      .filter((p): p is LocalParticipant | RemoteParticipant => p !== undefined);
    if (next.length === this.activeSpeakers.length && next.every((p, i) => p === this.activeSpeakers[i])) return;
    this.activeSpeakers = next;
    this.emit("activeSpeakersChanged", next);
  }

  private applyParticipant(info: ParticipantInfo) {
    if (info.identity === this.localParticipant.identity && !this.isViewer) {
      this.localParticipant.info = info;
      return;
    }
    let p = this.remoteParticipants.get(info.identity);
    if (p && p.sid !== info.sid) {
      this.removeParticipant(p); // rejoined with a new session
      p = undefined;
    }
    if (!p) {
      p = new RemoteParticipant(info, this);
      this.remoteParticipants.set(info.identity, p);
      this.emit("participantConnected", p);
    } else {
      const old = p.info;
      p.info = info;
      if (old.metadata !== info.metadata) this.emit("participantMetadataChanged", old.metadata, p);
      if (old.name !== info.name) this.emit("participantNameChanged", info.name, p);
      if (JSON.stringify(old.attributes) !== JSON.stringify(info.attributes)) this.emit("participantAttributesChanged", info.attributes, p);
    }
    p.syncTracks(info);
    if (info.state === ParticipantState.ACTIVE) p.activate();
  }

  private removeParticipant(p: RemoteParticipant) {
    this.remoteParticipants.delete(p.identity);
    p.close();
    this.emit("participantDisconnected", p);
  }

  /** @internal */
  emitTrack<K extends "trackPublished" | "trackUnpublished" | "trackMuted" | "trackUnmuted">(k: K, pub: RemoteTrackPublication, p: RemoteParticipant) {
    this.emit(k, pub, p);
  }
  /** @internal */
  emitSubscribed(sub: boolean, t: RemoteTrack, pub: RemoteTrackPublication, p: RemoteParticipant) {
    // Tracks subscribed while rewound join the rewind.
    if (sub && this.rewind?.active) this.rewind.play(p.identity, pub.source, t);
    if (!sub) this.rewind?.drop(t);
    this.emit(sub ? "trackSubscribed" : "trackUnsubscribed", t, pub, p);
  }
  /** @internal */
  emitData(payload: Uint8Array, p: RemoteParticipant, topic: string) {
    this.emit("dataReceived", payload, p, topic);
  }
  /** @internal */
  emitSendLimited(limited: boolean) {
    this.emit("sendLimitedChanged", limited);
  }
  /** @internal */
  emitLocal(published: boolean, t: LocalTrack) {
    this.emit(published ? "localTrackPublished" : "localTrackUnpublished", t);
  }
}

const VIEWER_ERROR = "viewers cannot publish";

function create_info(identity: string): ParticipantInfo {
  return { identity, sid: "", name: "", state: ParticipantState.JOINED, metadata: "", attributes: {}, tracks: [], joinedAt: 0n, nodeId: "" } as unknown as ParticipantInfo;
}

export class RemoteParticipant extends Participant {
  readonly trackPublications = new Map<string, RemoteTrackPublication>();
  private dataSub?: Subscription;
  private active = false;
  private closed = false;

  constructor(info: ParticipantInfo, private room: Room) {
    super(info);
  }

  get isLocal(): boolean {
    return false;
  }

  /** @internal Diffs the published tracks and auto-subscribes. */
  syncTracks(info: ParticipantInfo) {
    const seen = new Set<string>();
    for (const t of info.tracks) {
      seen.add(t.sid);
      let pub = this.trackPublications.get(t.sid);
      if (pub) {
        pub.update(t);
        continue;
      }
      pub = new RemoteTrackPublication(
        t,
        {
          session: this.room.session!, room: this.room.name, identity: this.identity, audio: this.room.audio,
          adaptive: this.room.options.adaptiveStream, audioClock: (video) => this.audioClock(video),
        },
        this.room.options.videoQuality,
      );
      const p = pub;
      pub.on("muted", () => this.room.emitTrack("trackMuted", p, this));
      pub.on("unmuted", () => this.room.emitTrack("trackUnmuted", p, this));
      pub.on("subscribed", (tr) => this.room.emitSubscribed(true, tr, p, this));
      pub.on("unsubscribed", (tr) => this.room.emitSubscribed(false, tr, p, this));
      this.trackPublications.set(t.sid, pub);
      if (t.kind === TrackKind.VIDEO && this.room.videoQuality) void pub.setVideoQuality(this.room.videoQuality);
      this.room.emitTrack("trackPublished", pub, this);
      if (this.room.options.autoSubscribe) void pub.subscribeRetrying();
    }
    for (const [sid, pub] of this.trackPublications) {
      if (!seen.has(sid)) {
        this.trackPublications.delete(sid);
        pub.close();
        this.room.emitTrack("trackUnpublished", pub, this);
      }
    }
  }

  /** @internal Subscribes to the participant's broadcast data once it is active. */
  activate() {
    if (this.active || this.closed) return;
    this.active = true;
    void (async () => {
      try {
        this.dataSub = await this.room.session!.subscribe({ namespace: participantNamespace(this.room.name, this.identity), name: TrackName.Data });
        if (this.closed) return this.dataSub.unsubscribe();
        for await (const g of this.dataSub) {
          await eachObject(g, (o) => {
            const pkt = fromBinary(DataPacketSchema, o.payload);
            this.room.emitData(pkt.payload, this, pkt.topic);
          });
        }
      } catch (e) {
        if (!(e instanceof RequestError)) console.warn("moqit: data track", e);
      }
    })();
  }

  /** Lip sync: camera video follows the microphone, screen video the screen audio. */
  private audioClock(video: TrackSource): number | undefined {
    const audio = this.getTrackPublication(video === TrackSource.SCREEN ? TrackSource.SCREEN_AUDIO : TrackSource.MICROPHONE);
    if (!audio || audio.muted) return undefined;
    return (audio.track as RemoteAudioTrack | undefined)?.clock();
  }

  getTrackPublication(source: TrackSource): RemoteTrackPublication | undefined {
    return [...this.trackPublications.values()].find((p) => p.source === source);
  }

  /** @internal */
  close() {
    this.closed = true;
    this.dataSub?.unsubscribe();
    for (const pub of this.trackPublications.values()) pub.close();
  }
}

export interface DataOptions {
  /** Ordered and retransmitted (default), or best-effort datagrams. */
  reliable?: boolean;
  topic?: string;
  /** Deliver only to these identities, through their inboxes. */
  destinationIdentities?: string[];
}

const MIC_CONSTRAINTS: MediaTrackConstraints = { echoCancellation: true, noiseSuppression: true, autoGainControl: true };

export class LocalParticipant extends Participant {
  /** Published tracks by name ("camera", "microphone", "screen", "screen-audio"). */
  readonly tracks = new Map<string, LocalTrack>();
  private videoCodec?: Promise<string>;
  private cameraRes: VideoResolution = "720p";
  private processors = new Map<TrackSource, TrackProcessor | undefined>();
  private send = new SendController();
  private suspensions = new Suspensions();
  private sendTimer?: ReturnType<typeof setInterval>;
  private calmSince = 0;
  /** Whether video is sent below the quality asked for, because the uplink can't carry it. */
  sendLimited = false;

  constructor(info: ParticipantInfo, private room: Room) {
    super(info);
    if (room.options.adaptiveSend) this.sendTimer = setInterval(() => this.controlSend(), 250);
  }

  /** The video bitrate send control allows, in bits per second; 0 before any video is sent. */
  get sendTarget(): number {
    return this.send.target;
  }

  /** @internal The server's report on how our media arrives. */
  sendFeedback(fb: Feedback) {
    this.send.feedback(fb);
  }

  /** @internal */
  stopSendControl() {
    clearInterval(this.sendTimer);
  }

  // Fits the video to the uplink: shares the controller's target among
  // the renditions being encoded and suspends those it can't carry.
  private controlSend() {
    const video = [...this.tracks.values()].filter((t): t is LocalVideoTrack => t instanceof LocalVideoTrack);
    const layers = video.flatMap((t) => t.layers());
    const suspended = this.suspensions.suspended;
    const running = layers.filter((l) => l.encoding && !suspended.has(l.layer.rendition)).map((l) => l.layer);
    const candidates = layers.filter((l) => suspended.has(l.layer.rendition)).map((l) => l.layer);
    if (!running.length && !candidates.length) return;
    let queueDelayMs = 0;
    let drops = 0;
    let sendRate = 0;
    for (const t of video) {
      for (const r of t.sending()) {
        queueDelayMs = Math.max(queueDelayMs, r.sendDelayMs);
        drops += r.droppedFrames;
        sendRate += r.bitrate;
      }
    }
    const audioRate = [...this.tracks.values()].reduce((n, t) => n + (t instanceof LocalAudioTrack ? t.bitrate : 0), 0);
    const maxTarget = [...running, ...candidates].reduce((n, l) => n + l.spec, 0);
    const signals = { queueDelayMs, drops, sendRate, audioRate, maxTarget };
    this.send.tick(signals);
    if (!this.send.calm(signals)) this.calmSince = performance.now();
    const alloc = allocate(this.send.target, running);
    for (const t of video) t.applyAllocation(alloc);
    if (this.suspensions.update(alloc, running, this.send.target, this.calmSince, candidates)) {
      for (const t of video) t.setSuspended(suspended);
      void this.syncTracks().catch(() => {});
    }
    const limited = suspended.size > 0 || running.some((l) => {
      const a = alloc.get(l.rendition);
      return !a || a.halfRate || a.bitrate < 0.95 * l.spec;
    });
    if (limited !== this.sendLimited) {
      this.sendLimited = limited;
      this.room.emitSendLimited(limited);
    }
  }

  get isLocal(): boolean {
    return true;
  }

  /** Captures and sends the camera at the send resolution (setCameraResolution). */
  async setCameraEnabled(enabled: boolean, constraints: MediaTrackConstraints = this.cameraConstraints()): Promise<LocalVideoTrack | undefined> {
    if (!enabled) return void (await this.unpublish("camera"));
    this.checkCanPublish();
    if (this.tracks.has("camera")) return this.tracks.get("camera") as LocalVideoTrack;
    const stream = await this.open("videoinput", constraints);
    this.videoCodec ??= pickVideoCodec();
    const track = new LocalVideoTrack(stream.getVideoTracks()[0], TrackSource.CAMERA, await this.videoCodec, CameraPresets[this.cameraRes].renditions);
    await this.applyProcessor(track);
    return this.publish(track) as Promise<LocalVideoTrack>;
  }

  async setMicrophoneEnabled(enabled: boolean, constraints: MediaTrackConstraints = MIC_CONSTRAINTS): Promise<LocalAudioTrack | undefined> {
    if (!enabled) return void (await this.unpublish("microphone"));
    this.checkCanPublish();
    if (this.tracks.has("microphone")) return this.tracks.get("microphone") as LocalAudioTrack;
    const stream = await this.open("audioinput", constraints);
    const track = new LocalAudioTrack(stream.getAudioTracks()[0], TrackSource.MICROPHONE);
    await this.applyProcessor(track);
    return this.publish(track) as Promise<LocalAudioTrack>;
  }

  /** The largest camera resolution sent. */
  get cameraResolution(): VideoResolution {
    return this.cameraRes;
  }

  /**
   * Sets the largest camera resolution sent (default 720p), now and when
   * the camera is next enabled. A live camera changes without being
   * republished: the capture size and simulcast layers follow, and
   * subscribers move to a layer that is still sent.
   */
  async setCameraResolution(resolution: VideoResolution): Promise<void> {
    this.cameraRes = resolution;
    const track = this.getTrack(TrackSource.CAMERA) as LocalVideoTrack | undefined;
    if (!track) return;
    const { width, height, frameRate } = CameraPresets[resolution];
    // Best effort: a camera that cannot do the size (or is shared with
    // another tab at a lower one) keeps its own, and renditions never upscale.
    await track.capturedTrack.applyConstraints({ width, height, frameRate }).catch((e) => console.warn("moqit: camera resolution", e));
    track.setRenditions(CameraPresets[resolution].renditions);
    await this.syncTracks();
  }

  private cameraConstraints(): MediaTrackConstraints {
    const { width, height, frameRate } = CameraPresets[this.cameraRes];
    return { width, height, frameRate };
  }

  /**
   * Processes the camera or microphone before it is sent, e.g. background
   * blur, now and whenever it is enabled again; undefined removes it. A
   * new track is published only once the processor's output is ready.
   */
  async setProcessor(source: TrackSource.CAMERA | TrackSource.MICROPHONE, processor: TrackProcessor | undefined): Promise<void> {
    await this.getTrack(source)?.setProcessor(processor); // on failure, nothing changes
    this.processors.set(source, processor);
  }

  getProcessor(source: TrackSource): TrackProcessor | undefined {
    return this.processors.get(source);
  }

  private async applyProcessor(track: LocalTrack) {
    try {
      await track.setProcessor(this.processors.get(track.source));
    } catch (e) {
      track.stop(); // never fall back to sending the unprocessed track
      throw e;
    }
  }

  // Opens the device chosen with Room.switchActiveDevice, or the default
  // one if it was unplugged. (Chrome ignores an "ideal" deviceId, so the
  // choice is exact, with a fallback.)
  private async open(kind: MediaDeviceKind, constraints: MediaTrackConstraints): Promise<MediaStream> {
    const get = (c: MediaTrackConstraints) => navigator.mediaDevices.getUserMedia(kind === "audioinput" ? { audio: c } : { video: c });
    const id = this.room.chosenDevice(kind);
    if (!id || constraints.deviceId !== undefined) return get(constraints);
    try {
      return await get({ ...constraints, deviceId: { exact: id } });
    } catch (e) {
      if (!["OverconstrainedError", "NotFoundError"].includes((e as DOMException).name)) throw e;
      return get(constraints);
    }
  }

  /** @internal Moves the published microphone or camera to another device. */
  async switchDevice(kind: MediaDeviceKind, deviceId: string): Promise<void> {
    const audio = kind === "audioinput";
    const track = this.getTrack(audio ? TrackSource.MICROPHONE : TrackSource.CAMERA);
    if (!track || track.capturedTrack.getSettings().deviceId === deviceId) return;
    const constraints = { ...(audio ? MIC_CONSTRAINTS : this.cameraConstraints()), deviceId: { exact: deviceId } };
    const open = () => navigator.mediaDevices.getUserMedia(audio ? { audio: constraints } : { video: constraints });
    let stream: MediaStream;
    try {
      stream = await open();
    } catch (e) {
      // Some systems open one camera at a time: release the old one first.
      if ((e as DOMException).name !== "NotReadableError") throw e;
      track.capturedTrack.stop();
      stream = await open();
    }
    await track.replaceTrack(audio ? stream.getAudioTracks()[0] : stream.getVideoTracks()[0]);
  }

  /**
   * Shares a screen, window or tab, with its audio when the browser can
   * capture it. Stopping from the browser's own "Stop sharing" control
   * unpublishes it too.
   */
  async setScreenShareEnabled(enabled: boolean, options: DisplayMediaStreamOptions = {}): Promise<LocalVideoTrack | undefined> {
    if (!enabled) {
      const tracks = ["screen", "screen-audio"].map((n) => this.tracks.get(n)).filter((t) => t !== undefined);
      if (tracks.length === 0) return;
      for (const t of tracks) {
        this.tracks.delete(t.name);
        t.stop();
      }
      await this.syncTracks();
      for (const t of tracks) this.room.emitLocal(false, t);
      return;
    }
    if (this.tracks.has("screen")) return this.tracks.get("screen") as LocalVideoTrack;
    this.checkCanPublish();
    const video = { frameRate: 15, ...((options.video as object | undefined) ?? {}) };
    let stream: MediaStream;
    try {
      stream = await navigator.mediaDevices.getDisplayMedia({ ...options, video, audio: options.audio ?? true });
    } catch (e) {
      // Some browsers reject audio capture outright; retry with video only,
      // unless the user dismissed the picker.
      if ((e as DOMException).name === "NotAllowedError" || options.audio === true) throw e;
      stream = await navigator.mediaDevices.getDisplayMedia({ ...options, video, audio: false });
    }
    const videoTrack = stream.getVideoTracks()[0];
    videoTrack.contentHint = "detail"; // text and UI: favour sharpness over motion
    this.videoCodec ??= pickVideoCodec();
    const screen = new LocalVideoTrack(videoTrack, TrackSource.SCREEN, await this.videoCodec, ScreenRenditions);
    const audioTrack = stream.getAudioTracks()[0];
    const screenAudio = audioTrack ? new LocalAudioTrack(audioTrack, TrackSource.SCREEN_AUDIO) : undefined;
    videoTrack.addEventListener("ended", () => void this.setScreenShareEnabled(false));
    // Announce video and audio together, in one track update.
    this.tracks.set(screen.name, screen);
    if (screenAudio) this.tracks.set(screenAudio.name, screenAudio);
    try {
      await this.syncTracks();
    } catch (e) {
      this.tracks.delete(screen.name);
      if (screenAudio) this.tracks.delete(screenAudio.name);
      stream.getTracks().forEach((t) => t.stop());
      throw e;
    }
    if (screenAudio) this.room.emitLocal(true, screenAudio);
    this.room.emitLocal(true, screen);
    return screen;
  }

  /** Publishes a track: it is announced to the room and served on demand. */
  async publish(track: LocalTrack): Promise<LocalTrack> {
    this.checkCanPublish();
    this.tracks.set(track.name, track);
    try {
      await this.syncTracks();
    } catch (e) {
      this.tracks.delete(track.name);
      track.stop();
      throw e;
    }
    this.room.emitLocal(true, track);
    return track;
  }

  async unpublish(name: string): Promise<void> {
    const track = this.tracks.get(name);
    if (!track) return;
    this.tracks.delete(name);
    track.stop();
    await this.syncTracks();
    this.room.emitLocal(false, track);
  }

  async setTrackMuted(name: string, muted: boolean): Promise<void> {
    const t = this.tracks.get(name);
    if (!t) return;
    if (muted) t.mute();
    else t.unmute();
    await this.syncTracks();
  }

  /** @internal */
  trackServing(moqTrackName: string): LocalTrack | undefined {
    return [...this.tracks.values()].find((t) => t.trackNames().includes(moqTrackName));
  }

  private syncTracks(): Promise<void> {
    const tracks = [...this.tracks.values()].map((t) =>
      create(TrackInfoSchema, { name: t.name, kind: t.kind, source: t.source, muted: t.muted, codec: t.codec, renditions: t.renditions() }),
    );
    return this.room.request({ case: "setTracks", value: { $typeName: "moqit.v1.SetTracks", tracks } });
  }

  setMetadata(metadata: string): Promise<void> {
    return this.room.request({ case: "updateParticipant", value: { $typeName: "moqit.v1.UpdateParticipant", metadata, attributes: {} } });
  }

  setName(name: string): Promise<void> {
    return this.room.request({ case: "updateParticipant", value: { $typeName: "moqit.v1.UpdateParticipant", name, attributes: {} } });
  }

  /** Merges attributes; an empty value deletes a key. */
  setAttributes(attributes: Record<string, string>): Promise<void> {
    return this.room.request({ case: "updateParticipant", value: { $typeName: "moqit.v1.UpdateParticipant", attributes } });
  }

  /** Sends data to everyone in the room, or to destinationIdentities only. */
  async publishData(payload: Uint8Array, opts: DataOptions = {}): Promise<void> {
    this.checkCanPublish();
    const topic = opts.topic ?? "";
    if (opts.destinationIdentities?.length) {
      return this.room.request({ case: "sendData", value: { $typeName: "moqit.v1.SendData", destinationIdentities: opts.destinationIdentities, topic, payload } });
    }
    await this.room.writeData(create(DataPacketSchema, { topic, payload }), opts.reliable ?? true);
  }

  getTrack(source: TrackSource): LocalTrack | undefined {
    return [...this.tracks.values()].find((t) => t.source === source);
  }

  // Before any capture, so a viewer is never asked for camera or microphone access.
  private checkCanPublish() {
    if (this.room.isViewer) throw new Error(VIEWER_ERROR);
  }
}

export { TrackKind, TrackSource };
