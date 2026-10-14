import { encodeMessage, MsgType } from "@moq-it/moqt";
import { afterEach, expect, test, vi } from "vitest";
import { ConnectionState, Room } from "./room.ts";

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
