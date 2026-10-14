import { useState } from "react";
import { navigate } from "./main.tsx";

function newCode(): string {
  const letters = "abcdefghijkmnpqrstuvwxyz";
  const pick = () => letters[crypto.getRandomValues(new Uint32Array(1))[0] % letters.length];
  const part = () => Array.from({ length: 4 }, pick).join("");
  return `${part()}-${part()}`;
}

export function Home() {
  const [code, setCode] = useState("");
  const normalized = code.trim().toLowerCase().replace(/^.*\/r\//, "").replace(/[^a-z0-9-]/g, "");
  return (
    <main className="home">
      <h1>moqit Meet</h1>
      <p className="tagline">Video meetings over Media over QUIC — no WebRTC.</p>
      <div className="home-actions">
        <button className="primary" onClick={() => navigate(`/r/${newCode()}`)}>
          New meeting
        </button>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (normalized) navigate(`/r/${normalized}`);
          }}
        >
          <input aria-label="meeting code" placeholder="Enter a code or link" value={code} onChange={(e) => setCode(e.target.value)} />
          <button type="submit" disabled={!normalized}>
            Join
          </button>
        </form>
      </div>
    </main>
  );
}
