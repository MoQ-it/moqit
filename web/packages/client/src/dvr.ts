// Rewind for viewers: the nodes record each participant's media, and a
// viewer fetches it back (MoQ FETCH) and plays it paced by a room-wide
// playhead in server time. See docs/protocol.md, "Rewind".

import { fromBinary } from "@bufbuild/protobuf";
import type { FetchObject, Session } from "@moq-it/moqt";
import { DvrIndexSchema, type DvrEpoch, TrackSource } from "./gen/moqit/v1/models_pb.ts";
import { sleep, TrackName } from "./util.ts";

/** A Group start in the archive, with server time (µs) and capture time (µs, 0 if unknown). */
export interface Checkpoint {
  track: string;
  group: number;
  at: number;
  loc: number;
}

/** Archive Group IDs are epoch << 32 | publisher Group. */
export const epochOf = (group: number): number => Math.floor(group / 2 ** 32);

/** Room-wide playback position in server time (unix µs), running at 1x unless paused. */
export class Playhead {
  private base = 0;
  private at = 0;
  paused = false;
  constructor(private now: () => number = () => performance.now()) {}

  /** Moves to serverUs; a paused playhead stays paused there. */
  start(serverUs: number) {
    this.base = serverUs;
    this.at = this.now();
  }

  position(): number {
    return this.paused ? this.base : this.base + (this.now() - this.at) * 1000;
  }

  pause() {
    if (this.paused) return;
    this.base = this.position();
    this.paused = true;
  }

  resume() {
    if (!this.paused) return;
    this.paused = false;
    this.at = this.now();
  }
}

/** The checkpoint to start playing at t from: the last at or before t, else the first after it. */
export function pickStart(cps: Checkpoint[], t: number): Checkpoint | undefined {
  let best: Checkpoint | undefined;
  for (const c of cps) {
    if (c.at <= t) best = c;
    else return best ?? c;
  }
  return best;
}

/**
 * The last Group of a fetch chunk starting at group (time from): just
 * before the first checkpoint of the same track and epoch at least span
 * µs later; undefined when the index has none yet (fetch to the end).
 */
export function chunkEnd(cps: Checkpoint[], track: string, group: number, from: number, span: number): number | undefined {
  const epoch = epochOf(group);
  for (const c of cps) {
    if (c.track === track && epochOf(c.group) === epoch && c.group > group && c.at >= from + span) return c.group - 1;
  }
  return undefined;
}

/** Whether an archived track name carries a publication's source. */
export function trackOfSource(name: string, source: TrackSource): boolean {
  switch (source) {
    case TrackSource.MICROPHONE:
      return name === TrackName.Mic;
    case TrackSource.SCREEN_AUDIO:
      return name === TrackName.ScreenAudio;
    case TrackSource.CAMERA:
      return name === "camera" || name.startsWith("camera-");
    case TrackSource.SCREEN:
      return name !== TrackName.ScreenAudio && (name === "screen" || name.startsWith("screen-"));
  }
  return false;
}

const MINUTE = 60e6;

/** A participant's rewind index, fetched a minute at a time from its "dvr" track. */
export class ParticipantIndex {
  /** Server time (µs) of the oldest and newest stored media, as last seen. */
  earliest = 0;
  latest = 0;
  readonly epochs = new Map<number, DvrEpoch>();
  private minutes = new Map<number, Checkpoint[]>();
  private loading = new Map<number, Promise<void>>();

  constructor(
    private session: Session,
    private ns: string[],
  ) {}

  /** Fetches the minutes covering [from, to] (server µs) not loaded yet; with fresh, also those that may still be growing. */
  async load(from: number, to: number, fresh = false): Promise<void> {
    const first = Math.floor(from / MINUTE);
    const last = Math.floor(to / MINUTE);
    const waits: Promise<void>[] = [];
    let run = -1; // first minute of a run of minutes to fetch
    for (let m = first; m <= last + 1; m++) {
      const pending = this.loading.get(m);
      const want = m <= last && (!pending || (fresh && m >= Math.floor(this.latest / MINUTE)));
      if (want && run < 0) run = m;
      if (!want && run >= 0) {
        const p = this.fetchMinutes(run, m - 1);
        for (let k = run; k < m; k++) this.loading.set(k, p);
        waits.push(p);
        run = -1;
      }
      if (pending && m <= last && !want) waits.push(pending);
    }
    await Promise.all(waits);
  }

