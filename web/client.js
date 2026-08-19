function callApp() {
  "use strict";

  const ICE_SERVERS = [
    { urls: "stun:stun.l.google.com:19302" },
  ];

  const COLORS = [
    "#e74c3c", "#3498db", "#2ecc71", "#9b59b6", "#f1c40f",
    "#1abc9c", "#e67e22", "#34495e", "#fd79a8", "#00cec9",
  ];

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
    iceServers: ICE_SERVERS,
    pcs: new Map(),
    mutedPeers: new Set(),
    pendingIce: new Map(),
    negotiating: false,

    audioInputId: "",
    audioOutputId: "",
    videoId: "",
    activeId: null,
    showSettings: false,
    screenShare: false,
    devices: { audioinput: [], audiooutput: [], videoinput: [] },

    async init() {
      const roomFromUrl = getRoomFromUrl();
      if (roomFromUrl) this.room = roomFromUrl;

      window.addEventListener("beforeunload", () => this.leave());
      await this.enumerateDevices();
      navigator.mediaDevices?.addEventListener("devicechange", () => {
        this.enumerateDevices();
      });
    },

    async enumerateDevices() {
      if (!navigator.mediaDevices?.enumerateDevices) return;
      const all = await navigator.mediaDevices.enumerateDevices();
      const grouped = { audioinput: [], audiooutput: [], videoinput: [] };
      for (const d of all) {
        if (d.kind in grouped) grouped[d.kind].push(d);
      }
      this.devices = grouped;
      if (!this.audioInputId && grouped.audioinput.length) this.audioInputId = grouped.audioinput[0].deviceId;
      if (!this.audioOutputId && grouped.audiooutput.length) this.audioOutputId = grouped.audiooutput[0].deviceId;
      if (!this.videoId && grouped.videoinput.length) this.videoId = grouped.videoinput[0].deviceId;
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
        }
      });
    },

    queueIce(id, candidate) {
      if (!this.pendingIce.has(id)) this.pendingIce.set(id, []);
      this.pendingIce.get(id).push(candidate);
    },

    flushIceQueue(id) {
      const queue = this.pendingIce.get(id);
      if (!queue) return;
      this.pendingIce.delete(id);
      const pc = this.pcs.get(id);
      if (!pc) return;
      for (const c of queue) {
        try {
          pc.addIceCandidate(new RTCIceCandidate(c));
        } catch (err) {
          console.error("addIceCandidate failed", err);
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
        });
      }
    },

    removePeer(id) {
      this.peers = this.peers.filter((p) => p.id !== id);
      this.mutedPeers.delete(id);
      this.pendingIce.delete(id);
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

    applyVideoBitrate(bitrate) {
      for (const pc of this.pcs.values()) {
        const sender = pc.getSenders().find((s) => s.track?.kind === "video");
        if (sender) setVideoBitrate(sender, bitrate);
      }
    },

    createPeer(peerId, initiator) {
      const pc = new RTCPeerConnection({ iceServers: this.iceServers });
      pc._polite = !initiator;
      pc._makingOffer = false;

      if (this.localStream) {
        for (const track of this.localStream.getTracks()) {
          pc.addTrack(track, this.localStream);
        }
      }

      this.applyVideoRestrictions(pc);

      pc.onicecandidate = (e) => {
        if (e.candidate) {
          this.send({ type: "candidate", to: peerId, data: e.candidate });
        }
      };

      pc.ontrack = (e) => {
        const [stream] = e.streams;
        this.attachStream(peerId, stream);
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

      if (initiator) {
        pc.onnegotiationneeded = async () => {
          if (pc._makingOffer) return;
          try {
            pc._makingOffer = true;
            await pc.setLocalDescription();
            this.send({ type: "offer", to: peerId, data: pc.localDescription });
          } catch (err) {
            console.error("offer failed", err);
          } finally {
            pc._makingOffer = false;
          }
        };
      }

      return pc;
    },

    async startMedia() {
      if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
        return;
      }

      const stream = new MediaStream();

      try {
        const vc = this.videoId ? { deviceId: { exact: this.videoId } } : true;
        const vs = await navigator.mediaDevices.getUserMedia({ video: vc });
        vs.getVideoTracks().forEach((t) => stream.addTrack(t));
      } catch (err) {
        console.warn("видео недоступно", err);
        this.camOn = false;
      }

      try {
        const ac = this.audioInputId ? { deviceId: { exact: this.audioInputId } } : true;
        const as = await navigator.mediaDevices.getUserMedia({ audio: ac });
        as.getAudioTracks().forEach((t) => stream.addTrack(t));
      } catch (err) {
        console.warn("микрофон недоступен", err);
        this.micOn = false;
      }

      this.localStream = stream;
      stream.getVideoTracks().forEach((t) => (t.enabled = this.camOn));
      stream.getAudioTracks().forEach((t) => (t.enabled = this.micOn));

      this.myColor = colorFor(this.name || "me");
      this.initial = (this.name || "Я").slice(0, 1).toUpperCase();

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

    acquireTrack(kind) {
      const isVideo = kind === "video";
      const opts = isVideo
        ? { video: this.videoId ? { deviceId: { exact: this.videoId } } : true }
        : { audio: this.audioInputId ? { deviceId: { exact: this.audioInputId } } : true };
      return navigator.mediaDevices.getUserMedia(opts).then((s) => {
        s.getTracks().forEach((t) => { if (t.kind !== kind) t.stop(); });
        return s.getTracks()[0] ?? null;
      });
    },

    async changeDevice(kind) {
      if (kind === "audiooutput") {
        this.applyAudioOutput();
        return;
      }

      if (kind === "videoinput" && this.screenShare) {
        this.status = "для смены камеры остановите демонстрацию экрана";
        return;
      }

      if (!this.localStream) return;

      const trackKind = kind === "videoinput" ? "video" : "audio";
      let newTrack = null;
      try {
        newTrack = await this.acquireTrack(trackKind);
      } catch (err) {
        console.warn(trackKind + " недоступно", err);
        if (trackKind === "video") this.camOn = false;
        if (trackKind === "audio") this.micOn = false;
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

      let added = false;
      for (const pc of this.pcs.values()) {
        const sender = pc.getSenders().find((s) => s.track?.kind === trackKind);
        try {
          if (sender) {
            await sender.replaceTrack(newTrack);
          } else {
            pc.addTrack(newTrack, this.localStream);
            added = true;
          }
        } catch (err) {
          console.error("replaceTrack failed", err);
        }
      }
      if (added) await this.negotiateAll();

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
      navigator.clipboard.writeText(link).then(() => {
        const prev = this.status;
        this.status = "ссылка скопирована!";
        setTimeout(() => { this.status = prev; }, 2000);
      }).catch(() => {
        this.status = "не удалось скопировать";
      });
    },

    selectId(id) {
      this.activeId = this.activeId === id ? null : id;
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

    async join() {
      const room = (this.room || "").trim();
      if (!room) {
        this.status = "введите ID комнаты";
        return;
      }

      this.status = "запрос камеры/микрофона...";
      this.myColor = colorFor(this.name || "me");
      this.initial = (this.name || "Я").slice(0, 1).toUpperCase();
      await this.startMedia();

      await this.loadIceServers();

      const proto = location.protocol === "https:" ? "wss" : "ws";
      this.ws = new WebSocket(`${proto}://${location.host}/ws`);

      this.ws.onopen = () => {
        this.connected = true;
        this.status = "в созвоне: " + room;
        this.send({
          type: "join",
          room,
        data: { name: this.name, camOn: this.camOn, micOn: this.micOn, screenShare: this.screenShare },
        });
      };

      this.ws.onclose = () => this.handleClose();

      this.ws.onmessage = async (ev) => {
        const msg = JSON.parse(ev.data);
        await this.handleMessage(msg);
      };
    },

    handleClose() {
      this.connected = false;
      if (this.room && this.myId) {
        this.status = "соединение потеряно, переподключение...";
        setTimeout(() => this.reconnect(), 2000);
      } else {
        this.status = "соединение закрыто";
      }
    },

    async reconnect() {
      const proto = location.protocol === "https:" ? "wss" : "ws";
      const ws = new WebSocket(`${proto}://${location.host}/ws`);
      ws.onopen = () => {
        this.connected = true;
        this.status = "в созвоне: " + this.room;
        this.send({ type: "join", room: this.room, data: { name: this.name, camOn: this.camOn, micOn: this.micOn, screenShare: this.screenShare } });
      };
      ws.onmessage = (ev) => {
        const msg = JSON.parse(ev.data);
        this.handleMessage(msg);
      };
      ws.onclose = () => this.handleClose();
      this.ws = ws;
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
          const peersList = msg.data.peers || [];
          const alive = new Set(peersList.map((p) => p.id));
          alive.add(this.myId);
          for (const [id, pc] of this.pcs) {
            if (!alive.has(id)) {
              pc.close();
              this.pcs.delete(id);
              this.pendingIce.delete(id);
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
            const info = this.parseInfo(msg.data);
            this.addPeer(peerId, info);
            pc = this.createPeer(peerId, false);
          }
          try {
            const colliding = pc._makingOffer || pc.signalingState !== "stable";
            if (!pc._polite) {
              if (colliding) return;
            } else if (colliding) {
              await pc.setLocalDescription({ type: "rollback" });
            }
            await pc.setRemoteDescription(new RTCSessionDescription(msg.data));
            const answer = await pc.createAnswer();
            await pc.setLocalDescription(answer);
            this.send({ type: "answer", to: peerId, data: answer });
            this.flushIceQueue(peerId);
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
            this.flushIceQueue(peerId);
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

    leave() {
      if (this.ws) {
        this.ws.close();
        this.ws = null;
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
      this.status = "не подключено";
    },

    broadcastState() {
      this.send({
        type: "state",
        data: { name: this.name, camOn: this.camOn, micOn: this.micOn, screenShare: this.screenShare },
      });
    },

    async negotiateAll() {
      if (this.negotiating) return;
      this.negotiating = true;
      try {
        for (const [peerId, pc] of this.pcs) {
          try {
            const offer = await pc.createOffer();
            await pc.setLocalDescription(offer);
            this.send({ type: "offer", to: peerId, data: offer });
          } catch (err) {
            console.error("renegotiate failed", err);
          }
        }
      } finally {
        this.negotiating = false;
      }
    },

    async toggleMic() {
      if (!this.localStream) return;
      const track = this.localStream.getAudioTracks()[0];
      if (!track) return;
      track.enabled = !track.enabled;
      this.micOn = track.enabled;
      this.broadcastState();
    },

    async toggleCam() {
      if (this.screenShare) {
        await this.toggleScreen();
        return;
      }
      if (!this.localStream) return;
      const track = this.localStream.getVideoTracks()[0];
      if (!track) return;
      track.enabled = !track.enabled;
      this.camOn = track.enabled;
      this.broadcastState();
    },

    async toggleScreen() {
      if (this.screenShare) {
        this.screenStream?.getTracks().forEach((t) => t.stop());
        this.screenStream = null;
        this.screenShare = false;
        this.camOn = this._camWasOn ?? false;
        this.applyVideoBitrate(2000000);
        await this.replaceVideoTrack();
        await this.replaceAudioTrack();
        this.broadcastState();
        return;
      }

      if (!navigator.mediaDevices?.getDisplayMedia) return;
      try {
        const ss = await navigator.mediaDevices.getDisplayMedia({ video: true, audio: true });
        this.screenStream = ss;
        this.screenShare = true;
        this._camWasOn = this.camOn;
        this.camOn = true;

        const screenTrack = ss.getVideoTracks()[0];
        const oldVideo = this.localStream.getVideoTracks()[0];
        if (oldVideo) this.localStream.removeTrack(oldVideo);
        this.localStream.addTrack(screenTrack);
        screenTrack.contentHint = "detail";
        this.applyVideoBitrate(4000000);

        const screenAudio = ss.getAudioTracks()[0];
        if (screenAudio) {
          this._savedMicTrack = this.localStream.getAudioTracks()[0] ?? null;
          const oldAudio = this.localStream.getAudioTracks()[0];
          if (oldAudio) this.localStream.removeTrack(oldAudio);
          this.localStream.addTrack(screenAudio);
        }

        for (const pc of this.pcs.values()) {
          try {
            const sender = pc.getSenders().find((s) => s.track?.kind === "video");
            if (sender) await sender.replaceTrack(screenTrack);
          } catch (err) {
            console.error("screen replaceTrack(video) failed", err);
          }

          if (screenAudio) {
            try {
              const audioSender = pc.getSenders().find((s) => s.track?.kind === "audio");
              if (audioSender) await audioSender.replaceTrack(screenAudio);
            } catch (err) {
              console.error("screen replaceTrack(audio) failed", err);
            }
          }
        }

        this.$nextTick(() => {
          const v = document.getElementById("vid-local");
          if (v) v.srcObject = this.localStream;
        });

        screenTrack.onended = async () => {
          this.screenStream = null;
          this.screenShare = false;
          this.camOn = this._camWasOn;
          this.applyVideoBitrate(2000000);
          await this.replaceVideoTrack();
          await this.replaceAudioTrack();
          this.broadcastState();
        };

        this.broadcastState();
      } catch (err) {
        console.warn("screen share failed", err);
      }
    },

    async replaceVideoTrack() {
      try {
        const vc = this.videoId ? { deviceId: { exact: this.videoId } } : true;
        const cs = await navigator.mediaDevices.getUserMedia({ video: vc });
        const newTrack = cs.getVideoTracks()[0];
        newTrack.enabled = this.camOn;
        newTrack.contentHint = "motion";

        const oldVideo = this.localStream.getVideoTracks()[0];
        if (oldVideo) this.localStream.removeTrack(oldVideo);
        if (newTrack) this.localStream.addTrack(newTrack);

        for (const pc of this.pcs.values()) {
          const sender = pc.getSenders().find((s) => s.track?.kind === "video");
          if (sender) await sender.replaceTrack(newTrack);
        }

        this.$nextTick(() => {
          const v = document.getElementById("vid-local");
          if (v) v.srcObject = this.localStream;
        });
      } catch (err) {
        console.warn("replace video track failed", err);
        this.camOn = false;
      }
    },

    async replaceAudioTrack() {
      try {
        const screenAudio = this.localStream.getAudioTracks()[0];
        if (screenAudio) this.localStream.removeTrack(screenAudio);

        const savedMic = this._savedMicTrack;
        if (savedMic) this.localStream.addTrack(savedMic);
        this._savedMicTrack = null;

        for (const pc of this.pcs.values()) {
          const sender = pc.getSenders().find((s) => s.track?.kind === "audio");
          if (sender) await sender.replaceTrack(savedMic ?? null);
        }
      } catch (err) {
        console.warn("replace audio track failed", err);
      }
    },
  };
}
