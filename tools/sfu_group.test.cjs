const assert = require("node:assert/strict");
const { spawn } = require("node:child_process");
const { once } = require("node:events");
const path = require("node:path");
const { test } = require("node:test");
const { chromium } = require(process.env.PLAYWRIGHT_MODULE_PATH || "playwright");

const root = path.resolve(__dirname, "..");
const port = Number(process.env.SFU_TEST_HTTP_PORT || 18003);
const url = `http://127.0.0.1:${port}/?room=browser-group`;
const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function stats(page) {
  return page.evaluate(async () => {
    const reports = await window.testConnections.at(-1).getStats();
    return [...reports.values()].filter((report) => report.type === "inbound-rtp").map((report) => ({
      id: report.id, kind: report.kind, bytes: report.bytesReceived || 0, frames: report.framesDecoded || 0,
    }));
  });
}

async function expectChatUnread(page, unread) {
  await page.waitForFunction((expected) => ["chat-toggle", "chat-tab"].every((id) =>
    document.getElementById(id).classList.contains("chat-unread") === expected), unread);
}

async function sendChat(page, text) {
  await page.locator("#chat-input").fill(text);
  await page.locator("#chat-input").press("Enter");
}

async function expectGroupMedia(pages, remoteCount = 3) {
  for (const page of pages) {
    await page.waitForFunction((count) => {
      const tiles = [...document.querySelectorAll(".tile")].filter((tile) => tile.id !== "tile-local");
      return tiles.length === count && tiles.every((tile) => {
        const audio = tile.querySelector("audio");
        const video = tile.querySelector("video");
        return audio.srcObject?.getAudioTracks().length === 1 && !audio.paused &&
          video.srcObject?.getVideoTracks().length === 1 && video.videoWidth > 0;
      });
    }, remoteCount, { timeout: 20000 });
    const before = await stats(page);
    await pause(800);
    const after = await stats(page);
    const active = after.filter((report) => {
      const previous = before.find((item) => item.id === report.id);
      return previous && report.bytes > previous.bytes &&
        (report.kind === "audio" || report.frames > previous.frames);
    });
    assert.equal(active.filter((report) => report.kind === "audio").length, remoteCount, "every remote audio stream must keep arriving");
    assert.equal(active.filter((report) => report.kind === "video").length, remoteCount, "every remote video stream must keep decoding");
  }
}

