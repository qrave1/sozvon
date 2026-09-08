function callApp() {
  "use strict";

  const ICE_SERVERS = [
    { urls: "stun:stun.l.google.com:19302" },
  ];

  const COLORS = [
    "#e74c3c", "#3498db", "#2ecc71", "#9b59b6", "#f1c40f",
    "#1abc9c", "#e67e22", "#34495e", "#fd79a8", "#00cec9",
  ];

  const MAX_RECONNECT_ATTEMPTS = 5;
  const RECONNECT_BASE_DELAY_MS = 1000;
  const RECONNECT_MAX_DELAY_MS = 15000;
  const PREFS_KEY = "sozvon.prefs";
  const SPOTLIGHT_ENABLED = false;

  function loadPrefs() {
    try {
      return JSON.parse(localStorage.getItem(PREFS_KEY)) || {};
    } catch {
      return {};
    }
  }

  function savePrefs(prefs) {
    try {
      localStorage.setItem(PREFS_KEY, JSON.stringify(prefs));
    } catch {}
  }

  function getRoomFromUrl() {
    const urlParams = new URLSearchParams(window.location.search);
    return urlParams.get('room') || urlParams.get('id') || '';
  }

  function colorFor(id) {
    let h = 0;
    for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
    return COLORS[h % COLORS.length];
  }

  function setVideoBitrate(sender, bitrate) {
    const params = sender.getParameters();
    if (!params.encodings) params.encodings = [{}];
    params.encodings.forEach((e) => { e.maxBitrate = bitrate; });
    sender.setParameters(params).catch(() => {});
  }

  let audioCtx = null;
  let speakTimer = null;
  let notifyTimer = null;
  let netTimer = null;
  const analysers = new Map();

  return {
    name: "",
    room: "",
    status: "не подключено",
    connected: false,
    micOn: false,
    camOn: false,
    localStream: null,
    myColor: "#3498db",
    initial: "Я",
    peers: [],

    ws: null,
    myId: null,
    connecting: false,
    connectionToken: 0,
    iceServers: ICE_SERVERS,
    pcs: new Map(),
    mutedPeers: new Set(),
    pendingIce: new Map(),
    pendingConnectReject: null,
    statsPrev: new Map(),
    reconnectAttempts: 0,
    reconnectTimer: null,
    codecPref: "auto",

    audioInputId: "",
    audioOutputId: "",
    videoId: "",
    activeId: null,
    showSettings: false,
    screenShare: false,
    screenStream: null,
    _screenAudioTrack: null,
    _savedMicTrack: null,
    _camWasOn: false,
    speaking: false,
    spotManual: false,
    spotAuto: false,
    devices: { audioinput: [], audiooutput: [], videoinput: [] },
    audioOutputSupported: false,

    async init() {
      const prefs = loadPrefs();
      if (prefs.name) this.name = prefs.name;
      if (prefs.codecPref) this.codecPref = prefs.codecPref;
      if (prefs.audioInputId) this.audioInputId = prefs.audioInputId;
      if (prefs.audioOutputId) this.audioOutputId = prefs.audioOutputId;
      if (prefs.videoId) this.videoId = prefs.videoId;
      this.room = getRoomFromUrl() || prefs.room || "";

      ["name", "room", "codecPref", "audioInputId", "audioOutputId", "videoId"].forEach((k) =>
        this.$watch(k, () => this.persist())
      );

      window.addEventListener("beforeunload", () => this.leave());
      window.addEventListener("keydown", (e) => this.onKeydown(e));
      this.audioOutputSupported =
        typeof HTMLVideoElement !== "undefined" &&
        typeof HTMLVideoElement.prototype.setSinkId === "function";
      await this.enumerateDevices();
      navigator.mediaDevices?.addEventListener("devicechange", () => {
        this.enumerateDevices({ resetInvalid: true });
      });
    },

    persist() {
      savePrefs({
        name: this.name,
        room: this.room,
        codecPref: this.codecPref,
        audioInputId: this.audioInputId,
        audioOutputId: this.audioOutputId,
        videoId: this.videoId,
      });
    },

    onKeydown(e) {
      if (e.repeat) return;
      const tag = e.target?.tagName;
      if (tag === "INPUT" || tag === "SELECT" || tag === "TEXTAREA") return;
      const k = e.key.toLowerCase();
      if (k === "m" || k === "ь") this.toggleMic();
      else if (k === "v" || k === "м") this.toggleCam();
    },

    async enumerateDevices({ resetInvalid = false } = {}) {
      if (!navigator.mediaDevices?.enumerateDevices) return;
      try {
        const all = await navigator.mediaDevices.enumerateDevices();
        const grouped = { audioinput: [], audiooutput: [], videoinput: [] };
        for (const d of all) {
          if (d.kind in grouped) grouped[d.kind].push(d);
        }
        this.devices = grouped;

        const selectDevice = (current, list) => {
          const exists = list.some((d) => d.deviceId === current);
          return exists || (!resetInvalid && current) ? current : (list[0]?.deviceId || "");
        };
        this.audioInputId = selectDevice(this.audioInputId, grouped.audioinput);
        this.audioOutputId = selectDevice(this.audioOutputId, grouped.audiooutput);
        this.videoId = selectDevice(this.videoId, grouped.videoinput);
      } catch (err) {
        console.warn("не удалось получить список устройств", err);
      }
    },

    async loadIceServers() {
      try {
        const res = await fetch("/turn-config");
        const cfg = await res.json();
        const servers = [{ urls: "stun:stun.l.google.com:19302" }];
        if (cfg.urls) {
          servers.push({
            urls: cfg.urls,
            username: cfg.username,
            credential: cfg.credential,
          });
        }
        this.iceServers = servers;
      } catch (err) {
        console.warn("failed to load TURN config, using STUN only", err);
      }
    },

    attachStream(id, stream) {
      this.$nextTick(() => {
        const video = document.getElementById("vid-" + id);
        if (video && stream) {
          video.srcObject = stream;
          if (this.mutedPeers.has(id)) this.setRemoteMuted(id, true);
          this.applyAudioOutput();
        }
      });
    },

    queueIce(id, candidate) {
      if (!this.pendingIce.has(id)) this.pendingIce.set(id, []);
      this.pendingIce.get(id).push(candidate);
    },

    async flushIceQueue(id) {
      const queue = this.pendingIce.get(id);
      if (!queue) return;
      this.pendingIce.delete(id);
      const pc = this.pcs.get(id);
      if (!pc) return;
      const results = await Promise.allSettled(
        queue.map((c) => pc.addIceCandidate(new RTCIceCandidate(c)))
      );
      for (const result of results) {
        if (result.status === "rejected") {
          console.error("addIceCandidate failed", result.reason);
        }
      }
    },

    setRemoteMuted(id, muted) {
      const video = document.getElementById("vid-" + id);
      if (!video?.srcObject) return;
      video.srcObject.getAudioTracks().forEach((t) => (t.enabled = !muted));
    },

    toggleMuteUser(id) {
      if (this.mutedPeers.has(id)) {
        this.mutedPeers.delete(id);
        this.setRemoteMuted(id, false);
      } else {
        this.mutedPeers.add(id);
        this.setRemoteMuted(id, true);
      }
      const p = this.peers.find((x) => x.id === id);
      if (p) p.muted = this.mutedPeers.has(id);
    },

    addPeer(id, info) {
      if (!this.peers.find((p) => p.id === id)) {
        this.peers.push({
          id,
          name: info?.name || "",
          camOn: info?.camOn ?? true,
          micOn: info?.micOn ?? true,
          color: colorFor(id),
          initial: (info?.name || id).slice(0, 1).toUpperCase(),
          mode: "",
          muted: this.mutedPeers.has(id),
          speaking: false,
          rtt: null,
          level: "",
        });
      }
    },

    removePeer(id) {
      this.peers = this.peers.filter((p) => p.id !== id);
      this.mutedPeers.delete(id);
      this.pendingIce.delete(id);
      this.stopAudioAnalysis(id);
      for (const k of [...this.statsPrev.keys()]) {
        if (k.startsWith(id + ":")) this.statsPrev.delete(k);
      }
      const pc = this.pcs.get(id);
      if (pc) {
        pc.close();
        this.pcs.delete(id);
      }
    },

    setPeerMode(id, mode) {
      const p = this.peers.find((x) => x.id === id);
      if (p) p.mode = mode;
    },

    updatePeer(id, patch) {
      const p = this.peers.find((x) => x.id === id);
      if (!p) return;
      Object.assign(p, patch);
      if (patch.name) p.initial = patch.name.slice(0, 1).toUpperCase();
    },

    applyVideoRestrictions(pc) {
      if (!pc.getTransceivers) return;
      for (const tr of pc.getTransceivers()) {
        if (tr.sender?.track?.kind !== "video") continue;
        setVideoBitrate(tr.sender, 2000000);
      }
    },

    applyVideoCodecPrefs(pc) {
      if (!pc.getTransceivers) return;
      const caps = RTCRtpSender.getCapabilities?.("video") || RTCRtpReceiver.getCapabilities?.("video");
      if (!caps?.codecs?.length) return;
      const wanted = (this.codecPref || "").toLowerCase();
      let codecs = caps.codecs.slice();
      if (wanted && wanted !== "auto") {
        codecs.sort((a, b) => {
          const aHit = a.mimeType.toLowerCase().includes(wanted);
          const bHit = b.mimeType.toLowerCase().includes(wanted);
          return (bHit ? 1 : 0) - (aHit ? 1 : 0);
        });
      }
      for (const tr of pc.getTransceivers()) {
        if (tr.sender?.track?.kind !== "video") continue;
        try { tr.setCodecPreferences(codecs); } catch (_) {}
      }
    },

    changeCodec() {
      for (const [peerId, pc] of this.pcs) {
        this.applyVideoCodecPrefs(pc);
        if (this.connected) this.makeOffer(peerId, pc);
      }
    },

    async makeOffer(peerId, pc) {
      try {
        pc._makingOffer = true;
        await pc.setLocalDescription();
        this.send({ type: "offer", to: peerId, data: pc.localDescription });
      } catch (err) {
        console.error("offer failed", err);
      } finally {
        pc._makingOffer = false;
      }
    },

    applyVideoBitrate(bitrate) {
      for (const pc of this.pcs.values()) {
        const sender = pc.getSenders().find((s) => s.track?.kind === "video");
        if (sender) setVideoBitrate(sender, bitrate);
      }
    },

    async replaceOrAddTrack(pc, kind, track) {
      if (!pc || pc.connectionState === "closed") return;

      let sender = pc.getSenders().find((s) => s.track?.kind === kind);
      if (!sender && pc.getTransceivers) {
        const transceiver = pc.getTransceivers().find((tr) =>
          tr.sender?.track?.kind === kind || tr.receiver?.track?.kind === kind
        );
        sender = transceiver?.sender || null;
      }

      if (sender) {
        await sender.replaceTrack(track);
      } else if (track && this.localStream) {
        pc.addTrack(track, this.localStream);
      }
    },

    createPeer(peerId, initiator) {
      const pc = new RTCPeerConnection({
        iceServers: this.iceServers,
        bundlePolicy: "max-bundle",
      });
      pc._polite = !initiator;
      pc._makingOffer = false;
      pc._ignoreOffer = false;

      if (this.localStream) {
        for (const track of this.localStream.getTracks()) {
          pc.addTrack(track, this.localStream);
        }
      }

      this.applyVideoRestrictions(pc);
      this.applyVideoCodecPrefs(pc);

      pc.onicecandidate = (e) => {
        if (e.candidate) {
          this.send({ type: "candidate", to: peerId, data: e.candidate });
        }
      };

      pc.ontrack = (e) => {
        const [stream] = e.streams;
        this.attachStream(peerId, stream);
        this.setupAudioAnalysis(peerId, stream);
      };

      pc.oniceconnectionstatechange = () => {
        const state = pc.iceConnectionState;
        if (state === "connected" || state === "completed") {
          let mode = "direct";
          const sel = pc.getReceivers()[0]?.transport?.getSelectedCandidatePair?.();
          const local = sel?.local ?? pc.sctp?.transport?.getSelectedCandidatePair?.()?.local;
          if (local && local.candidateType === "relay") mode = "relay";
          this.setPeerMode(peerId, mode);
        } else if (state === "disconnected" || state === "failed") {
          this.setPeerMode(peerId, "reconnecting...");
          try { pc.restartIce(); } catch (err) { console.error("restart ice failed", err); }
        }
      };

      this.pcs.set(peerId, pc);

      pc.onnegotiationneeded = () => this.makeOffer(peerId, pc);

      return pc;
    },

    async startMedia() {
      if (!navigator.mediaDevices?.getUserMedia) {
        this.notify("браузер не поддерживает захват камеры и микрофона");
        return;
      }

      const stream = new MediaStream();

      let combined = null;
      try {
        combined = await navigator.mediaDevices.getUserMedia({
          video: this.videoId ? { deviceId: { exact: this.videoId } } : true,
          audio: this.audioInputId ? { deviceId: { exact: this.audioInputId } } : true,
        });
      } catch (err) {
        console.warn("совместный запрос медиа не удался", err);
      }

      if (combined) {
        combined.getTracks().forEach((t) => stream.addTrack(t));
      } else {
        for (const kind of ["video", "audio"]) {
          try {
            stream.addTrack(await this.acquireTrack(kind));
          } catch (err) {
            if (kind === "video") this.camOn = false;
            else this.micOn = false;
            this.notify(kind === "video" ? "камера недоступна" : "микрофон недоступен");
          }
        }
      }

      if (!stream.getTracks().length) {
        this.notify("не удалось получить доступ к камере и микрофону");
        return;
      }

      this.localStream = stream;
      await this.enumerateDevices({ resetInvalid: true });
      stream.getVideoTracks().forEach((t) => (t.enabled = this.camOn));
      stream.getAudioTracks().forEach((t) => (t.enabled = this.micOn));

      this.myColor = colorFor(this.name || "me");
      this.initial = (this.name || "Я").slice(0, 1).toUpperCase();
      this.setupAudioAnalysis("local", stream);

      this.$nextTick(() => {
        const v = document.getElementById("vid-local");
        if (v) v.srcObject = stream;
        this.applyAudioOutput();
      });
    },

    applyAudioOutput() {
      if (!this.audioOutputId || typeof HTMLVideoElement.prototype.setSinkId !== "function") return;
      document.querySelectorAll("#videos video").forEach((v) => {
        v.setSinkId(this.audioOutputId).catch(() => {});
      });
    },

    setupAudioAnalysis(id, stream) {
      if (!stream.getAudioTracks().length) return;
      try {
        if (!audioCtx) {
          const Ctx = window.AudioContext || window.webkitAudioContext;
          if (!Ctx) return;
          audioCtx = new Ctx();
        }
        if (audioCtx.state === "suspended") audioCtx.resume().catch(() => {});
        const source = audioCtx.createMediaStreamSource(stream);
        const analyser = audioCtx.createAnalyser();
        analyser.fftSize = 512;
        source.connect(analyser);
        analysers.set(id, {
          analyser,
          data: new Float32Array(analyser.fftSize),
          level: 0,
        });
      } catch (err) {
        console.warn("audio analysis setup failed", err);
      }
    },

    stopAudioAnalysis(id) {
      analysers.delete(id);
      this.setSpeaking(id, false);
    },

    startSpeakingLoop() {
      if (speakTimer) return;
      speakTimer = setInterval(() => this.detectSpeaking(), 250);
    },

    stopSpeakingLoop() {
      clearInterval(speakTimer);
      speakTimer = null;
      for (const id of [...analysers.keys()]) this.setSpeaking(id, false);
    },

    detectSpeaking() {
      const THRESHOLD = 0.02;
      let loudest = null;
      let loudestLevel = 0;
      for (const [id, entry] of analysers) {
        entry.analyser.getFloatTimeDomainData(entry.data);
        let sum = 0;
        for (let i = 0; i < entry.data.length; i++) sum += entry.data[i] * entry.data[i];
        const rms = Math.sqrt(sum / entry.data.length);
        entry.level = Math.max(rms, entry.level * 0.6);
        this.setSpeaking(id, entry.level > THRESHOLD);
        if (id !== "local" && entry.level > loudestLevel) {
          loudestLevel = entry.level;
          loudest = id;
        }
      }
      if (!SPOTLIGHT_ENABLED || this.spotManual) return;
      const speaker = loudestLevel > THRESHOLD ? loudest : null;
      if (speaker && this.activeId !== speaker) {
        this.activeId = speaker;
        this.spotAuto = true;
      } else if (!speaker && this.activeId && this.spotAuto) {
        this.activeId = null;
        this.spotAuto = false;
      }
    },

    setSpeaking(id, speaking) {
      if (id === "local") {
        this.speaking = speaking;
        return;
      }
      const p = this.peers.find((x) => x.id === id);
      if (p) p.speaking = speaking;
    },

    startNetMonitor() {
      if (netTimer) return;
      netTimer = setInterval(() => this.collectStats(), 2000);
    },

    stopNetMonitor() {
      clearInterval(netTimer);
      netTimer = null;
      this.statsPrev.clear();
      for (const p of this.peers) {
        p.rtt = null;
        p.level = "";
      }
    },

    levelFor(rttMs, lossPct) {
      if ((rttMs == null || rttMs < 150) && (lossPct == null || lossPct < 2)) return "good";
      if ((rttMs == null || rttMs < 400) && (lossPct == null || lossPct < 8)) return "fair";
      return "poor";
    },

    async collectStats() {
      for (const [peerId, pc] of this.pcs) {
        try {
          const report = await pc.getStats();
          let bestPair = null;
          const inboundByKind = {};
          report.forEach((r) => {
            if (r.type === "candidate-pair" && r.state === "succeeded" && r.currentRoundTripTime != null) {
              if (
                !bestPair ||
                (r.selected === true && bestPair.selected !== true) ||
                (r.selected === bestPair.selected &&
                  (r.lastPacketReceivedTimestamp || 0) > (bestPair.lastPacketReceivedTimestamp || 0))
              ) {
                bestPair = r;
              }
            }
            if (r.type === "inbound-rtp" && !r.isRemote) {
              inboundByKind[r.kind] = r;
            }
          });

          let rttMs = null;
          if (bestPair) rttMs = bestPair.currentRoundTripTime * 1000;

          let lossPct = null;
          const inbound = inboundByKind.video || inboundByKind.audio;
          if (inbound) {
            const key = peerId + ":" + inbound.kind;
            const prev = this.statsPrev.get(key);
            const received = inbound.packetsReceived || 0;
            const lost = Math.max(inbound.packetsLost || 0, 0);
            if (prev) {
              const dRecv = received - prev.received;
              const dLost = Math.max(lost - prev.lost, 0);
              const total = dRecv + dLost;
              if (total > 0) lossPct = (dLost / total) * 100;
            }
            this.statsPrev.set(key, { received, lost });
          }

          this.updatePeer(peerId, { rtt: rttMs, level: this.levelFor(rttMs, lossPct) });
        } catch (err) {
          console.warn("stats collection failed", err);
        }
      }
    },

    async acquireTrack(kind) {
      const devId = kind === "video" ? this.videoId : this.audioInputId;
      const attempts = [];
      if (devId) attempts.push({ [kind]: { deviceId: { exact: devId } } });
      attempts.push({ [kind]: true });
      for (const constraints of attempts) {
        try {
          const s = await navigator.mediaDevices.getUserMedia(constraints);
          s.getTracks().forEach((t) => { if (t.kind !== kind) t.stop(); });
          const track = s.getTracks().find((t) => t.kind === kind);
          if (track) return track;
        } catch (err) {
          console.warn(kind + " недоступно", err);
        }
      }
      throw new Error(kind + " недоступно");
    },

    async changeDevice(kind) {
      if (kind === "audiooutput") {
        this.applyAudioOutput();
        return;
      }

      if (kind === "videoinput" && this.screenShare) {
        this.notify("для смены камеры остановите демонстрацию экрана");
        return;
      }

      if (!this.localStream) return;

      const trackKind = kind === "videoinput" ? "video" : "audio";
      let newTrack = null;
      try {
        newTrack = await this.acquireTrack(trackKind);
      } catch (err) {
        console.warn(trackKind + " недоступно", err);
        if (trackKind === "video") { this.camOn = false; this.notify("камера недоступна"); }
        if (trackKind === "audio") { this.micOn = false; this.notify("микрофон недоступен"); }
        return;
      }
      if (!newTrack) return;

      const old = this.localStream.getTracks().find((t) => t.kind === trackKind);
      if (old) {
        old.stop();
        this.localStream.removeTrack(old);
      }
      newTrack.enabled = trackKind === "video" ? this.camOn : this.micOn;
      this.localStream.addTrack(newTrack);

      for (const pc of this.pcs.values()) {
        try {
          await this.replaceOrAddTrack(pc, trackKind, newTrack);
        } catch (err) {
          console.error("replaceTrack failed", err);
        }
      }

      this.$nextTick(() => {
        const v = document.getElementById("vid-local");
        if (v) v.srcObject = this.localStream;
        this.applyAudioOutput();
      });
    },

    getRoomLink() {
      return location.origin + location.pathname + "?room=" + encodeURIComponent(this.room);
    },

    share() {
      const link = this.getRoomLink();
      (async () => {
        try {
          if (navigator.clipboard?.writeText) {
            await navigator.clipboard.writeText(link);
          } else {
            const input = document.createElement("textarea");
            input.value = link;
            input.setAttribute("readonly", "");
            input.style.position = "fixed";
            input.style.opacity = "0";
            document.body.appendChild(input);
            input.select();
            const copied = document.execCommand("copy");
            input.remove();
            if (!copied) throw new Error("copy command failed");
          }
          this.notify("ссылка скопирована!", 2000);
        } catch (err) {
          console.warn("не удалось скопировать ссылку", err);
          this.notify("не удалось скопировать ссылку");
        }
      })();
    },

    selectId(id) {
      if (!SPOTLIGHT_ENABLED) return;
      if (this.activeId === id) {
        this.activeId = null;
        this.spotManual = false;
      } else {
        this.activeId = id;
        this.spotManual = true;
        this.spotAuto = false;
      }
    },

    fullscreen(id) {
      const tile = document.getElementById("tile-" + id);
      if (!tile) return;
      if (document.fullscreenElement) {
        document.exitFullscreen().catch(() => {});
      } else {
        tile.requestFullscreen().catch(() => {});
      }
    },

    send(obj) {
      if (this.ws && this.ws.readyState === WebSocket.OPEN) {
        this.ws.send(JSON.stringify(obj));
      }
    },

    notify(text, ms = 4000) {
      clearTimeout(notifyTimer);
      this.status = text;
      notifyTimer = setTimeout(() => {
        if (this.status === text) {
          this.status = this.connected ? "в созвоне: " + this.room : "не подключено";
        }
      }, ms);
    },

    async join() {
      if (this.connected || this.connecting) return;

      const room = (this.room || "").trim();
      if (!room) {
        this.notify("введите ID комнаты");
        return;
      }

      const url = new URL(location.href);
      url.searchParams.set("room", room);
      history.replaceState(null, "", url);
      this.room = room;
      this.connectionToken += 1;
      const token = this.connectionToken;
      this.connecting = true;

      this.status = "запрос камеры/микрофона...";
      this.myColor = colorFor(this.name || "me");
      this.initial = (this.name || "Я").slice(0, 1).toUpperCase();

      try {
        await this.startMedia();
        this.startSpeakingLoop();
        this.startNetMonitor();

        this.reconnectAttempts = 0;
        await this.connect(token);
      } catch (err) {
        console.error("join failed", err);
        if (token === this.connectionToken) {
          this.teardown();
          this.status = "не удалось подключиться";
        }
      } finally {
        if (token === this.connectionToken) this.connecting = false;
      }
    },

    async connect(token = this.connectionToken) {
      await this.loadIceServers();
      if (token !== this.connectionToken || !this.room) {
        throw new Error("connection cancelled");
      }

      const proto = location.protocol === "https:" ? "wss" : "ws";
      return new Promise((resolve, reject) => {
        let ws = null;
        let settled = false;
        let timeout = null;

        const clearPending = () => {
          if (this.pendingConnectReject === reject) this.pendingConnectReject = null;
        };
        const resolveOnce = () => {
          if (settled) return;
          settled = true;
          clearTimeout(timeout);
          clearPending();
          resolve();
        };
        const rejectOnce = (err) => {
          if (settled) return;
          settled = true;
          clearTimeout(timeout);
          clearPending();
          reject(err);
        };

        try {
          ws = new WebSocket(`${proto}://${location.host}/ws`);
        } catch (err) {
          rejectOnce(err);
          return;
        }

        this.pendingConnectReject = reject;
        this.ws = ws;
        timeout = setTimeout(() => {
          rejectOnce(new Error("WebSocket connection timeout"));
          if (this.ws === ws) this.ws = null;
          ws.onopen = null;
          ws.onmessage = null;
          ws.onerror = null;
          ws.onclose = null;
          try { ws.close(); } catch (_) {}
        }, 10000);

        ws.onopen = () => {
          if (token !== this.connectionToken || this.ws !== ws) {
            rejectOnce(new Error("stale WebSocket connection"));
            ws.close();
            return;
          }
          this.connected = true;
          this.connecting = false;
          this.status = "в созвоне: " + this.room;
          this.send({
            type: "join",
            room: this.room,
            data: { name: this.name, camOn: this.camOn, micOn: this.micOn, screenShare: this.screenShare },
          });
          resolveOnce();
        };

        ws.onerror = () => {
          console.warn("WebSocket error");
        };
        ws.onclose = () => {
          const isCurrent = this.ws === ws;
          if (!settled) rejectOnce(new Error("WebSocket connection closed"));
          if (!isCurrent) return;
          this.ws = null;
          this.handleClose();
        };
        ws.onmessage = (ev) => this.handleWsMessage(ev);
      });
    },

    handleWsMessage(ev) {
      let msg;
      try {
        msg = JSON.parse(ev.data);
      } catch (err) {
        console.warn("malformed signaling message", err);
        return;
      }
      this.handleMessage(msg).catch((err) => console.error("message handling failed", err));
    },

    handleClose() {
      this.connected = false;
      if (!this.room || !this.myId) {
        this.connecting = false;
        this.status = "соединение закрыто";
        return;
      }
      this.connecting = true;
      this.scheduleReconnect();
    },

    scheduleReconnect() {
      if (this.reconnectTimer) return;
      const attempt = this.reconnectAttempts + 1;
      if (attempt > MAX_RECONNECT_ATTEMPTS) {
        this.teardown();
        this.status = "не удалось восстановить соединение";
        return;
      }
      this.reconnectAttempts = attempt;
      this.connecting = true;
      const delay = Math.min(RECONNECT_BASE_DELAY_MS * 2 ** (attempt - 1), RECONNECT_MAX_DELAY_MS);
      this.status = `соединение потеряно, переподключение (${attempt}/${MAX_RECONNECT_ATTEMPTS})...`;
      this.reconnectTimer = setTimeout(() => {
        this.reconnectTimer = null;
        if (!this.room || !this.myId) {
          this.connecting = false;
          return;
        }
        this.connect(this.connectionToken).catch((err) => {
          console.warn("reconnect failed", err);
          if (this.room && this.myId && !this.reconnectTimer) this.scheduleReconnect();
        });
      }, delay);
    },

    parseInfo(raw) {
      if (!raw) return {};
      if (typeof raw === "string") {
        try {
          return JSON.parse(raw);
        } catch {
          return {};
        }
      }
      return raw;
    },

    async handleMessage(msg) {
      switch (msg.type) {
        case "joined": {
          this.myId = msg.data.id;
          this.reconnectAttempts = 0;
          document.title = "Sozvon — " + this.room;
          const peersList = msg.data.peers || [];
          const alive = new Set(peersList.map((p) => p.id));
          alive.add(this.myId);
          for (const [id, pc] of this.pcs) {
            if (!alive.has(id)) {
              pc.close();
              this.pcs.delete(id);
              this.pendingIce.delete(id);
              this.stopAudioAnalysis(id);
            }
          }
          this.peers = this.peers.filter((p) => alive.has(p.id));
          for (const p of peersList) {
            const info = this.parseInfo(p);
            if (p.id === this.myId) continue;
            const old = this.pcs.get(p.id);
            if (old) {
              old.close();
              this.pcs.delete(p.id);
              this.pendingIce.delete(p.id);
              this.stopAudioAnalysis(p.id);
            }
            this.addPeer(p.id, info);
            this.createPeer(p.id, true);
          }
          break;
        }

        case "user_joined": {
          const peerId = msg.from;
          const info = this.parseInfo(msg.data);
          this.addPeer(peerId, info);
          this.createPeer(peerId, false);
          break;
        }

        case "user_left": {
          this.removePeer(msg.from);
          break;
        }

        case "state": {
          const peerId = msg.from;
          const info = this.parseInfo(msg.data);
          this.updatePeer(peerId, info);
          break;
        }

        case "offer": {
          const peerId = msg.from;
          let pc = this.pcs.get(peerId);
          if (!pc) {
            this.addPeer(peerId, {});
            pc = this.createPeer(peerId, false);
          }
          try {
            const colliding = pc._makingOffer || pc.signalingState !== "stable";
            pc._ignoreOffer = !pc._polite && colliding;
            if (pc._ignoreOffer) break;
            if (colliding) {
              await pc.setLocalDescription({ type: "rollback" });
            }
            await pc.setRemoteDescription(new RTCSessionDescription(msg.data));
            const answer = await pc.createAnswer();
            await pc.setLocalDescription(answer);
            this.send({ type: "answer", to: peerId, data: answer });
            await this.flushIceQueue(peerId);
          } catch (err) {
            console.error("answer failed", err);
          }
          break;
        }

        case "answer": {
          const peerId = msg.from;
          const pc = this.pcs.get(peerId);
          if (pc) {
            try {
              await pc.setRemoteDescription(new RTCSessionDescription(msg.data));
            } catch (err) {
              console.error("setRemoteDescription(answer) failed", err);
            }
            await this.flushIceQueue(peerId);
          }
          break;
        }

        case "candidate": {
          const peerId = msg.from;
          if (!msg.data) break;
          const pc = this.pcs.get(peerId);
          if (!pc || !pc.remoteDescription) {
            this.queueIce(peerId, msg.data);
            break;
          }
          try {
            await pc.addIceCandidate(new RTCIceCandidate(msg.data));
          } catch (err) {
            console.error("addIceCandidate failed", err);
          }
          break;
        }
      }
    },

    teardown() {
      this.connectionToken += 1;
      this.connecting = false;
      if (this.reconnectTimer) {
        clearTimeout(this.reconnectTimer);
        this.reconnectTimer = null;
      }
      if (this.pendingConnectReject) {
        const reject = this.pendingConnectReject;
        this.pendingConnectReject = null;
        reject(new Error("connection cancelled"));
      }
      const screenStream = this.screenStream;
      const savedMic = this._savedMicTrack;
      const screenAudio = this._screenAudioTrack;
      this.screenStream = null;
      this.screenShare = false;
      this._screenAudioTrack = null;
      this._savedMicTrack = null;
      this._camWasOn = false;
      screenStream?.getTracks().forEach((t) => {
        t.onended = null;
        t.stop();
      });
      if (screenAudio && !screenStream?.getTracks().includes(screenAudio)) screenAudio.stop();
      if (savedMic && !this.localStream?.getTracks().includes(savedMic)) savedMic.stop();
      if (this.ws) {
        const ws = this.ws;
        this.ws = null;
        ws.onclose = null;
        ws.onmessage = null;
        ws.close();
      }
      for (const pc of this.pcs.values()) pc.close();
      this.pcs.clear();
      this.pendingIce.clear();
      if (this.localStream) {
        this.localStream.getTracks().forEach((t) => t.stop());
        this.localStream = null;
      }
      this.peers = [];
      this.myId = null;
      this.connected = false;
      this.reconnectAttempts = 0;
      this.stopSpeakingLoop();
      this.stopNetMonitor();
      analysers.clear();
      if (audioCtx) {
        audioCtx.close().catch(() => {});
        audioCtx = null;
      }
      this.speaking = false;
      this.spotAuto = false;
      this.activeId = null;
      document.title = "Sozvon";
    },

    leave() {
      this.teardown();
      this.status = "не подключено";
    },

    broadcastState() {
      this.send({
        type: "state",
        data: { name: this.name, camOn: this.camOn, micOn: this.micOn, screenShare: this.screenShare },
      });
    },

    async toggleMic() {
      const track = this.localStream?.getAudioTracks()[0];
      if (!track) {
        this.notify(this.connected ? "микрофон недоступен" : "сначала войдите в созвон");
        return;
      }
      track.enabled = !track.enabled;
      this.micOn = track.enabled;
      this.broadcastState();
    },

    async toggleCam() {
      if (this.screenShare) {
        await this.toggleScreen();
        return;
      }
      const track = this.localStream?.getVideoTracks()[0];
      if (!track) {
        this.notify(this.connected ? "камера недоступна" : "сначала войдите в созвон");
        return;
      }
      track.enabled = !track.enabled;
      this.camOn = track.enabled;
      this.broadcastState();
    },

    async toggleScreen() {
      if (this.screenShare) {
        await this.stopScreenShare();
        return;
      }

      if (!navigator.mediaDevices?.getDisplayMedia) {
        this.notify("демонстрация экрана не поддерживается в этом браузере");
        return;
      }
      if (!this.localStream) {
        this.notify("сначала войдите в созвон");
        return;
      }
      let ss = null;
      try {
        ss = await navigator.mediaDevices.getDisplayMedia({ video: true, audio: true });
        const screenTrack = ss.getVideoTracks()[0];
        if (!screenTrack) throw new Error("screen video track is unavailable");

        const oldVideo = this.localStream.getVideoTracks()[0] ?? null;
        const screenAudio = ss.getAudioTracks()[0] ?? null;
        const oldAudio = screenAudio ? (this.localStream.getAudioTracks()[0] ?? null) : null;

        this.screenStream = ss;
        this.screenShare = true;
        this._camWasOn = this.camOn;
        this.camOn = true;
        this._screenAudioTrack = screenAudio;
        this._savedMicTrack = oldAudio;
        screenTrack.onended = () => this.stopScreenShare();

        if (oldVideo) {
          this.localStream.removeTrack(oldVideo);
          oldVideo.stop();
        }
        this.localStream.addTrack(screenTrack);
        screenTrack.contentHint = "detail";
        this.applyVideoBitrate(4000000);

        if (screenAudio) {
          if (oldAudio) this.localStream.removeTrack(oldAudio);
          screenAudio.enabled = this.micOn;
          this.localStream.addTrack(screenAudio);
        }

        for (const pc of this.pcs.values()) {
          try {
            await this.replaceOrAddTrack(pc, "video", screenTrack);
          } catch (err) {
            console.error("screen replaceTrack(video) failed", err);
          }

          if (screenAudio) {
            try {
              await this.replaceOrAddTrack(pc, "audio", screenAudio);
            } catch (err) {
              console.error("screen replaceTrack(audio) failed", err);
            }
          }
        }

        this.$nextTick(() => {
          const v = document.getElementById("vid-local");
          if (v) v.srcObject = this.localStream;
        });

        this.broadcastState();
      } catch (err) {
        console.warn("screen share failed", err);
        if (this.screenShare) {
          await this.stopScreenShare();
        } else {
          ss?.getTracks().forEach((t) => {
            t.onended = null;
            t.stop();
          });
        }
        this.notify("не удалось начать демонстрацию экрана");
      }
    },

    async stopScreenShare() {
      if (!this.screenShare) return;
      this.screenShare = false;
      const ss = this.screenStream;
      this.screenStream = null;
      this.camOn = this._camWasOn ?? false;
      this._camWasOn = false;
      ss?.getTracks().forEach((t) => {
        t.onended = null;
        t.stop();
      });
      this.applyVideoBitrate(2000000);
      await this.replaceVideoTrack();
      await this.replaceAudioTrack();
      this.broadcastState();
    },

    async replaceVideoTrack() {
      if (!this.localStream) return;
      const oldVideo = this.localStream.getVideoTracks()[0] ?? null;
      if (oldVideo) {
        this.localStream.removeTrack(oldVideo);
        oldVideo.stop();
      }

      try {
        const vc = this.videoId ? { deviceId: { exact: this.videoId } } : true;
        const cs = await navigator.mediaDevices.getUserMedia({ video: vc });
        const newTrack = cs.getVideoTracks()[0];
        if (!newTrack) throw new Error("camera track is unavailable");
        newTrack.enabled = this.camOn;
        newTrack.contentHint = "motion";
        this.localStream.addTrack(newTrack);

        for (const pc of this.pcs.values()) {
          await this.replaceOrAddTrack(pc, "video", newTrack);
        }

        this.$nextTick(() => {
          const v = document.getElementById("vid-local");
          if (v) v.srcObject = this.localStream;
        });
      } catch (err) {
        console.warn("replace video track failed", err);
        this.camOn = false;
        for (const pc of this.pcs.values()) {
          try { await this.replaceOrAddTrack(pc, "video", null); } catch (_) {}
        }
      }
    },

    async replaceAudioTrack() {
      const screenAudio = this._screenAudioTrack;
      if (!screenAudio || !this.localStream) return;

      try {
        if (this.localStream.getAudioTracks().includes(screenAudio)) {
          this.localStream.removeTrack(screenAudio);
        }

        const savedMic = this._savedMicTrack;
        const restoredMic = savedMic && savedMic.readyState !== "ended" ? savedMic : null;
        if (restoredMic) {
          restoredMic.enabled = this.micOn;
          this.localStream.addTrack(restoredMic);
        } else {
          this.micOn = false;
        }

        for (const pc of this.pcs.values()) {
          await this.replaceOrAddTrack(pc, "audio", restoredMic);
        }
      } catch (err) {
        console.warn("replace audio track failed", err);
      } finally {
        this._screenAudioTrack = null;
        this._savedMicTrack = null;
      }
    },
  };
}
