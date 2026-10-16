// UI test of moqit Meet: two people join a meeting through the real UI in
// the system Chrome (fake camera and microphone) against `meet -dev`.
//
//   npm run build -w meet && npm run e2e -w meet
import { spawn, execFileSync } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { chromium } from "playwright-core";

const repo = resolve(import.meta.dirname, "../../../..");
const shots = process.env.SCREENSHOTS ?? mkdtempSync(join(tmpdir(), "meet-e2e-"));
const port = 20000 + Math.floor(Math.random() * 20000);
// MEET_URL tests an already-running deployment instead of `meet -dev`.
// For one with a locally trusted certificate (see docs/deploy.md), set
// MEET_INSECURE=1, CHROME_HOME to a home whose NSS database trusts its root,
// and CHROME_ARGS=--webtransport-developer-mode.
const base = process.env.MEET_URL ?? `http://localhost:${port}`;
const insecure = !!process.env.MEET_INSECURE;
if (insecure) process.env.NODE_TLS_REJECT_UNAUTHORIZED = "0";

function log(msg) {
  console.log(`${new Date().toISOString().slice(11, 23)} ${msg}`);
}

async function main() {
  let server;
  let serverLog = "";
  if (!process.env.MEET_URL) {
    const bin = join(mkdtempSync(join(tmpdir(), "meet-bin-")), "meet");
    execFileSync("go", ["build", "-o", bin, "./cmd/meet"], { cwd: repo, stdio: "inherit" });
    const dvr = mkdtempSync(join(tmpdir(), "meet-dvr-"));
    server = spawn(bin, ["-dev", "-addr", `127.0.0.1:${port}`, "-udp", `:${port}`, "-static", join(repo, "web/apps/meet/dist"), "-dvr-dir", dvr], { stdio: ["ignore", "ignore", "pipe"] });
    server.stderr.on("data", (d) => (serverLog += d));
  }
  try {
    await waitFor(async () => (await fetch(`${base}/healthz`).catch(() => null))?.ok, "meet to start");
    const browser = await chromium.launch({
      env: process.env.CHROME_HOME ? { ...process.env, HOME: process.env.CHROME_HOME } : undefined,
      executablePath: process.env.CHROME ?? "/usr/bin/google-chrome",
      headless: true,
      args: [
        "--use-fake-device-for-media-stream=device-count=2", "--use-fake-ui-for-media-stream", "--autoplay-policy=no-user-gesture-required",
        "--auto-select-desktop-capture-source=Entire screen", // getDisplayMedia picks the (fake) screen
        ...(process.env.CHROME_ARGS?.split(" ").filter(Boolean) ?? []),
      ],
    });
    try {
      await scenario(browser);
    } finally {
      await browser.close();
    }
    log(`PASS (screenshots in ${shots})`);
  } catch (e) {
    console.error(serverLog.split("\n").filter((l) => !l.includes("receive buffer")).slice(-20).join("\n"));
    throw e;
  } finally {
    server?.kill();
  }
}

async function waitFor(f, what, timeoutMs = 15000) {
  const end = Date.now() + timeoutMs;
  for (;;) {
    if (await f()) return;
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 100));
  }
}

async function join_(browser, url, name, viaHome) {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, ignoreHTTPSErrors: insecure });
  const page = await ctx.newPage();
  if (name === "Alice") await page.addInitScript(fakeScreen);
  if (name === "Alice") await page.addInitScript(tapOutput);
  // Bob has a setting saved by an older version (stats were off by default
  // then); stats must show regardless.
  if (name === "Bob") await page.addInitScript(() => {
    if (sessionStorage.getItem("seeded")) return;
    localStorage.setItem("moqit-quality", JSON.stringify({ send: "720p", receive: "auto", stats: false }));
    sessionStorage.setItem("seeded", "1");
  });
  page.on("pageerror", (e) => console.error(`[${name}] page error:`, e.message));
  page.on("console", (m) => m.type() === "error" && console.error(`[${name}] console:`, m.text()));
  if (viaHome) {
    await page.goto(base);
    await page.screenshot({ path: join(shots, "home.png") });
    await page.getByRole("button", { name: "New meeting" }).click();
    await page.waitForURL(/\/r\/[a-z]{4}-[a-z]{4}$/);
  } else {
    await page.goto(url);
  }
  await page.getByLabel("your name").fill(name);
  if (viaHome) {
    await page.locator(".preview video").evaluate((v) => new Promise((r) => (v.videoWidth ? r() : v.addEventListener("loadeddata", r, { once: true }))));
    // Device pickers list the fake devices by name once permission is
    // granted; pick the second camera, and see the microphone meter move.
    const cameras = page.locator("select[aria-label=camera]");
    await waitFor(async () => (await cameras.locator("option", { hasText: "fake_device_1" }).count()) === 1, "the camera list");
    await cameras.selectOption({ label: "fake_device_1" });
    await waitFor(() => page.locator(".preview video").evaluate((v) => v.srcObject?.getVideoTracks()[0]?.label === "fake_device_1"), "the preview to switch camera");
    await waitFor(async () => Number(await page.locator(".meter").getAttribute("data-level")) > 0.3, "the microphone meter to move");
    if ((await page.locator("select[aria-label=speaker] option").count()) < 2) throw new Error("no speakers listed");
    log("pre-join: camera picked, microphone meter moving");
    // Effects (Longpipe): the solid "Studio" background. The fake camera
    // shows no person, so nearly the whole frame becomes the backdrop colour,
    // which the other side can measure.
    await page.locator("select[aria-label=background]").selectOption("studio");
    await page.locator('.preview video[data-effects="on"]').waitFor({ timeout: 30000 });
    const previewFrames = await page.locator(".preview video").evaluate((v) => new Promise((resolve) => {
      let n = 0;
      const tick = () => (n++, v.requestVideoFrameCallback(tick));
      v.requestVideoFrameCallback(tick);
      setTimeout(() => resolve(n), 1500);
    }));
    if (previewFrames < 3) throw new Error(`the preview with effects is not playing: ${previewFrames} frames in 1.5s`);
    log(`pre-join: Studio background in the preview (${previewFrames} frames in 1.5s)`);
    await page.screenshot({ path: join(shots, "prejoin-effects.png") });
    await page.screenshot({ path: join(shots, "prejoin.png") });
  }
  await page.getByRole("button", { name: "Join meeting" }).click();
  await page.locator(".count[data-count]").waitFor();
  return page;
}

