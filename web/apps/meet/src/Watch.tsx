import { Room } from "@moqit/client";
import { ConferenceLayout, RoomProvider, Scrubber, StartAudio, useRoom, useTimeline } from "@moqit/react";
import { useEffect, useState } from "react";
import { navigate } from "./main.tsx";
import { type ConnectionDetails, hexBytes, ParticipantCount, ViewerCount } from "./Meeting.tsx";

async function fetchViewerDetails(room: string): Promise<ConnectionDetails> {
  const res = await fetch("/api/token", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ room, viewer: true }) });
  if (!res.ok) throw new Error(await res.text());
  return res.json();
}

/**
 * Watches a meeting read-only. Until it starts, and whenever the connection
 * drops, it tries again with a fresh token, which may route to another node.
 */
export function Watch({ room: roomName }: { room: string }) {
  const [room, setRoom] = useState<Room>();
  const [status, setStatus] = useState("Connecting…");

  useEffect(() => {
    let cancelled = false;
    let current: Room | undefined;
    (async () => {
      for (let attempt = 0; !cancelled; attempt++) {
        const r = new Room();
        current = r;
        try {
          const d = await fetchViewerDetails(roomName);
          await r.connect(d.url, d.token, { ...(d.certHash ? { serverCertificateHashes: [hexBytes(d.certHash)] } : {}), timeoutMs: 15000 });
          if (cancelled) return void r.disconnect();
          attempt = 0;
          void r.startAudio().catch(() => {});
          setRoom(r);
          await new Promise((resolve) => r.once("disconnected", resolve)); // next() would time out
          if (cancelled) return;
          setRoom(undefined);
          setStatus("Reconnecting…");
        } catch {
          void r.disconnect();
          setStatus("Waiting for the meeting to start…");
        }
        const delay = Math.min(10000, 1000 * 2 ** attempt);
        await new Promise((resolve) => setTimeout(resolve, delay / 2 + (Math.random() * delay) / 2));
      }
    })();
    return () => {
      cancelled = true;
      void current?.disconnect();
    };
  }, [roomName]);

  const leave = () => navigate("/");
  return (
    <div className="meeting watch">
      <header>
        <span className="room-code">{roomName}</span>
        <span className="badge">Watching</span>
        {room ? (
          <RoomProvider room={room}>
            <ParticipantCount />
            <ViewerCount />
            <StartAudio />
          </RoomProvider>
        ) : (
          <span className="status">{status}</span>
        )}
        <button className="leave" onClick={leave}>
          Leave
        </button>
      </header>
      <section className="stage">
        {room && (
          <RoomProvider room={room}>
            <ConferenceLayout />
          </RoomProvider>
        )}
      </section>
      {room && (
        <RoomProvider room={room}>
          <RewindBar />
        </RoomProvider>
      )}
    </div>
  );
}

/** The rewind bar, for meetings that keep history; ← and → step 10 seconds. */
function RewindBar() {
  const room = useRoom();
  const tl = useTimeline();
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null;
      if (!room.timeline.available || el?.tagName === "INPUT" || el?.tagName === "TEXTAREA") return;
      const behind = room.timeline.behind;
      if (e.key === "ArrowLeft") void room.seek(behind + 10).catch(() => {});
      else if (e.key === "ArrowRight") void (behind - 10 < 2 ? room.goLive() : room.seek(behind - 10)).catch(() => {});
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [room]);
  if (!tl.available) return null;
  return (
    <footer>
      <Scrubber />
    </footer>
  );
}