  private async fetchMinutes(from: number, to: number): Promise<void> {
    try {
      const f = await this.session.fetch({ namespace: this.ns, name: TrackName.Dvr }, { locationFilter: { type: 4, startGroup: from, endGroupDelta: to - from } });
      for await (const o of f) {
        if (o.range) continue;
        const idx = fromBinary(DvrIndexSchema, o.payload);
        this.earliest = Number(idx.earliest);
        this.latest = Math.max(this.latest, Number(idx.latest));
        for (const e of idx.epochs) this.epochs.set(e.epoch, e);
        this.minutes.set(o.group, idx.checkpoints.map((c) => ({ track: c.track, group: Number(c.group), at: Number(c.at), loc: Number(c.loc) })));
      }
    } catch {
      // No history (yet): the participant joined later, or nothing is stored.
      for (let m = from; m <= to; m++) this.loading.delete(m);
    }
  }

  /** Loaded checkpoints of a source's tracks, ordered by time. */
  checkpoints(source: TrackSource): Checkpoint[] {
    const out: Checkpoint[] = [];
    for (const m of [...this.minutes.keys()].sort((a, b) => a - b)) {
      for (const c of this.minutes.get(m)!) if (trackOfSource(c.track, source)) out.push(c);
    }
    return out;
  }
}

/** Where an archive player sends what it fetched. */
export interface ArchiveSink {
  /** A new epoch starts; decoders reconfigure for it. */
  epoch(e: DvrEpoch | undefined): void;
  /** One object, at capture time ts (µs, publisher clock), key when it starts a Group. */
  object(payload: Uint8Array, ts: number, key: boolean): void;
}

const CHUNK_US = 10e6; // media per FETCH
const AHEAD_US = 20e6; // fetched ahead of the playhead at most
const LEAD_US = 100e3; // objects reach the decoder this far ahead of the playhead

/** Plays one publication's archive from a time, following the playhead. */
export class ArchivePlayer {
  private stopped = false;
  private fetch?: { cancel(): void };

  constructor(
    private session: Session,
    private ns: string[],
    private source: TrackSource,
    private index: ParticipantIndex,
    private playhead: Playhead,
    private sink: ArchiveSink,
    private audio: boolean,
  ) {}

  stop() {
    this.stopped = true;
    this.fetch?.cancel();
  }

  /**
   * Waits until the playhead is within lead of t (server µs). While paused
   * no audio passes, and video only up to the paused position, so a seek
   * while paused shows its frame.
   */
  private async until(t: number, lead: number): Promise<boolean> {
    for (;;) {
      if (this.stopped) return false;
      const paused = this.playhead.paused;
      const wait = (t - (paused ? 0 : lead) - this.playhead.position()) / 1000;
      if (wait <= 0 && !(paused && this.audio)) return true;
      // Short sleeps while paused, so playing resumes promptly.
      await sleep(paused ? 50 : Math.min(wait, 1000));
    }
  }