// Chrome's fake screen is 1280x720 whatever the flags; real screens are
// larger, which is where H.264 level limits bite. Share a 1920x1080 canvas.
function fakeScreen() {
  const orig = MediaDevices.prototype.getDisplayMedia;
  MediaDevices.prototype.getDisplayMedia = async function (constraints) {
    const s = await orig.call(this, constraints);
    const canvas = Object.assign(document.createElement("canvas"), { width: 1920, height: 1080 });
    const g = canvas.getContext("2d");
    let n = 0;
    setInterval(() => {
      g.fillStyle = `hsl(${(n++ * 7) % 360} 50% 40%)`;
      g.fillRect(0, 0, 1920, 1080);
      g.fillStyle = "#fff";
      g.font = "96px sans-serif";
      g.fillText(`1920x1080 screen, frame ${n}`, 120, 540);
    }, 66);
    for (const t of s.getVideoTracks()) {
      s.removeTrack(t);
      t.stop();
    }
    s.addTrack(canvas.captureStream(15).getVideoTracks()[0]);
    return s;
  };
}

// Taps every stream played by an <audio> element (the SDK's output), so a
// test can measure what reaches the speakers.
function tapOutput() {
  const desc = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, "srcObject");
  Object.defineProperty(HTMLMediaElement.prototype, "srcObject", {
    ...desc,
    set(s) {
      desc.set.call(this, s);
      if (!(this instanceof HTMLAudioElement) || !(s instanceof MediaStream)) return;
      const ctx = new AudioContext();
      const a = ctx.createAnalyser();
      a.fftSize = 2048;
      ctx.createMediaStreamSource(s).connect(a);
      (window.__outputs ??= []).push(a);
    },
  });
}

/** The loudest RMS level the page plays within ms; -1 without a tapped output. */
function outputPeak(page, ms = 1500) {
  return page.evaluate((ms) => new Promise((resolve) => {
    const taps = window.__outputs ?? [];
    const buf = new Float32Array(2048);
    let peak = 0;
    const id = setInterval(() => {
      for (const a of taps) {
        a.getFloatTimeDomainData(buf);
        peak = Math.max(peak, Math.sqrt(buf.reduce((n, x) => n + x * x, 0) / buf.length));
      }
    }, 40);
    setTimeout(() => (clearInterval(id), resolve(taps.length ? peak : -1)), ms);
  }), ms);
}
const AUDIBLE = 0.01;
const SILENT = 0.001;

/** Whether the page shows playing video for a participant whose identity starts with prefix. */
function videoPlaying(page, prefix) {
  return page.evaluate((prefix) => {
    const tile = [...document.querySelectorAll(".moqit-tile[data-source=camera]")].find((t) => t.dataset.identity.startsWith(prefix));
    const v = tile?.querySelector("video, canvas");
    if (!v) return false;
    return v instanceof HTMLVideoElement ? v.videoWidth > 0 && !v.paused : v.width > 0;
  }, prefix);
}

