import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// `npm run dev` serves the app with hot reload; /api goes to `go run ./cmd/meet -dev`.
export default defineConfig({
  plugins: [react()],
  server: { proxy: { "/api": "http://localhost:8080" } },
  // Longpipe (effects) is a ~660 kB chunk, loaded only when effects are turned on.
  build: { target: "es2022", chunkSizeWarningLimit: 700 },
});