  async run(): Promise<void> {
    const t = this.playhead.position();
    let start: Checkpoint | undefined;
    for (;;) {
      await this.index.load(t - MINUTE, t + MINUTE);
      start = pickStart(this.index.checkpoints(this.source), t);
      if (start || this.stopped) break;
      await sleep(2000); // nothing recorded around t (yet)
      if (this.stopped) return;
    }
    if (!start) return;
    let track = start.track;
    let next = start.group;
    let last = start.at; // server time of the latest object handed on
    let offset = start.loc ? start.at - start.loc : 0;
    let epoch = -1;
    let needKey = true;
    while (!this.stopped) {
      if (!(await this.until(last, AHEAD_US))) return;
      await this.index.load(last, last + 2 * MINUTE, true);
      const cps = this.index.checkpoints(this.source);
      const byGroup = new Map(cps.filter((c) => c.track === track).map((c) => [c.group, c]));
      const end = chunkEnd(cps, track, next, last, CHUNK_US);
      let got = 0;
      try {
        const f = await this.session.fetch(
          { namespace: this.ns, name: track },
          { locationFilter: end === undefined ? { type: 2, startGroup: next } : { type: 4, startGroup: next, endGroupDelta: end - next } },
        );
        this.fetch = f;
        for await (const o of f) {
          if (this.stopped) return f.cancel();
          if (o.range) continue;
          got++;
          next = o.group + 1;
          const cp = byGroup.get(o.group);
          if (cp?.loc) offset = cp.at - cp.loc;
          if (epochOf(o.group) !== epoch) {
            epoch = epochOf(o.group);
            this.sink.epoch(this.index.epochs.get(epoch));
            needKey = true;
          }
          const ts = locOf(o);
          const at = ts ? ts + offset : (cp?.at ?? last);
          if (needKey && o.object !== 0) continue;
          if (!(await this.until(at, LEAD_US))) return f.cancel();
          last = Math.max(last, at);
          // Audio from before the playhead (a seek lands on the Group
          // before it) is skipped; video decodes from the keyframe.
          if (this.audio && at < this.playhead.position() - 50e3) continue;
          needKey = false;
          this.sink.object(o.payload, ts, o.object === 0);
        }
      } catch {
        // swept, or the node went away: look again below
      }
      if (got > 0) continue;
      // Nothing more on this track from next: move on to the source's next
      // recording, or wait at the live edge of the archive.
      const later = cps.find((c) => c.at > last && !(c.track === track && c.group < next));
      if (later && (later.track !== track || later.group > next)) {
        track = later.track;
        next = later.group;
        last = Math.max(last, later.at);
        needKey = true;
        continue;
      }
      await sleep(1000);
    }
  }
}

function locOf(o: FetchObject): number {
  return o.properties?.find((p) => p.type === 0x10)?.int ?? 0;
}

/** Where a viewer is in a room's history. */
export interface Timeline {
  /** The room keeps history and this is a viewer: seek works. */
  available: boolean;
  /** Playing live rather than rewound. */
  live: boolean;
  /** Seconds behind live of what is shown; 0 when live. */
  behind: number;
  /** Seconds of history there is to rewind to, as far as known. */
  history: number;
  /** Paused (Room.pause): what is shown stays, and behind grows. */
  paused: boolean;
}

/** A track Rewind plays history into. */
export interface RewindTrack {
  readonly kind: number;
  enterArchive(): void;
  leaveArchive(): Promise<void>;
  archiveEpoch(arg?: string | number): void;
  archiveObject(payload: Uint8Array, ts: number, key: boolean): void;
}

/** @internal A room's rewind state: the playhead and one player per track. */
export class Rewind {
  readonly playhead = new Playhead();
  active = false;
  private indexes = new Map<string, ParticipantIndex>();
  // A track waiting for resume after pauseLive has no player yet.
  private players = new Map<RewindTrack, { start: () => ArchivePlayer; player?: ArchivePlayer }>();
  private offset?: number; // server µs minus local µs, from the newest stored media
  private earliest = 0;

  constructor(
    private session: Session,
    private ns: (identity: string) => string[],
  ) {}

  /** Server time now (µs), as estimated. */
  serverNow(): number {
    return Date.now() * 1000 + (this.offset ?? 0);
  }

  private index(identity: string): ParticipantIndex {
    let ix = this.indexes.get(identity);
    if (!ix) {
      ix = new ParticipantIndex(this.session, this.ns(identity));
      this.indexes.set(identity, ix);
    }
    return ix;
  }