// Share of a participant's camera frame (as another page shows it) in the
// Studio backdrop colour, #3a4150; null before a frame arrives. Chrome's
// fake cameras have none of that bluish grey; with the effect, the
// segmentation keeps their dark areas as "person" and replaces the rest.
function studioShare(page, prefix) {
  return page.evaluate((prefix) => {
    const tile = [...document.querySelectorAll(".moqit-tile[data-source=camera]")].find((t) => t.dataset.identity.startsWith(prefix));
    const v = tile?.querySelector("video");
    if (!v?.videoWidth) return null;
    const g = new OffscreenCanvas(64, 36).getContext("2d");
    g.drawImage(v, 0, 0, 64, 36);
    const d = g.getImageData(0, 0, 64, 36).data;
    let n = 0;
    for (let i = 0; i < d.length; i += 4) {
      const [r, g, b] = [d[i], d[i + 1], d[i + 2]];
      if (Math.abs(r - 0x3a) < 16 && Math.abs(g - 0x41) < 16 && Math.abs(b - 0x50) < 16 && b - r > 12) n++; // bluish, not neutral grey
    }
    return n / (64 * 36);
  }, prefix);
}

const count = (page) => page.locator(".count").getAttribute("data-count").then(Number);
const tile = (page, prefix) => page.locator(`.moqit-tile[data-source=camera][data-identity^="${prefix}"]`);

/** Frames Alice's camera tile shows on a page within 1.5s. */
function framesShown(page, prefix) {
  return page.evaluate((prefix) => new Promise((resolve) => {
    const tile = [...document.querySelectorAll(".moqit-tile[data-source=camera]")].find((t) => t.dataset.identity.startsWith(prefix));
    const v = tile?.querySelector("video");
    if (!v) return resolve(-1);
    let n = 0;
    const tick = () => (n++, v.requestVideoFrameCallback(tick));
    v.requestVideoFrameCallback(tick);
    setTimeout(() => resolve(n), 1500);
  }), prefix);
}

// The viewer steps 10 seconds back with the rewind bar, keeps seeing
// video, and returns to live.
async function rewind(viewer) {
  const bar = viewer.locator(".moqit-scrubber");
  await waitFor(async () => (await bar.count()) === 1 && Number(await bar.locator("input").getAttribute("max")) >= 12, "the viewer to have 12s of history", 30000);
  await bar.locator(".moqit-back").click();
  await waitFor(async () => (await bar.getAttribute("data-live")) === "false" && Number(await bar.getAttribute("data-behind")) >= 8, "the viewer to be about 10s behind");
  // Rewound playback outlasts the SDK's default 10s event timeout.
  await new Promise((r) => setTimeout(r, 11000));
  if ((await bar.getAttribute("data-live")) !== "false") throw new Error("the viewer left the rewind by itself");
  await waitFor(async () => (await framesShown(viewer, "alice-")) >= 5, "rewound video to play");
  const behind = await bar.getAttribute("data-behind");
  await viewer.screenshot({ path: join(shots, "viewer-rewound.png") });
  // Pause holds the picture; space plays on from where it stopped.
  await bar.locator(".moqit-pause").click();
  await waitFor(async () => (await bar.getAttribute("data-paused")) === "true", "the viewer to pause");
  await new Promise((r) => setTimeout(r, 500));
  const still = await framesShown(viewer, "alice-");
  if (still > 1) throw new Error(`video kept playing while paused: ${still} frames in 1.5s`);
  const pausedAt = Number(await bar.getAttribute("data-behind"));
  await viewer.screenshot({ path: join(shots, "viewer-paused.png") });
  await viewer.keyboard.press(" ");
  await waitFor(async () => (await bar.getAttribute("data-paused")) === "false", "space to play again");
  await waitFor(async () => (await framesShown(viewer, "alice-")) >= 5, "video to play on after the pause");
  if (Number(await bar.getAttribute("data-behind")) < pausedAt) throw new Error(`play jumped ahead: ${await bar.getAttribute("data-behind")}s behind, paused at ${pausedAt}s`);
  log(`viewer paused ${pausedAt}s behind (${still} frames in 1.5s) and played on with space`);
  // ← steps 10s further back, or to the start of the history.
  const history = Number(await bar.locator("input").getAttribute("max"));
  await viewer.keyboard.press("ArrowLeft");
  await waitFor(async () => Number(await bar.getAttribute("data-behind")) >= Math.min(Number(behind) + 8, history - 3), "← to step further back");
  await bar.locator(".moqit-live").click();
  await waitFor(async () => (await bar.getAttribute("data-live")) === "true", "the viewer to be live again");
  await waitFor(async () => (await framesShown(viewer, "alice-")) >= 5, "live video to play again");
  log(`viewer rewound ${behind}s, then 10s more with ←, and went live again`);
}

