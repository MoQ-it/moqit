import { StrictMode, useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { Home } from "./Home.tsx";
import { Meeting } from "./Meeting.tsx";
import { Watch } from "./Watch.tsx";
import "./styles.css";

/** Minimal router: "/", "/r/<room>" and "/watch/<room>". */
export function navigate(path: string) {
  history.pushState(null, "", path);
  dispatchEvent(new PopStateEvent("popstate"));
}

function App() {
  const [path, setPath] = useState(location.pathname);
  useEffect(() => {
    const on = () => setPath(location.pathname);
    addEventListener("popstate", on);
    return () => removeEventListener("popstate", on);
  }, []);
  const m = path.match(/^\/r\/([a-z0-9-]{1,64})\/?$/);
  if (m) return <Meeting key={m[1]} room={m[1]} />;
  const w = path.match(/^\/watch\/([a-z0-9-]{1,64})\/?$/);
  return w ? <Watch key={w[1]} room={w[1]} /> : <Home />;
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
