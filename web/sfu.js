(() => {
  "use strict";

  const PREFS_KEY = "sozvon.prefs";
  const COLORS = ["#e74c3c", "#3498db", "#2ecc71", "#9b59b6", "#f1c40f", "#1abc9c", "#e67e22", "#34495e", "#fd79a8", "#00cec9"];
  const BITRATES = {
    low: { cam: 500000, screen: 1500000 },
    medium: { cam: 2000000, screen: 4000000 },
    high: { cam: 8000000, screen: 16000000 },
  };
  const el = Object.fromEntries([
    "name", "room", "join", "leave", "welcome", "welcome-name", "welcome-room", "welcome-join", "mic", "cam", "screen", "share", "settings", "status",
    "device-bar", "settings-close", "audio-input", "audio-output", "video-input", "color", "bitrate", "videos", "toast",
  ].map((id) => [id, document.getElementById(id)]));

  let pc = null;
  let ws = null;
  let localStream = null;
  let screenStream = null;
  let cameraTrack = null;
  let joined = false;
  let busy = false;
  let micOn = true;
  let camOn = false;
  let activeId = null;
  let attempt = 0;
  let messageQueue = Promise.resolve();
  let joinSent = false;
  const localCandidates = [];
  const remoteCandidates = [];
  let audioContext = null;
  let speakingTimer = null;
  let noticeTimer = null;
  let bitrateUpdate = Promise.resolve();
  let videoDeviceUpdate = Promise.resolve();
  const localSenders = { audio: null, video: null };
  const remoteStreams = new Map();
  const profiles = new Map();
  const mutedPeers = new Set();
  const analysers = new Map();

  function loadPrefs() {
    try { return JSON.parse(localStorage.getItem(PREFS_KEY)) || {}; } catch { return {}; }
  }

  function savePrefs() {
    try {
      localStorage.setItem(PREFS_KEY, JSON.stringify({
        ...loadPrefs(), name: el.name.value, room: el.room.value,
        audioInputId: el["audio-input"].value, audioOutputId: el["audio-output"].value,
        videoId: el["video-input"].value, prefColor: el.color.value,
        bitratePref: el.bitrate.value,
      }));
    } catch { /* Storage can be unavailable in private mode. */ }
  }

  function colorFor(id) {
    let hash = 0;
    for (const char of id) hash = (hash * 31 + char.charCodeAt(0)) >>> 0;
    return COLORS[hash % COLORS.length];
  }

  function profile() {
    return {
      name: el.name.value.trim().slice(0, 64), micOn, camOn,
      screenShare: !!screenStream,
      color: el.color.value || colorFor(el.name.value.trim() || "me"),
    };
  }

  function normalizeProfile(value) {
    return {
      name: value?.name || "", micOn: value?.micOn ?? true,
      camOn: value?.camOn ?? true, screenShare: !!value?.screenShare,
      color: /^#[0-9a-f]{6}$/i.test(value?.color || "") ? value.color : "",
    };
  }

  function status(text) { el.status.textContent = text; }

  function notify(text, duration = 3000) {
    clearTimeout(noticeTimer);
    status(text);
    if (el.toast) {
      el.toast.textContent = text;
      el.toast.hidden = false;
      el.toast.classList.remove("hidden");
    }
    noticeTimer = setTimeout(() => {
      if (el.toast) { el.toast.hidden = true; el.toast.classList.add("hidden"); }
      if (el.status.textContent === text) status(joined ? `В комнате: ${el.room.value}` : "Не подключено");
    }, duration);
  }

  function updateControls() {
    el.welcome.hidden = joined || busy;
    el.join.hidden = joined;
    el.join.disabled = busy;
    el.leave.hidden = !joined && !busy;
    el.name.disabled = joined || busy;
    el.room.disabled = joined || busy;
    el.mic.disabled = !joined;
    el.cam.disabled = !joined;
    el.screen.hidden = !joined;
    el.share.hidden = !joined;
    el.mic.textContent = micOn ? "🎤 Микрофон" : "🔇 Микрофон";
    el.cam.textContent = camOn ? "📹 Камера" : "🚫 Камера";
    el.screen.textContent = screenStream ? "🖥 Экран: вкл" : "🖥 Экран";
    el.mic.setAttribute("aria-pressed", String(!micOn));
    el.cam.setAttribute("aria-pressed", String(!camOn));
    el.screen.setAttribute("aria-pressed", String(!!screenStream));
  }

  function renderTile(id) {
    let tile = document.getElementById(`tile-${id}`);
    if (!tile) {
      tile = document.createElement("div");
      tile.className = "tile group relative overflow-hidden rounded-2xl border border-line bg-slate-950 shadow-lg transition duration-200 hover:border-slate-500";
      tile.id = `tile-${id}`;
      const video = document.createElement("video");
      video.autoplay = true;
      video.playsInline = true;
      const audio = document.createElement("audio");
      audio.autoplay = true;
      const avatar = document.createElement("div");
      avatar.className = "avatar text-5xl font-bold";
      const label = document.createElement("div");
      label.className = "label rounded-lg border border-white/10 bg-slate-950/75 px-2 py-1 text-xs font-medium text-white backdrop-blur";
      const badge = document.createElement("div");
      badge.className = "badge rounded-lg border border-indigo-300/20 bg-indigo-950/80 px-2 py-1 text-[11px] font-medium text-indigo-100 backdrop-blur";
      const mute = document.createElement("button");
      mute.className = "peer-mute rounded-lg border border-white/10 bg-slate-950/75 px-2 py-1 text-xs text-white opacity-0 backdrop-blur transition group-hover:opacity-100";
      mute.onclick = (event) => {
        event.stopPropagation();
        if (mutedPeers.has(id)) mutedPeers.delete(id); else mutedPeers.add(id);
        renderTile(id);
      };
      const fullscreen = document.createElement("button");
      fullscreen.className = "peer-fullscreen rounded-lg border border-white/10 bg-slate-950/75 px-2 py-1 text-xs text-white opacity-0 backdrop-blur transition group-hover:opacity-100";
      fullscreen.textContent = "⛶";
      fullscreen.title = "Во весь экран";
      fullscreen.setAttribute("aria-label", "Во весь экран");
      fullscreen.onclick = (event) => {
        event.stopPropagation();
        if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
        else tile.requestFullscreen?.().catch(() => {});
      };
      tile.onclick = () => {
        activeId = activeId === id ? null : id;
        el.videos.classList.toggle("spotlight", !!activeId);
        for (const item of el.videos.children) item.classList.toggle("active-lg", item.id === `tile-${activeId}`);
      };
      tile.append(video, audio, avatar, mute, badge, fullscreen, label);
      el.videos.append(tile);
    }

    const own = id === "local";
    const info = own ? profile() : normalizeProfile(profiles.get(id));
    const stream = own ? (screenStream || localStream) : remoteStreams.get(id);
    const video = tile.querySelector("video");
    const audio = tile.querySelector("audio");
    const hasVideo = !!stream?.getVideoTracks().length && (info.camOn || info.screenShare);
    video.muted = true;
    audio.muted = own || mutedPeers.has(id);
    if (video.srcObject !== stream) {
      video.srcObject = stream || null;
      if (stream) video.play().catch(() => {});
    }
    const audioStream = !own && stream?.getAudioTracks().length ? stream : null;
    if (audio.srcObject !== audioStream) audio.srcObject = audioStream;
    if (audioStream) audio.play().catch(() => {});
    if (!own && el["audio-output"].value) audio.setSinkId?.(el["audio-output"].value).catch(() => {});
    video.hidden = !hasVideo;
    const avatar = tile.querySelector(".avatar");
    avatar.hidden = hasVideo;
    avatar.style.background = info.color || colorFor(info.name || id);
    avatar.textContent = (info.name || (own ? "Я" : id)).slice(0, 1).toUpperCase();
    tile.querySelector(".label").textContent = `${info.name || (own ? "Я" : id.slice(0, 8))}${info.micOn ? "" : " 🔇"}`;
    const badge = tile.querySelector(".badge");
    badge.textContent = info.screenShare ? "Экран" : "";
    badge.hidden = !info.screenShare;
    const mute = tile.querySelector(".peer-mute");
    mute.hidden = own;
    mute.textContent = mutedPeers.has(id) ? "🔇" : "🔊";
    mute.title = mutedPeers.has(id) ? "Включить звук для себя" : "Выключить звук для себя";
    mute.setAttribute("aria-label", mute.title);
    tile.querySelector(".peer-fullscreen").hidden = !hasVideo;
  }

  function removePeer(id) {
    profiles.delete(id);
    remoteStreams.delete(id);
    mutedPeers.delete(id);
    stopAnalysis(id);
    document.getElementById(`tile-${id}`)?.remove();
    if (activeId === id) {
      activeId = null;
      el.videos.classList.remove("spotlight");
    }
  }

  function stopAnalysis(id) {
    analysers.get(id)?.source.disconnect();
    analysers.delete(id);
  }

  function analyzeAudio(id, stream) {
    if (!stream?.getAudioTracks().length || analysers.has(id)) return;
    try {
      const Context = window.AudioContext || window.webkitAudioContext;
      if (!Context) return;
      audioContext ||= new Context();
      audioContext.resume().catch(() => {});
      const source = audioContext.createMediaStreamSource(stream);
      const analyser = audioContext.createAnalyser();
      analyser.fftSize = 512;
      source.connect(analyser);
      analysers.set(id, { source, analyser, data: new Float32Array(analyser.fftSize), lastActive: 0 });
      speakingTimer ||= setInterval(() => {
        const now = Date.now();
        for (const [peerId, entry] of analysers) {
          entry.analyser.getFloatTimeDomainData(entry.data);
          let power = 0;
          for (const sample of entry.data) power += sample * sample;
          if (Math.sqrt(power / entry.data.length) > 0.03) entry.lastActive = now;
          document.getElementById(`tile-${peerId}`)?.classList.toggle("speaking", now - entry.lastActive < 900);
        }
      }, 250);
    } catch (error) { console.warn("audio analysis failed", error); }
  }

  function onTrack(event) {
    const stream = event.streams[0];
    const id = stream?.id;
    if (!id) return;
    const refresh = () => {
      if (remoteStreams.get(id) !== stream) return;
      stopAnalysis(id);
      analyzeAudio(id, stream);
      renderTile(id);
    };
    if (remoteStreams.get(id) !== stream) {
      remoteStreams.set(id, stream);
      stream.addEventListener("addtrack", refresh);
      stream.addEventListener("removetrack", refresh);
    }
    event.track.addEventListener("unmute", refresh);
    event.track.addEventListener("ended", () => {
      if (remoteStreams.get(id) !== stream) return;
      stream.removeTrack(event.track);
      refresh();
    });
    refresh();
  }

  async function enumerateDevices() {
    if (!navigator.mediaDevices?.enumerateDevices) return;
    try {
      const devices = await navigator.mediaDevices.enumerateDevices();
      const selections = { audioinput: el["audio-input"], audiooutput: el["audio-output"], videoinput: el["video-input"] };
      for (const [kind, select] of Object.entries(selections)) {
        const selected = select.value;
        select.replaceChildren();
        for (const device of devices.filter((item) => item.kind === kind)) {
          const option = document.createElement("option");
          option.value = device.deviceId;
          option.textContent = device.label || `${kind === "videoinput" ? "Камера" : kind === "audiooutput" ? "Аудиовыход" : "Микрофон"} ${select.length + 1}`;
          select.append(option);
        }
        if ([...select.options].some((option) => option.value === selected)) select.value = selected;
        select.disabled = !select.length || (kind === "audiooutput" && typeof HTMLMediaElement.prototype.setSinkId !== "function");
      }
    } catch (error) { console.warn("device enumeration failed", error); }
  }

  function applyAudioOutput() {
    if (!el["audio-output"].value) return;
    for (const audio of el.videos.querySelectorAll("audio")) audio.setSinkId?.(el["audio-output"].value).catch(() => {});
  }

  async function loadIceServers() {
    const servers = [{ urls: "stun:stun.l.google.com:19302" }];
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 6000);
    try {
      const response = await fetch("/turn-config", { signal: controller.signal });
      if (response.ok) {
        const config = await response.json();
        if (config.urls) servers.push({ urls: config.urls, username: config.username, credential: config.credential });
      }
    } catch (error) { console.warn("TURN config unavailable", error); }
    finally { clearTimeout(timeout); }
    return servers;
  }

  async function acquireTrack(kind) {
    if (!navigator.mediaDevices?.getUserMedia) throw new Error("Камера и микрофон доступны только через HTTPS");
    const select = kind === "audio" ? el["audio-input"] : el["video-input"];
    const constraints = { [kind]: select.value ? { deviceId: { exact: select.value } } : true };
    const token = attempt;
    let stream;
    try { stream = await navigator.mediaDevices.getUserMedia(constraints); }
    catch { stream = await navigator.mediaDevices.getUserMedia({ [kind]: true }); }
    if (token !== attempt) {
      stream.getTracks().forEach((track) => track.stop());
      throw new Error("Соединение закрыто");
    }
    return stream.getTracks().find((track) => track.kind === kind);
  }

  async function startMedia() {
    if (!navigator.mediaDevices?.getUserMedia) {
      notify("Камера и микрофон доступны только через HTTPS", 6000);
      return new MediaStream();
    }
    const audio = el["audio-input"].value ? { deviceId: { exact: el["audio-input"].value } } : true;
    const video = el["video-input"].value ? { deviceId: { exact: el["video-input"].value } } : true;
    let media;
    try { media = await navigator.mediaDevices.getUserMedia({ audio, video }); }
    catch {
      try { media = await navigator.mediaDevices.getUserMedia({ audio }); }
      catch {
        try { media = await navigator.mediaDevices.getUserMedia({ video }); }
        catch { media = new MediaStream(); }
      }
    }
    return media;
  }

  function videoSender() {
    return localSenders.video;
  }

  function applyBitrate() {
    const connection = pc;
    const sender = videoSender();
    if (!connection || !sender?.track) return Promise.resolve();
    bitrateUpdate = bitrateUpdate.catch(() => {}).then(async () => {
      if (pc !== connection || sender.track?.kind !== "video") return;
      const params = sender.getParameters();
      if (!params.encodings?.length) return;
      const preset = BITRATES[el.bitrate.value] || BITRATES.medium;
      for (const encoding of params.encodings) encoding.maxBitrate = screenStream ? preset.screen : preset.cam;
      await sender.setParameters(params);
    });
    return bitrateUpdate;
  }

  async function openWebSocket(url) {
    const socket = new WebSocket(url);
    await new Promise((resolve, reject) => {
      const timeout = setTimeout(() => fail(new Error("Тайм-аут WebSocket")), 10000);
      function fail(error) { clearTimeout(timeout); socket.close(); reject(error); }
      socket.onopen = () => { clearTimeout(timeout); resolve(); };
      socket.onerror = () => fail(new Error("WebSocket недоступен"));
    });
    return socket;
  }

  async function handleMessage(message, token) {
    if (token !== attempt || !pc || !ws || ws.readyState !== WebSocket.OPEN) return;
    const connection = pc;
    const socket = ws;
    if (message.type === "joined") {
      await connection.setRemoteDescription(message.data.answer);
      if (token !== attempt) return;
      for (const candidate of remoteCandidates.splice(0)) await connection.addIceCandidate(candidate);
      if (token !== attempt) return;
      for (const id of message.data.peers || []) {
        profiles.set(id, normalizeProfile(message.data.profiles?.[id]));
        renderTile(id);
      }
      joined = true;
      busy = false;
      document.title = `Sozvon SFU - ${el.room.value}`;
      updateControls();
      status(pc.connectionState === "connected" ? `В комнате: ${el.room.value}` : "Подключение медиа...");
    } else if (message.type === "peer_joined") {
      profiles.set(message.data.id, normalizeProfile(message.data.profile));
      renderTile(message.data.id);
    } else if (message.type === "state") {
      profiles.set(message.data.id, normalizeProfile(message.data.profile));
      renderTile(message.data.id);
    } else if (message.type === "peer_left") {
      removePeer(message.data.id);
    } else if (message.type === "offer") {
      await connection.setRemoteDescription(message.data);
      if (token !== attempt) return;
      await connection.setLocalDescription(await connection.createAnswer());
      if (token !== attempt) return;
      socket.send(JSON.stringify({ type: "answer", data: connection.localDescription }));
    } else if (message.type === "candidate") {
      if (connection.remoteDescription) await connection.addIceCandidate(message.data);
      else remoteCandidates.push(message.data);
    }
  }

  async function join() {
    const room = el.room.value.trim();
    if (!room || pc || busy) { if (!room) notify("Введите ID комнаты"); return; }
    el.room.value = room;
    savePrefs();
    const url = new URL(location.href);
    url.searchParams.set("room", room);
    history.replaceState(null, "", url);
    const token = ++attempt;
    joinSent = false;
    localCandidates.length = 0;
    remoteCandidates.length = 0;
    busy = true;
    updateControls();
    status("Запрос камеры и микрофона...");
    try {
      const media = await startMedia();
      if (token !== attempt) { media.getTracks().forEach((track) => track.stop()); return; }
      localStream = media;
      cameraTrack = media.getVideoTracks()[0] || null;
      for (const track of media.getAudioTracks()) track.enabled = micOn;
      if (cameraTrack) cameraTrack.enabled = camOn;
      await enumerateDevices();
      if (token !== attempt) return;
      renderTile("local");
      analyzeAudio("local", media);
      status("Поиск сетевого маршрута...");
      const iceServers = await loadIceServers();
      if (token !== attempt) return;
      pc = new RTCPeerConnection({ iceServers, bundlePolicy: "max-bundle" });
      pc.ontrack = (event) => { if (token === attempt) onTrack(event); };
      pc.onicecandidate = (event) => {
        if (!event.candidate || token !== attempt) return;
        const candidate = event.candidate.toJSON();
        if (joinSent && ws?.readyState === WebSocket.OPEN) {
          ws.send(JSON.stringify({ type: "candidate", data: candidate }));
        } else {
          localCandidates.push(candidate);
        }
      };
      pc.onconnectionstatechange = () => {
        if (!pc || token !== attempt) return;
        if (pc.connectionState === "connected") status(`В комнате: ${room}`);
        else if (pc.connectionState === "failed") status("Медиасоединение не установлено");
        else if (joined && pc.connectionState === "disconnected") status("Медиасоединение прервано");
      };
      for (const kind of ["audio", "video"]) {
        const track = localStream.getTracks().find((item) => item.kind === kind);
        const transceiver = pc.addTransceiver(track || kind, { direction: "sendonly", streams: [localStream] });
        localSenders[kind] = transceiver.sender;
      }
      await pc.setLocalDescription(await pc.createOffer());
      await applyBitrate().catch((error) => console.warn("initial bitrate update failed", error));
      if (token !== attempt) return;
      const proto = location.protocol === "https:" ? "wss" : "ws";
      status("Подключение к комнате...");
      const socket = await openWebSocket(`${proto}://${location.host}/sfu/ws`);
      if (token !== attempt) { socket.close(); return; }
      ws = socket;
      ws.onmessage = (event) => {
        messageQueue = messageQueue.then(() => handleMessage(JSON.parse(event.data), token)).catch((error) => {
          if (token !== attempt) return;
          console.error(error);
          status("Ошибка согласования соединения");
        });
      };
      ws.onclose = () => { if (token === attempt) leave("Соединение закрыто"); };
      ws.send(JSON.stringify({ type: "join", room, data: pc.localDescription, profile: profile() }));
      joinSent = true;
      for (const candidate of localCandidates.splice(0)) {
        ws.send(JSON.stringify({ type: "candidate", data: candidate }));
      }
    } catch (error) {
      console.error("SFU join failed", error);
      if (token === attempt) leave(`Не удалось подключиться: ${error.message}`);
    }
  }

  function leave(message = "Не подключено") {
    attempt += 1;
    joinSent = false;
    localCandidates.length = 0;
    remoteCandidates.length = 0;
    busy = false;
    joined = false;
    clearTimeout(noticeTimer);
    if (ws) { ws.onclose = null; ws.close(); ws = null; }
    if (pc) { pc.close(); pc = null; }
    localSenders.audio = null;
    localSenders.video = null;
    screenStream?.getTracks().forEach((track) => { track.onended = null; track.stop(); });
    screenStream = null;
    localStream?.getTracks().forEach((track) => track.stop());
    localStream = null;
    cameraTrack = null;
    remoteStreams.clear();
    profiles.clear();
    mutedPeers.clear();
    for (const id of analysers.keys()) stopAnalysis(id);
    clearInterval(speakingTimer);
    speakingTimer = null;
    audioContext?.close().catch(() => {});
    audioContext = null;
    activeId = null;
    el.videos.classList.remove("spotlight");
    el.videos.replaceChildren();
    document.title = "Sozvon SFU";
    messageQueue = Promise.resolve();
    updateControls();
    status(message);
  }

  async function replaceLocalTrack(kind, track) {
    const connection = pc;
    const sender = localSenders[kind];
    if (!sender) { track.stop(); throw new Error("Не найден медиаканал"); }
    try { await sender.replaceTrack(track); }
    catch (error) { track.stop(); throw error; }
    if (pc !== connection || localSenders[kind] !== sender) {
      track.stop();
      throw new Error("Соединение закрыто");
    }
    const old = localStream?.getTracks().find((item) => item.kind === kind);
    if (old) { localStream.removeTrack(old); old.stop(); }
    localStream ||= new MediaStream();
    localStream.addTrack(track);
    if (kind === "video") cameraTrack = track;
    stopAnalysis("local");
    analyzeAudio("local", localStream);
    renderTile("local");
    await applyBitrate().catch((error) => console.warn("bitrate update failed after track replacement", error));
    if (pc === connection && kind === "video") requestVideoRefresh();
  }

  function broadcastState() {
    if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "state", data: profile() }));
  }

  function requestVideoRefresh() {
    if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "refresh_video" }));
  }

  async function toggleMic() {
    const token = attempt;
    let track = localStream?.getAudioTracks()[0];
    if (!track) {
      try { track = await acquireTrack("audio"); await replaceLocalTrack("audio", track); }
      catch { if (token === attempt) notify("Микрофон недоступен"); return; }
      if (token !== attempt) return;
      track.enabled = true;
    } else {
      track.enabled = !track.enabled;
    }
    micOn = track.enabled;
    renderTile("local");
    updateControls();
    broadcastState();
  }

  async function toggleCam() {
    const token = attempt;
    if (screenStream) { await toggleScreen(); return; }
    let track = cameraTrack;
    if (!track) {
      try { track = await acquireTrack("video"); await replaceLocalTrack("video", track); }
      catch { if (token === attempt) notify("Камера недоступна"); return; }
      if (token !== attempt) return;
      track.enabled = true;
    } else {
      track.enabled = !track.enabled;
    }
    camOn = track.enabled;
    renderTile("local");
    updateControls();
    broadcastState();
  }

  async function toggleScreen() {
    const token = attempt;
    const sender = videoSender();
    if (!sender) return;
    if (screenStream) {
      const previousScreen = screenStream;
      await sender.replaceTrack(cameraTrack);
      if (token !== attempt) return;
      previousScreen.getTracks().forEach((track) => { track.onended = null; track.stop(); });
      screenStream = null;
    } else {
      if (!navigator.mediaDevices?.getDisplayMedia) { notify("Демонстрация экрана недоступна"); return; }
      let stream;
      try {
        stream = await navigator.mediaDevices.getDisplayMedia({ video: true });
        if (token !== attempt) { stream.getTracks().forEach((track) => track.stop()); return; }
        await sender.replaceTrack(stream.getVideoTracks()[0]);
        if (token !== attempt) { stream.getTracks().forEach((track) => track.stop()); return; }
        screenStream = stream;
        stream.getVideoTracks()[0].onended = () => { if (token === attempt) toggleScreen().catch(console.error); };
      } catch (error) {
        stream?.getTracks().forEach((track) => track.stop());
        if (token === attempt) console.warn("screen share failed", error);
        return;
      }
    }
    renderTile("local");
    updateControls();
    await applyBitrate().catch((error) => console.warn("bitrate update failed after screen share change", error));
    if (token !== attempt) return;
    broadcastState();
    requestVideoRefresh();
  }

  async function changeDevice(kind) {
    const token = attempt;
    savePrefs();
    if (kind === "audiooutput") { applyAudioOutput(); return; }
    if (!joined) return;
    if (kind === "video" && screenStream) { notify("Остановите демонстрацию экрана"); return; }
    if (kind === "video") {
      const deviceId = el["video-input"].value;
      videoDeviceUpdate = videoDeviceUpdate.catch(() => {}).then(() => changeVideoDevice(deviceId, token)).catch((error) => {
        if (token !== attempt) return;
        console.warn("camera switch failed", error);
        notify("Не удалось переключить камеру");
      });
      return videoDeviceUpdate;
    }
    try {
      const track = await acquireTrack(kind);
      track.enabled = kind === "audio" ? micOn : camOn;
      await replaceLocalTrack(kind, track);
      if (token === attempt) broadcastState();
    } catch (error) {
      if (token !== attempt) return;
      console.warn("device change failed", error);
      notify("Устройство недоступно");
    }
  }

  async function changeVideoDevice(deviceId, token) {
    if (token !== attempt || !joined || !pc || screenStream) return;
    const sender = videoSender();
    const previousTrack = cameraTrack;
    const previousDeviceId = previousTrack?.getSettings().deviceId || "";
    const previousEnabled = previousTrack?.enabled ?? camOn;
    const isMobile = navigator.userAgentData?.mobile || /Android|iPhone|iPad|iPod/i.test(navigator.userAgent);
    const acquireVideo = async (id) => {
      if (token !== attempt) throw new Error("Соединение закрыто");
      const constraints = id ? { deviceId: { exact: id } } : true;
      const stream = await navigator.mediaDevices.getUserMedia({ video: constraints });
      if (token !== attempt) {
        stream.getTracks().forEach((item) => item.stop());
        throw new Error("Соединение закрыто");
      }
      const track = stream.getVideoTracks()[0];
      if (!track) {
        stream.getTracks().forEach((item) => item.stop());
        throw new Error("Камера не вернула видеотрек");
      }
      return track;
    };
    const releasePrevious = async () => {
      await sender.replaceTrack(null);
      if (token !== attempt) throw new Error("Соединение закрыто");
      localStream?.removeTrack(previousTrack);
      previousTrack.stop();
      cameraTrack = null;
    };
    const restorePrevious = async () => {
      const restored = await acquireVideo(previousDeviceId);
      restored.enabled = previousEnabled;
      await sender.replaceTrack(restored);
      if (token !== attempt) { restored.stop(); return; }
      localStream ||= new MediaStream();
      localStream.addTrack(restored);
      cameraTrack = restored;
      renderTile("local");
      await applyBitrate().catch((error) => console.warn("bitrate update failed after camera restore", error));
      if (token === attempt) requestVideoRefresh();
    };

    let track;
    let releasedPrevious = false;
    try {
      if (isMobile && previousTrack && sender) {
        await releasePrevious();
        releasedPrevious = true;
      }
      track = await acquireVideo(deviceId);
    } catch (firstError) {
      if (token !== attempt) return;
      if (!previousTrack || !sender) throw firstError;
      if (!releasedPrevious) {
        await releasePrevious();
        releasedPrevious = true;
        try {
          track = await acquireVideo(deviceId);
        } catch (switchError) {
          firstError = switchError;
        }
      }
      if (!track && releasedPrevious) {
        try { await restorePrevious(); }
        catch (restoreError) { console.warn("camera restore failed", restoreError); }
        if ([...el["video-input"].options].some((option) => option.value === previousDeviceId)) {
          el["video-input"].value = previousDeviceId;
          savePrefs();
        }
        throw firstError;
      }
    }

    track.enabled = camOn;
    await replaceLocalTrack("video", track);
    if (token !== attempt) return;
    savePrefs();
    broadcastState();
  }

  async function share() {
    const link = `${location.origin}${location.pathname}?room=${encodeURIComponent(el.room.value)}`;
    try {
      if (navigator.clipboard?.writeText) await navigator.clipboard.writeText(link);
      else {
        const input = document.createElement("textarea");
        input.value = link;
        input.style.position = "fixed";
        input.style.opacity = "0";
        document.body.append(input);
        input.select();
        if (!document.execCommand("copy")) throw new Error("copy failed");
        input.remove();
      }
      notify("Ссылка скопирована", 2000);
    } catch { notify("Не удалось скопировать ссылку"); }
  }

  const prefs = loadPrefs();
  el.name.value = prefs.name || "";
  el.room.value = new URLSearchParams(location.search).get("room") || new URLSearchParams(location.search).get("id") || prefs.room || "";
  el.color.value = COLORS.includes(prefs.prefColor) ? prefs.prefColor : "";
  el.bitrate.value = BITRATES[prefs.bitratePref] ? prefs.bitratePref : "medium";
  el["audio-input"].dataset.preferred = prefs.audioInputId || "";
  el["audio-output"].dataset.preferred = prefs.audioOutputId || "";
  el["video-input"].dataset.preferred = prefs.videoId || "";
  el.join.onclick = join;
  el.leave.onclick = () => leave();
  el.mic.onclick = toggleMic;
  el.cam.onclick = toggleCam;
  el.screen.onclick = () => toggleScreen().catch(console.error);
  el.share.onclick = share;
  el["welcome-join"].onclick = () => {
    el.name.value = el["welcome-name"].value;
    el.room.value = el["welcome-room"].value;
    el.join.click();
  };
  el["welcome-name"].addEventListener("input", () => { el.name.value = el["welcome-name"].value; });
  el["welcome-room"].addEventListener("input", () => { el.room.value = el["welcome-room"].value; });
  el.name.addEventListener("input", () => { el["welcome-name"].value = el.name.value; });
  el.room.addEventListener("input", () => { el["welcome-room"].value = el.room.value; });
  el.settings.onclick = () => {
    el["device-bar"].hidden = !el["device-bar"].hidden;
    el.settings.setAttribute("aria-pressed", String(!el["device-bar"].hidden));
  };
  el["settings-close"].onclick = () => {
    el["device-bar"].hidden = true;
    el.settings.setAttribute("aria-pressed", "false");
  };
  el.name.addEventListener("input", () => { savePrefs(); if (document.getElementById("tile-local")) renderTile("local"); });
  el.room.addEventListener("input", savePrefs);
  el.color.onchange = () => { savePrefs(); if (joined) { renderTile("local"); broadcastState(); } };
  el.bitrate.onchange = () => {
    savePrefs();
    if (!joined) return;
    applyBitrate().then(() => notify("Качество видео обновлено", 1800)).catch((error) => {
      console.warn("bitrate update failed", error);
      notify("Не удалось применить качество видео");
    });
  };
  el["audio-input"].onchange = () => changeDevice("audio");
  el["audio-output"].onchange = () => changeDevice("audiooutput");
  el["video-input"].onchange = () => changeDevice("video");
  navigator.mediaDevices?.addEventListener("devicechange", enumerateDevices);
  document.addEventListener("pointerdown", () => {
    audioContext?.resume().catch(() => {});
    for (const audio of el.videos.querySelectorAll("audio")) {
      if (audio.srcObject) audio.play().catch(() => {});
    }
  });
  window.addEventListener("beforeunload", () => leave());
  updateControls();
  enumerateDevices().then(() => {
    for (const id of ["audio-input", "audio-output", "video-input"]) {
      const select = el[id];
      if ([...select.options].some((option) => option.value === select.dataset.preferred)) select.value = select.dataset.preferred;
    }
  });
})();