async function scenario(browser) {
  const alice = await join_(browser, null, "Alice", true);
  const url = alice.url();
  log(`alice created ${url}`);
  const bob = await join_(browser, url, "Bob", false);
  await waitFor(async () => (await count(alice)) === 2 && (await count(bob)) === 2, "both to see two participants");
  log("both see two participants");

  await waitFor(() => videoPlaying(bob, "alice-"), "Bob to see Alice's video");
  await waitFor(() => videoPlaying(alice, "bob-"), "Alice to see Bob's video");
  log("video flowing both ways");

  // A viewer watches through the watch link: it sees and hears the meeting
  // but is not a participant, and the participants see one viewer.
  const viewerCtx = await browser.newContext({ viewport: { width: 1280, height: 800 }, ignoreHTTPSErrors: insecure });
  const viewer = await viewerCtx.newPage();
  viewer.on("pageerror", (e) => console.error("[viewer] page error:", e.message));
  viewer.on("console", (m) => ["warning", "error"].includes(m.type()) && console.error(`[viewer] ${m.type()}: ${m.text()}`));
  await viewer.goto(url.replace("/r/", "/watch/"));
  await waitFor(() => videoPlaying(viewer, "alice-"), "the viewer to see Alice's video");
  await waitFor(async () => (await count(viewer)) === 2, "the viewer to see two participants");
  await waitFor(async () => (await alice.locator(".viewers").getAttribute("data-viewers", { timeout: 500 }).catch(() => null)) === "1", "Alice to see one viewer");
  if ((await count(alice)) !== 2 || (await count(bob)) !== 2) throw new Error("the viewer was counted as a participant");
  await viewer.screenshot({ path: join(shots, "viewer.png") });
  if (!process.env.MEET_URL) await rewind(viewer); // meet -dev keeps an hour of history
  // ?debug shows the stats and exposes the Room to the console.
  await viewer.goto(url.replace("/r/", "/watch/") + "?debug");
  await waitFor(async () => (await tile(viewer, "alice-").locator(".moqit-stats").textContent({ timeout: 1000 }).catch(() => "")).includes("bps")
    && /\d+ (k|M)?bps/.test((await viewer.locator(".moqit-meeting-stats").textContent({ timeout: 1000 }).catch(() => "")) ?? "")
    && (await viewer.evaluate(() => window.moqitRoom?.remoteParticipants.size)) === 2, "the viewer's debug stats and window.moqitRoom");
  log(`viewer ?debug: Alice's tile "${await tile(viewer, "alice-").locator(".moqit-stats").textContent()}"`);
  await viewer.screenshot({ path: join(shots, "viewer-debug.png") });
  await viewerCtx.close();
  await waitFor(async () => (await alice.locator(".viewers").count()) === 0, "the viewer count to clear");
  log("viewer watched read-only and was counted as a viewer");
  await waitFor(async () => /\d+ (k|M)?bps/.test((await bob.locator(".moqit-meeting-stats").textContent({ timeout: 1000 }).catch(() => "")) ?? "")
    && (await tile(bob, "alice-").locator(".moqit-stats").textContent({ timeout: 1000 }).catch(() => "")).includes("bps"), "Bob to see bitrates despite an old saved setting");
  log(`stats shown: header "${await bob.locator(".moqit-meeting-stats").textContent()}", Alice's tile "${await tile(bob, "alice-").locator(".moqit-stats").textContent()}"`);

  // Chrome's fake microphone plays a beep: the speaking ring proves audio
  // is captured, sent, decoded and measured.
  await waitFor(async () => (await tile(bob, "alice-").getAttribute("class")).includes("speaking"), "Alice to show as speaking on Bob's screen");
  log("speaking indicator from decoded audio");
  await bob.screenshot({ path: join(shots, "bob-two-people.png") });

  // Alice picked the Studio background before joining: Bob gets it.
const WITH_EFFECT = 0.04;
const RAW = 0.01;
  await waitFor(async () => (await studioShare(bob, "alice-")) > WITH_EFFECT, "Bob to see Alice with the Studio background");
  log(`effects: Bob sees Alice's Studio background (${Math.round((await studioShare(bob, "alice-")) * 100)}% of the frame)`);

  // Devices: Alice joined on the camera she picked, and switches camera,
  // microphone and speaker live. Her old devices are stopped, so Bob still
  // seeing and hearing her proves the new ones are sent.
  await alice.getByRole("button", { name: "devices" }).click();
  const menu = alice.getByRole("dialog", { name: "device settings" });
  const selected = (kind) => menu.locator(`select[aria-label=${kind}]`).evaluate((s) => s.selectedOptions[0]?.textContent);
  await waitFor(async () => (await selected("camera")) === "fake_device_1", "Alice to have joined on the camera she picked", 3000);
  if (JSON.parse(await alice.evaluate(() => localStorage.getItem("moqit-devices"))).videoinput === undefined) throw new Error("camera choice not saved");
  await alice.screenshot({ path: join(shots, "alice-device-menu.png") });
  // Watch Bob's view while the camera changes under the effect: the new
  // camera must never go out unprocessed.
  const shares = [];
  let watching = true;
  const watch = (async () => {
    while (watching) {
      shares.push(await studioShare(bob, "alice-"));
      await new Promise((r) => setTimeout(r, 50));
    }
  })();
  await menu.locator("select[aria-label=camera]").selectOption({ label: "fake_device_0" });
  await menu.locator("select[aria-label=microphone]").selectOption({ label: "Fake Audio Input 2" });
  await menu.locator("select[aria-label=speaker]").selectOption({ label: "Fake Audio Output 1" });
  await waitFor(async () => (await selected("camera")) === "fake_device_0" && (await selected("microphone")) === "Fake Audio Input 2" && (await selected("speaker")) === "Fake Audio Output 1", "the menu to show the new devices");
  await new Promise((r) => setTimeout(r, 1500));
  watching = false;
  await watch;
  const leaked = shares.filter((x) => x !== null && x < RAW);
  if (leaked.length) throw new Error(`unprocessed camera reached Bob while switching: ${leaked.length} of ${shares.length} samples`);
  if (await menu.locator(".moqit-device-error").count()) throw new Error(await menu.locator(".moqit-device-error").first().textContent());
  await alice.keyboard.press("Escape");
  await menu.waitFor({ state: "detached" });
  const frames = await bob.evaluate((prefix) => new Promise((resolve) => {
    const tile = [...document.querySelectorAll(".moqit-tile[data-source=camera]")].find((t) => t.dataset.identity.startsWith(prefix));
    const v = tile?.querySelector("video");
    if (!v) return resolve(-1);
    let n = 0;
    const tick = () => (n++, v.requestVideoFrameCallback(tick));
    v.requestVideoFrameCallback(tick);
    setTimeout(() => resolve(n), 1500);
  }), "alice-");
  // Headless Chrome runs the effects on a software GPU at ~10 fps.
  if (frames >= 0 && frames < 5) throw new Error(`Alice's video stalled after switching camera: ${frames} frames in 1.5s`);
  await new Promise((r) => setTimeout(r, 1500)); // let the old microphone's speaking indicator lapse
  await waitFor(async () => (await tile(bob, "alice-").getAttribute("class")).includes("speaking"), "Bob to hear Alice's new microphone");
  log(`devices switched live: camera (effect held over ${shares.length} samples, min ${Math.min(...shares.filter((x) => x !== null)).toFixed(3)}; ${frames} frames in 1.5s on Bob's side), microphone, speaker`);

  // Effects menu: noise removal on and off, then the background off.
  await alice.getByRole("button", { name: "effects" }).click();
  const fx = alice.getByRole("dialog", { name: "effects settings" });
  const settled = async (what) => {
    await waitFor(async () => (await fx.locator(".effects-status").count()) === 0, what, 30000);
    if (await fx.locator(".moqit-device-error").count()) throw new Error(`${what}: ${await fx.locator(".moqit-device-error").textContent()}`);
  };
  await fx.getByLabel("noise removal").check();
  await settled("noise removal to start");
  await alice.screenshot({ path: join(shots, "alice-effects-menu.png") });
  await fx.getByLabel("noise removal").uncheck();
  await settled("noise removal to stop");
  await fx.locator("select[aria-label=background]").selectOption("none");
  await settled("the background to come off");
  await waitFor(async () => (await studioShare(bob, "alice-")) < RAW, "Bob to see Alice without the background");
  if (JSON.parse(await alice.evaluate(() => localStorage.getItem("moqit-effects"))).background !== "none") throw new Error("effects choice not saved");
  await alice.keyboard.press("Escape");
  await new Promise((r) => setTimeout(r, 1500));
  await waitFor(async () => (await tile(bob, "alice-").getAttribute("class")).includes("speaking"), "Bob to hear Alice after noise removal is off");
  log("effects menu: noise removal on/off, background off live");

  // Quality: Bob shows tile resolutions and receives the top layer; Alice
  // changes what she sends, live; Bob then picks layers himself.
  const received = () => tile(bob, "alice-").locator(".moqit-stats").getAttribute("data-resolution", { timeout: 1000 }).catch(() => null);
  const receives = (res, layer) => waitFor(async () => (await received()) === res && (await tile(bob, "alice-").locator(".moqit-stats").textContent()).includes(layer),
    `Bob to receive Alice at ${res} (${layer})`, 15000).catch(async (e) => {
      throw new Error(`${e.message}; got ${await tile(bob, "alice-").locator(".moqit-stats").textContent().catch(() => "nothing")}`);
    });
  const quality = async (page, label, value) => {
    await page.getByRole("button", { name: "quality" }).click();
    const panel = page.getByRole("dialog", { name: "quality settings" });
    if (label === "show stats") await panel.getByLabel(label).check();
    else await panel.locator(`select[aria-label="${label}"]`).selectOption(value);
    await waitFor(async () => !(await panel.locator("select").first().isDisabled()), `${label} to apply`);
    await page.keyboard.press("Escape");
  };
  // Chrome cannot raise a camera's capture size while another page holds
  // it, so Alice goes back to the camera Bob is not using.
  await alice.getByRole("button", { name: "devices" }).click();
  await alice.getByRole("dialog", { name: "device settings" }).locator("select[aria-label=camera]").selectOption({ label: "fake_device_1" });
  await alice.keyboard.press("Escape");
  await quality(bob, "show stats"); // on by default; stays on
  await quality(bob, "receive quality", "high");
  await receives("1280x720", "camera-h");
  await quality(alice, "send resolution", "1080p");
  await receives("1920x1080", "camera-h");
  // Bitrate: once the old layers drain, Bob is the only viewer of Alice's
  // camera-h, so what he receives should match what she sends on it.
  await new Promise((r) => setTimeout(r, 5000));
  const aliceSends = () => alice.locator('.moqit-tile[data-source=camera][data-identity^="alice-"] .moqit-stats div[data-rendition]').evaluateAll((ds) => ds.map((d) => [d.dataset.rendition, Number(d.dataset.bitrate)]));
  const sent = Object.fromEntries(await aliceSends());
  const got = Number(await tile(bob, "alice-").locator(".moqit-stats").getAttribute("data-bitrate"));
  // Besides camera-h for Bob, the server records camera-m (the largest
  // rendition up to 720p) for rewinding.
  const extra = Object.keys(sent).filter((r) => r !== "camera-h" && !(r === "camera-m" && !process.env.MEET_URL));
  if (!(sent["camera-h"] > 0) || extra.length) throw new Error(`Alice should send camera-h (and the recorded camera-m) only: ${JSON.stringify(sent)}`);
  if (!(got > 0.6 * sent["camera-h"] && got < 1.6 * sent["camera-h"])) throw new Error(`Bob receives ${got} bps of Alice's camera-h, she sends ${sent["camera-h"]} bps`);
  const header = async (page) => ({ send: Number(await page.locator(".moqit-meeting-stats").getAttribute("data-send")), receive: Number(await page.locator(".moqit-meeting-stats").getAttribute("data-receive")) });
  const [ha, hb] = [await header(alice), await header(bob)];
  // Totals include audio and are sampled a moment apart from the tiles.
  if (!(ha.send > 0.7 * sent["camera-h"] && hb.receive > 0.7 * got && ha.receive > 0 && hb.send > 0)) {
    throw new Error(`header totals: alice ${JSON.stringify(ha)}, bob ${JSON.stringify(hb)}; tiles: sent ${sent["camera-h"]}, got ${got}`);
  }
  log(`bitrate: Alice sends camera-h at ${Math.round(sent["camera-h"] / 1000)} kbps, Bob receives ${Math.round(got / 1000)} kbps; header ↑${Math.round(ha.send / 1000)}k on Alice, ↓${Math.round(hb.receive / 1000)}k on Bob`);
  await bob.screenshot({ path: join(shots, "bob-receives-1080p.png") });
  await quality(alice, "send resolution", "360p");
  await receives("640x360", "camera-m"); // the top layer is now 360p
  await quality(bob, "receive quality", "low");
  await receives("320x180", "camera-l");
  await quality(bob, "receive quality", "auto");
  await receives("640x360", "camera-m");
  await quality(alice, "send resolution", "720p");
  if (JSON.parse(await bob.evaluate(() => localStorage.getItem("moqit-quality"))).receive !== "auto") throw new Error("receive quality not saved");
  log("quality: send 720p → 1080p → 360p live; receive high / low / auto");

  // Screen share: Bob sees Alice's screen on the stage; Alice sees the
  // presenter card instead of a mirror of her own screen.
  await alice.getByRole("button", { name: "screen share" }).click();
  await alice.locator(".moqit-presenting").waitFor({ timeout: 10000 });
  await alice.getByText("You are presenting").first().waitFor();
  await waitFor(() => bob.evaluate(() => {
    const v = document.querySelector('.moqit-stage .moqit-tile[data-source=screen][data-identity^="alice-"] video, .moqit-stage .moqit-tile[data-source=screen][data-identity^="alice-"] canvas');
    return !!v && (v instanceof HTMLVideoElement ? v.videoWidth > 0 && !v.paused : v.width > 0);
  }), "Bob to see Alice's screen on the stage");
  // The stage is wide enough for screen-h: full 1080p must arrive.
  await waitFor(() => bob.evaluate(() => {
    const v = document.querySelector('.moqit-stage .moqit-tile[data-source=screen] video, .moqit-stage .moqit-tile[data-source=screen] canvas');
    return (v instanceof HTMLVideoElement ? v.videoWidth : v?.width) === 1920;
  }), "Bob to receive Alice's screen at 1920x1080");
  const presented = await bob.evaluate(() => new Promise((resolve) => {
    const v = document.querySelector(".moqit-stage .moqit-tile[data-source=screen] video");
    if (!v) return resolve(-1); // canvas renderer: nothing to count
    let n = 0;
    const tick = () => (n++, v.requestVideoFrameCallback(tick));
    v.requestVideoFrameCallback(tick);
    setTimeout(() => resolve(n), 2000);
  }));
  if (presented >= 0 && presented < 10) throw new Error(`screen share stalled on Bob's side: ${presented} frames in 2s (15 fps sent)`);
  log(`screen share at 1920x1080, ${presented} frames in 2s`);
  await bob.getByText("Alice is presenting").waitFor();
  // The share keeps its shape whatever the stage's: in a wide, short window
  // the 16:9 screen must shrink to fit, not overflow and get cropped.
  for (const viewport of [{ width: 1600, height: 600 }, { width: 800, height: 900 }]) {
    await bob.setViewportSize(viewport);
    await waitFor(() => bob.evaluate(() => {
      const stage = document.querySelector(".moqit-stage")?.getBoundingClientRect();
      const v = document.querySelector(".moqit-stage .moqit-tile[data-source=screen] video, .moqit-stage .moqit-tile[data-source=screen] canvas");
      const r = v?.getBoundingClientRect();
      return !!stage && !!r && r.width > 0 && r.top >= stage.top - 1 && r.bottom <= stage.bottom + 1 && r.left >= stage.left - 1 && r.right <= stage.right + 1
        && getComputedStyle(v).objectFit === "contain";
    }), `Alice's screen to fit Bob's stage at ${viewport.width}x${viewport.height}`, 5000);
    await bob.screenshot({ path: join(shots, `bob-screen-share-${viewport.width}x${viewport.height}.png`) });
  }
  await bob.setViewportSize({ width: 1280, height: 800 });
  log("screen share fits the stage in wide and tall windows");
  if ((await bob.locator(".moqit-strip .moqit-tile").count()) !== 2) throw new Error("camera strip should hold both participants while presenting");
  log("screen share on Bob's stage, presenter card for Alice");
  await bob.screenshot({ path: join(shots, "bob-sees-screen-share.png") });
  await alice.screenshot({ path: join(shots, "alice-presenting.png") });
  await alice.getByRole("button", { name: "Stop presenting" }).click();
  await bob.locator(".moqit-stage").waitFor({ state: "detached", timeout: 10000 });
  await alice.locator(".moqit-grid").waitFor();
  log("stopping the share returns both to the grid");

  // Three participants: Bob's grid fits every tile in the stage, equally
  // sized, whatever the window's shape.
  const carol = await join_(browser, url, "Carol", false);
  await waitFor(async () => (await count(bob)) === 3, "Bob to see three participants");
  await waitFor(() => videoPlaying(bob, "carol-"), "Bob to see Carol's video");
  const gridFits = () => bob.evaluate(() => {
    const grid = document.querySelector(".moqit-grid")?.getBoundingClientRect();
    const tiles = [...document.querySelectorAll(".moqit-grid > .moqit-tile")].map((t) => t.getBoundingClientRect());
    return !!grid && tiles.length === 3 && tiles.every((r) => r.width > 100 && Math.abs(r.width - tiles[0].width) < 1
      && r.top >= grid.top - 1 && r.bottom <= grid.bottom + 1 && r.left >= grid.left - 1 && r.right <= grid.right + 1);
  });
  for (const [viewport, cols] of [[{ width: 1280, height: 800 }, "2"], [{ width: 1800, height: 560 }, "3"], [{ width: 600, height: 1000 }, "1"]]) {
    await bob.setViewportSize(viewport);
    await waitFor(async () => (await gridFits()) && (await bob.locator(".moqit-grid").getAttribute("data-cols")) === cols,
      `three tiles in ${cols} column(s) to fit Bob's ${viewport.width}x${viewport.height} window`, 5000);
    await bob.screenshot({ path: join(shots, `bob-grid-3-${viewport.width}x${viewport.height}.png`) });
  }
  await bob.setViewportSize({ width: 1280, height: 800 });
  log("grid: three participants fit in 2, 3 and 1 columns as the window changes");

  // Bob pins Carol to the stage, takes her full screen and unpins her.
  await bob.getByRole("button", { name: /^pin Carol/ }).click();
  await bob.locator('.moqit-stage .moqit-tile.pinned[data-source=camera][data-identity^="carol-"]').waitFor({ timeout: 5000 });
  const strip = await bob.locator(".moqit-strip .moqit-tile").evaluateAll((ts) => ts.map((t) => t.dataset.identity.split("-")[0]).sort().join(","));
  if (strip !== "alice,bob") throw new Error(`strip beside pinned Carol holds ${strip}`);
  await waitFor(() => bob.evaluate(() => {
    const v = document.querySelector(".moqit-stage .moqit-tile video, .moqit-stage .moqit-tile canvas");
    return (v instanceof HTMLVideoElement ? v.videoWidth : v?.width) >= 1280;
  }), "Bob to receive Carol's top layer on the stage");
  await bob.getByRole("button", { name: "full screen", exact: true }).click();
  await waitFor(() => bob.evaluate(() => document.fullscreenElement?.classList.contains("moqit-stage")), "the stage to go full screen", 5000);
  await bob.screenshot({ path: join(shots, "bob-carol-full-screen.png") });
  await bob.getByRole("button", { name: "exit full screen" }).click();
  await waitFor(() => bob.evaluate(() => !document.fullscreenElement), "full screen to end", 5000);
  await bob.getByRole("button", { name: /^unpin Carol/ }).click();
  await bob.locator(".moqit-grid").waitFor({ timeout: 5000 });
  log("pin: Carol on Bob's stage at her top layer, full screen and back, unpinned to the grid");

  // A pin ends when its participant leaves.
  await bob.getByRole("button", { name: /^pin Carol/ }).click();
  await bob.locator(".moqit-stage").waitFor({ timeout: 5000 });
  await carol.context().close();
  await waitFor(async () => (await count(bob)) === 2 && (await count(alice)) === 2, "both to see Carol leave");
  await bob.locator(".moqit-grid").waitFor({ timeout: 5000 });
  log("a pinned participant leaving returns Bob to the grid");

  await alice.getByRole("button", { name: "chat" }).click();
  await alice.getByLabel("chat message").fill("hello from alice");
  await alice.getByRole("button", { name: "Send" }).click();
  await bob.getByRole("button", { name: "chat" }).click();
  await bob.getByText("hello from alice").waitFor({ timeout: 10000 });
  log("chat delivered");

  await alice.getByRole("button", { name: "microphone" }).click();
  await tile(bob, "alice-").locator(".moqit-muted").waitFor({ timeout: 10000 });
  await waitFor(async () => !(await tile(bob, "alice-").getAttribute("class")).includes("speaking"), "Alice's speaking ring to clear once muted", 3000);
  log("mute shown to the other side, speaking ring cleared");

  await alice.getByRole("button", { name: "camera" }).click();
  await tile(bob, "alice-").locator(".moqit-avatar").waitFor({ timeout: 10000 });
  log("camera off shown to the other side");
  await bob.screenshot({ path: join(shots, "bob-alice-muted-no-video.png") });

  // Alice mutes Bob for herself: her speakers go silent, though his audio
  // still arrives (his speaking ring stays). It holds across his refresh.
  await waitFor(async () => (await outputPeak(alice)) > AUDIBLE, "Alice to hear Bob");
  const heard = await outputPeak(alice);
  await alice.getByRole("button", { name: /^mute Bob for me/ }).click();
  await alice.getByRole("button", { name: /^unmute Bob for me/ }).waitFor({ timeout: 2000 });
  await new Promise((r) => setTimeout(r, 500)); // audio already past the gain drains from the output
  const muted = await outputPeak(alice);
  if (muted > SILENT) throw new Error(`Alice still hears Bob after muting him for herself: level ${muted}`);
  await waitFor(async () => (await tile(alice, "bob-").getAttribute("class")).includes("speaking"), "Bob's speaking ring to stay while muted for Alice", 5000);
  await alice.screenshot({ path: join(shots, "alice-mutes-bob.png") });
  log(`mute for me: Alice heard Bob at ${heard.toFixed(3)}, ${muted.toFixed(4)} once muted`);

  // Bob refreshes the page and joins again: Alice must never see a frozen
  // old Bob next to the new one, and he comes back as the same participant.
  const bobId = () => alice.evaluate(() => [...document.querySelectorAll(".moqit-tile[data-source=camera]")].map((t) => t.dataset.identity).filter((i) => i.startsWith("bob-")));
  const [before] = await bobId();
  let most = 0;
  let refreshing = true;
  const watchCount = (async () => {
    while (refreshing) {
      most = Math.max(most, await count(alice).catch(() => 0));
      await new Promise((r) => setTimeout(r, 100));
    }
  })();
  await bob.reload();
  await bob.getByRole("button", { name: "Join meeting" }).click();
  await waitFor(async () => (await count(alice)) === 2 && (await count(bob)) === 2, "Bob to be back after refreshing");
  await waitFor(() => videoPlaying(alice, "bob-"), "Alice to see Bob's video after his refresh");
  await new Promise((r) => setTimeout(r, 1000));
  refreshing = false;
  await watchCount;
  const after = await bobId();
  if (most > 2) throw new Error(`Alice saw ${most} participants while Bob refreshed`);
  if (after.length !== 1 || after[0] !== before) throw new Error(`Bob rejoined as ${after.join(", ")}, was ${before}`);
  log(`refresh: Bob rejoined as the same participant (${before}); Alice never saw a stale Bob`);
  await waitFor(async () => (await tile(alice, "bob-").getAttribute("class")).includes("speaking"), "Bob's audio to reach Alice after his refresh");
  const stillMuted = await outputPeak(alice);
  if (stillMuted > SILENT) throw new Error(`Bob is audible to Alice again after his refresh: level ${stillMuted}`);
  await alice.getByRole("button", { name: /^unmute Bob for me/ }).click();
  await waitFor(async () => (await outputPeak(alice)) > AUDIBLE, "Alice to hear Bob once she unmutes him");
  log("mute for me: kept across Bob's refresh, and undone");

  await alice.getByRole("button", { name: "leave" }).click();
  await alice.waitForURL(base + "/");
  await waitFor(async () => (await count(bob)) === 1, "Bob to see Alice leave");
  log("leave seen by the other side");
}

main().catch((e) => {
  console.error("FAIL:", e.message);
  process.exit(1);
});
