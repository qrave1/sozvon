(() => {
  "use strict";

  const roomInput = document.getElementById("room");
  const status = document.getElementById("status");
  const videos = document.getElementById("videos");
  const buttons = Object.fromEntries(["join", "leave", "mic", "cam", "screen"].map((id) => [id, document.getElementById(id)]));
  roomInput.value = new URLSearchParams(location.search).get("room") || "";

  let pc = null;
  let ws = null;
  let localStream = null;
  let screenStream = null;
  let cameraTrack = null;
  let messageQueue = Promise.resolve();
  const remoteStreams = new Map();

  function setStatus(text) { status.textContent = text; }

  function setConnected(connected) {
    buttons.join.disabled = connected;
    buttons.leave.disabled = !connected;
    buttons.mic.disabled = !connected;
    buttons.cam.disabled = !connected;
    buttons.screen.disabled = !connected;
    roomInput.disabled = connected;
  }

  function addTile(id, stream, muted = false) {
    let tile = document.getElementById(`tile-${id}`);
    if (!tile) {
      tile = document.createElement("div");
      tile.className = "tile";
      tile.id = `tile-${id}`;
      const video = document.createElement("video");
      video.autoplay = true;
      video.playsInline = true;
      video.muted = muted;
      const label = document.createElement("div");
      label.className = "label";
      label.textContent = muted ? "Вы" : id.slice(0, 8);
      tile.append(video, label);
      videos.append(tile);
    }
    tile.querySelector("video").srcObject = stream;
  }

  function onTrack(event) {
    const streamId = event.streams[0]?.id || event.transceiver.mid;
    let stream = remoteStreams.get(streamId);
    if (!stream) {
      stream = new MediaStream();
      remoteStreams.set(streamId, stream);
    }
    stream.addTrack(event.track);
    addTile(streamId, stream);
    event.track.onended = () => {
      stream.removeTrack(event.track);
      if (!stream.getTracks().length) {
        remoteStreams.delete(streamId);
        document.getElementById(`tile-${streamId}`)?.remove();
      }
    };
  }

  async function iceComplete(connection) {
    if (connection.iceGatheringState === "complete") return;
    await new Promise((resolve) => {
      connection.addEventListener("icegatheringstatechange", () => {
        if (connection.iceGatheringState === "complete") resolve();
      });
    });
  }

  async function handleMessage(message) {
    if (!pc || !ws || ws.readyState !== WebSocket.OPEN) return;
    if (message.type === "joined") {
      await pc.setRemoteDescription(message.data.answer);
      setStatus(`В комнате: ${roomInput.value}`);
    } else if (message.type === "offer") {
      await pc.setRemoteDescription(message.data);
      await pc.setLocalDescription(await pc.createAnswer());
      await iceComplete(pc);
      ws.send(JSON.stringify({ type: "answer", data: pc.localDescription }));
    }
  }

  async function join() {
    const room = roomInput.value.trim();
    if (!room || pc) return;
    setStatus("Подключение...");
    buttons.join.disabled = true;
    try {
      pc = new RTCPeerConnection();
      pc.ontrack = onTrack;
      pc.onconnectionstatechange = () => {
        if (pc) setStatus(`${room}: ${pc.connectionState}`);
      };
      let media;
      try {
        media = await navigator.mediaDevices.getUserMedia({ audio: true, video: true });
      } catch {
        media = await navigator.mediaDevices.getUserMedia({ audio: true, video: false });
      }
      localStream = media;
      cameraTrack = media.getVideoTracks()[0] || null;
      for (const track of media.getTracks()) pc.addTrack(track, media);
      if (!cameraTrack) pc.addTransceiver("video", { direction: "recvonly" });
      addTile("local", media, true);

      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      await iceComplete(pc);
      const proto = location.protocol === "https:" ? "wss" : "ws";
      ws = new WebSocket(`${proto}://${location.host}/sfu/ws`);
      await new Promise((resolve, reject) => {
        ws.onopen = resolve;
        ws.onerror = () => reject(new Error("WebSocket unavailable"));
      });
      ws.onmessage = (event) => {
        messageQueue = messageQueue.then(() => handleMessage(JSON.parse(event.data))).catch((err) => {
          console.error(err);
          setStatus("Ошибка согласования соединения");
        });
      };
      ws.onclose = () => leave();
      ws.send(JSON.stringify({ type: "join", room, data: pc.localDescription }));
      setConnected(true);
    } catch (err) {
      console.error(err);
      leave();
      setStatus(`Не удалось подключиться: ${err.message}`);
    }
  }

  function leave() {
    ws?.close();
    ws = null;
    pc?.close();
    pc = null;
    screenStream?.getTracks().forEach((track) => track.stop());
    screenStream = null;
    localStream?.getTracks().forEach((track) => track.stop());
    localStream = null;
    cameraTrack = null;
    remoteStreams.clear();
    videos.replaceChildren();
    setConnected(false);
    setStatus("Не подключено");
  }

  function toggleTrack(kind, button) {
    const track = localStream?.getTracks().find((item) => item.kind === kind);
    if (!track) return;
    track.enabled = !track.enabled;
    button.setAttribute("aria-pressed", String(!track.enabled));
  }

  async function toggleScreen() {
    if (!pc || !cameraTrack) return;
    const sender = pc.getSenders().find((item) => item.track === (screenStream?.getVideoTracks()[0] || cameraTrack));
    if (!sender) return;
    if (screenStream) {
      await sender.replaceTrack(cameraTrack);
      screenStream.getTracks().forEach((track) => track.stop());
      screenStream = null;
      buttons.screen.setAttribute("aria-pressed", "false");
      addTile("local", localStream, true);
      return;
    }
    try {
      const stream = await navigator.mediaDevices.getDisplayMedia({ video: true });
      const track = stream.getVideoTracks()[0];
      track.onended = () => toggleScreen();
      await sender.replaceTrack(track);
      screenStream = stream;
      buttons.screen.setAttribute("aria-pressed", "true");
      addTile("local", stream, true);
    } catch (err) {
      console.warn("screen share failed", err);
    }
  }

  buttons.join.addEventListener("click", join);
  buttons.leave.addEventListener("click", leave);
  buttons.mic.addEventListener("click", () => toggleTrack("audio", buttons.mic));
  buttons.cam.addEventListener("click", () => toggleTrack("video", buttons.cam));
  buttons.screen.addEventListener("click", toggleScreen);
  window.addEventListener("beforeunload", leave);
})();
