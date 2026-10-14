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
    server = spawn(bin, ["-dev", "-addr", `127.0.0.1:${port}`, "-udp", `:${port}`, "-static", join(repo, "web/apps/meet/dist")], { stdio: ["ignore", "ignore", "pipe"] });
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
  if (!(sent["camera-h"] > 0) || Object.keys(sent).length !== 1) throw new Error(`Alice should send camera-h only: ${JSON.stringify(sent)}`);
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

  await alice.getByRole("button", { name: "leave" }).click();
  await alice.waitForURL(base + "/");
  await waitFor(async () => (await count(bob)) === 1, "Bob to see Alice leave");
  log("leave seen by the other side");
}

main().catch((e) => {
  console.error("FAIL:", e.message);
  process.exit(1);
});