  /** Reads how much history there is from each participant's newest index minutes. */
  async refresh(identities: string[]): Promise<void> {
    const now = this.serverNow();
    const span = this.offset === undefined ? 10 * MINUTE : 2 * MINUTE; // the local clock may be off
    await Promise.all(identities.map((id) => this.index(id).load(now - span, now + span, true)));
    let earliest = 0;
    let latest = 0;
    for (const id of identities) {
      const ix = this.indexes.get(id);
      if (!ix?.latest) continue;
      earliest = earliest ? Math.min(earliest, ix.earliest) : ix.earliest;
      latest = Math.max(latest, ix.latest);
    }
    if (!latest) return;
    this.earliest = earliest;
    // Stored media trails real time by a little; the newest seen is the best estimate.
    const off = latest - Date.now() * 1000;
    this.offset = this.offset === undefined ? off : Math.max(off, this.offset - 50e3);
  }

  timeline(available: boolean): Timeline {
    const now = this.serverNow();
    const history = this.earliest ? Math.max(0, (now - this.earliest) / 1e6) : 0;
    const behind = this.active ? Math.max(0, (now - this.playhead.position()) / 1e6) : 0;
    return { available, live: !this.active, behind, history, paused: this.active && this.playhead.paused };
  }

  /** Plays every track from secondsBehind before live. */
  seek(secondsBehind: number, tracks: { identity: string; source: TrackSource; track: RewindTrack }[]) {
    this.moveTo(secondsBehind);
    for (const { identity, source, track } of tracks) this.play(identity, source, track);
  }

  /**
   * Freezes live tracks as they are shown, paused secondsBehind before
   * live; their history starts playing from there on resume.
   */
  pauseLive(secondsBehind: number, tracks: { identity: string; source: TrackSource; track: RewindTrack }[]) {
    this.moveTo(secondsBehind);
    this.playhead.pause();
    for (const { identity, source, track } of tracks) this.play(identity, source, track, false);
  }

  /** Plays on from a pause, starting the players pauseLive held back. */
  resume() {
    this.playhead.resume();
    for (const p of this.players.values()) this.run(p);
  }

  private moveTo(secondsBehind: number) {
    const now = this.serverNow();
    const t = this.earliest ? Math.max(now - secondsBehind * 1e6, this.earliest) : now - secondsBehind * 1e6;
    this.stopAll();
    this.active = true;
    this.playhead.start(t);
  }

  /** Starts one track's history at the playhead, or with start false only takes it off live. */
  play(identity: string, source: TrackSource, track: RewindTrack, start = true) {
    this.players.get(track)?.player?.stop();
    track.enterArchive();
    const audio = source === TrackSource.MICROPHONE || source === TrackSource.SCREEN_AUDIO;
    const entry = {
      start: () => new ArchivePlayer(this.session, this.ns(identity), source, this.index(identity), this.playhead, {
        epoch: (e) => track.archiveEpoch(audio ? e?.channels : e?.codec),
        object: (payload, ts, key) => track.archiveObject(payload, ts, key),
      }, audio),
    };
    this.players.set(track, entry);
    if (start) this.run(entry);
  }

  private run(entry: { start: () => ArchivePlayer; player?: ArchivePlayer }) {
    if (entry.player) return;
    entry.player = entry.start();
    void entry.player.run().catch((e) => console.warn("moqit: rewind", e));
  }

  /** Stops playing a track's history (it was unsubscribed). */
  drop(track: RewindTrack) {
    this.players.get(track)?.player?.stop();
    this.players.delete(track);
  }

  /** Returns every track to live. */
  async goLive(): Promise<void> {
    this.active = false;
    this.playhead.resume();
    const tracks = [...this.players.keys()];
    this.stopAll();
    await Promise.all(tracks.map((t) => t.leaveArchive()));
  }

  private stopAll() {
    for (const p of this.players.values()) p.player?.stop();
    this.players.clear();
  }

  close() {
    this.active = false;
    this.stopAll();
  }
}