test("Group call: late join, camera replacement, bitrate, screen, mute, leave and rejoin", { timeout: 120000 }, async () => {
  const server = spawn(process.env.SFU_TEST_BINARY || path.join(root, ".sfu-check.exe"), [], {
    cwd: root, windowsHide: true,
    env: { ...process.env, PORT: `:${port}`, SFU_UDP_PORT: "40003", SFU_PUBLIC_IP: "", TURN_RELAY_IP: "", TURN_ENABLED: "false" },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let serverLogs = "";
  for (const output of [server.stdout, server.stderr]) output.on("data", (data) => { serverLogs = (serverLogs + data).slice(-8000); });
  let browser;
  try {
    let ready = false;
    for (let i = 0; i < 100; i++) {
      if (server.exitCode !== null) break;
      try { if ((await fetch(url)).ok) { ready = true; break; } } catch {}
      await pause(100);
    }
    assert.ok(ready, `test server did not start: ${serverLogs}`);
    browser = await chromium.launch({ channel: "chrome", headless: true, args: [
      "--use-fake-device-for-media-stream", "--use-fake-ui-for-media-stream",
      "--autoplay-policy=no-user-gesture-required", "--disable-features=WebRtcHideLocalIpsWithMdns",
    ] });
    const pages = [];
    const errors = [];
    for (let i = 0; i < 4; i++) {
      const context = await browser.newContext({
        permissions: ["camera", "microphone"],
        ...(i === 3 ? { viewport: { width: 390, height: 844 }, isMobile: true, userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Mobile" } : {}),
      });
      const page = await context.newPage();
      const websocketURLs = [];
      page.on("websocket", (socket) => websocketURLs.push(socket.url()));
      await page.route("**/turn-config", (route) => route.fulfill({ json: {} }));
      await page.route("**/favicon.ico", (route) => route.fulfill({ status: 204 }));
      page.on("pageerror", (error) => errors.push(error.message));
      page.on("console", (message) => { if (message.type() === "error") errors.push(message.text()); });
      await page.addInitScript((blockVideo) => {
        const getUserMedia = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
        window.testBlockVideo = blockVideo;
        navigator.mediaDevices.getUserMedia = async (constraints) => {
          if (constraints.video && window.testBlockVideo) throw new DOMException("Camera unavailable", "NotAllowedError");
          return getUserMedia(constraints);
        };
        const Original = window.RTCPeerConnection;
        window.testConnections = [];
        window.RTCPeerConnection = class extends Original {
          constructor(config) { super({ ...config, iceServers: [] }); window.testConnections.push(this); }
        };
        navigator.mediaDevices.getDisplayMedia = async () => {
          const canvas = document.createElement("canvas");
          canvas.width = 640; canvas.height = 480;
          let frame = 0;
          const timer = setInterval(() => {
            const ctx = canvas.getContext("2d");
            ctx.fillStyle = frame++ % 2 ? "green" : "red";
            ctx.fillRect(0, 0, 640, 480);
          }, 50);
          const stream = canvas.captureStream(20);
          window.testScreenTrack = stream.getVideoTracks()[0];
          window.testScreenTrack.addEventListener("ended", () => clearInterval(timer));
          return stream;
        };
      }, i === 3);
      await page.goto(url);
      assert.equal(await page.title(), "Sozvon");
      assert.equal(await page.locator("#room").inputValue(), "browser-group");
      await page.locator("#name").fill(`Participant ${i + 1}`);
      await page.locator("#join").click();
      await page.waitForFunction(() => window.testConnections.at(-1)?.connectionState === "connected", null, { timeout: 15000 });
      assert.equal(await page.title(), "Sozvon - browser-group");
      assert.deepEqual(websocketURLs, [`ws://127.0.0.1:${port}/ws`]);
      await page.evaluate(() => { window.testBlockVideo = false; });
      await page.locator("#cam").click();
      pages.push(page);
    }
    await expectGroupMedia(pages);

    await pages[0].evaluate(() => {
      Object.defineProperty(navigator.clipboard, "writeText", { configurable: true, value: async (text) => { window.testSharedLink = text; } });
    });
    await pages[0].locator("#share").click();
    assert.equal(await pages[0].evaluate(() => window.testSharedLink), url);

    const mobile = pages[3];
    await pages[0].locator("#chat-toggle").click();
    await sendChat(pages[0], "First unread message");
    for (const page of pages) await page.waitForFunction(() => document.querySelectorAll(".chat-message").length === 1);
    await expectChatUnread(pages[0], false);
    for (const page of pages.slice(1)) await expectChatUnread(page, true);
    for (const [page, id] of [[pages[1], "chat-toggle"], [mobile, "chat-tab"]]) {
      await page.waitForFunction((buttonId) => getComputedStyle(document.getElementById(buttonId)).backgroundColor === "rgb(251, 191, 36)", id);
    }
    await pages[1].locator("#chat-toggle").click();
    await mobile.locator("#chat-tab").click();
    await expectChatUnread(pages[1], false);
    await expectChatUnread(mobile, false);
    await sendChat(pages[0], "Message with chat open");
    for (const page of pages) await page.waitForFunction(() => document.querySelectorAll(".chat-message").length === 2);
    await expectChatUnread(pages[1], false);
    await expectChatUnread(mobile, false);
    await pages[1].locator("#chat-toggle").click();
    await mobile.locator("#video-tab").click();
    await sendChat(pages[0], "Another unread message");
    await expectChatUnread(pages[1], true);
    await expectChatUnread(mobile, true);
    await mobile.setViewportSize({ width: 1000, height: 844 });
    await expectChatUnread(mobile, true);
    await mobile.locator("#chat-toggle").click();
    await expectChatUnread(mobile, false);
    await mobile.setViewportSize({ width: 390, height: 844 });
    await sendChat(pages[0], "Mobile chat hidden despite desktop panel being open");
    await expectChatUnread(mobile, true);
    await mobile.locator("#chat-tab").click();
    await expectChatUnread(mobile, false);
    await mobile.locator("#video-tab").click();
    await pages[0].locator("#chat-toggle").click();

    await mobile.locator("#settings").click();
    const cameraId = await mobile.evaluate(() => window.testConnections.at(-1).getSenders().find((sender) => sender.track?.kind === "video").track.id);
    const deviceId = await mobile.locator("#video-input").inputValue();
    await mobile.locator("#video-input").selectOption(deviceId);
    await mobile.waitForFunction((previous) => window.testConnections.at(-1).getSenders().some((sender) => sender.track?.kind === "video" && sender.track.id !== previous), cameraId);
    await mobile.locator("#bitrate").selectOption("low");
    await mobile.waitForFunction(() => window.testConnections.at(-1).getSenders().find((sender) => sender.track?.kind === "video").getParameters().encodings[0].maxBitrate === 500000);
    await expectGroupMedia(pages);

    await pages[0].locator("#screen").click();
    await pages[0].waitForFunction(() => window.testConnections.at(-1).getSenders().some((sender) => sender.track === window.testScreenTrack));
    await expectGroupMedia(pages);
    await pages[0].locator("#screen").click();
    await expectGroupMedia(pages);

    await pages[0].locator("#cam").click();
    for (const page of pages.slice(1)) {
      await page.waitForFunction(() => [...document.querySelectorAll(".tile")].some((tile) => tile.querySelector(".label")?.textContent.startsWith("Participant 1") && tile.querySelector("video").hidden && !tile.querySelector("audio").paused));
    }
    await pages[0].locator("#cam").click();
    await pages[1].locator("#mic").click();
    await pages[1].locator("#mic").click();
    await expectGroupMedia(pages);

    await pages[1].locator("#leave").click();
    assert.equal(await pages[1].title(), "Sozvon");
    await expectChatUnread(pages[1], false);
    await expectGroupMedia([pages[0], pages[2], pages[3]], 2);
    await pages[1].locator("#join").click();
    await pages[1].waitForFunction(() => window.testConnections.at(-1)?.connectionState === "connected");
    await pages[1].waitForFunction(() => document.querySelectorAll(".chat-message").length === 4);
    await expectChatUnread(pages[1], false);
    await expectGroupMedia(pages);
    await mobile.evaluate(() => {
      const acquire = navigator.mediaDevices.getUserMedia;
      navigator.mediaDevices.getUserMedia = async (constraints) => {
        const stream = await acquire(constraints);
        window.testPendingCapture = stream;
        await new Promise((resolve) => { window.testReleaseCapture = resolve; });
        return stream;
      };
    });
    await mobile.locator("#video-input").selectOption(deviceId);
    await mobile.waitForFunction(() => !!window.testReleaseCapture);
    await mobile.locator("#leave").click();
    await mobile.evaluate(() => window.testReleaseCapture());
    await mobile.waitForFunction(() => window.testPendingCapture.getTracks().every((track) => track.readyState === "ended"));
    assert.equal(await mobile.locator(".tile").count(), 0, "late camera capture must not restore a departed participant");
    assert.deepEqual(errors, [], "browser must not report signaling or media errors");
  } finally {
    if (browser) await browser.close();
    if (server.exitCode === null) {
      const exited = once(server, "exit");
      server.kill();
      await exited;
    }
  }
});
