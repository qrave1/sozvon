function callApp() {
  "use strict";

  const ICE_SERVERS = [
    { urls: "stun:stun.l.google.com:19302" },
  ];

  const COLORS = [
    "#e74c3c", "#3498db", "#2ecc71", "#9b59b6", "#f1c40f",
    "#1abc9c", "#e67e22", "#34495e", "#fd79a8", "#00cec9",
  ];

  function colorFor(id) {
    let h = 0;
    for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
    return COLORS[h % COLORS.length];
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

    audioInputId: "",
    audioOutputId: "",
    videoId: "",
    devices: { audioinput: [], audiooutput: [], videoinput: [] },

    async init() {
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
        if (video && stream) video.srcObject = stream;
      });
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
        });
      }
    },

    removePeer(id) {
      this.peers = this.peers.filter((p) => p.id !== id);
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

    createPeer(peerId, initiator) {
      const pc = new RTCPeerConnection({ iceServers: this.iceServers });

      if (this.localStream) {
        for (const track of this.localStream.getTracks()) {
          pc.addTrack(track, this.localStream);
        }
      }

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
          try {
            const offer = await pc.createOffer();
            await pc.setLocalDescription(offer);
            this.send({ type: "offer", to: peerId, data: offer });
          } catch (err) {
            console.error("offer failed", err);
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
        if (this.videoId) {
          const vs = await navigator.mediaDevices.getUserMedia({ video: { deviceId: { exact: this.videoId } } });
          vs.getVideoTracks().forEach((t) => stream.addTrack(t));
        } else {
          const vs = await navigator.mediaDevices.getUserMedia({ video: true });
          vs.getVideoTracks().forEach((t) => stream.addTrack(t));
        }
      } catch (err) {
        console.warn("видео недоступно", err);
        this.camOn = false;
      }

      try {
        if (this.audioInputId) {
          const as = await navigator.mediaDevices.getUserMedia({ audio: { deviceId: { exact: this.audioInputId } } });
          as.getAudioTracks().forEach((t) => stream.addTrack(t));
        } else {
          const as = await navigator.mediaDevices.getUserMedia({ audio: true });
          as.getAudioTracks().forEach((t) => stream.addTrack(t));
        }
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

    async changeDevice(kind) {
      if (kind === "audiooutput") {
        this.applyAudioOutput();
        return;
      }

      if (!this.connected) return;

      if (this.localStream) {
        this.localStream.getTracks().forEach((t) => t.stop());
      }

      const stream = new MediaStream();

      if (kind === "videoinput" || kind === "audioinput") {
        try {
          const c = kind === "videoinput"
            ? { video: this.videoId ? { deviceId: { exact: this.videoId } } : true }
            : { audio: this.audioInputId ? { deviceId: { exact: this.audioInputId } } : true };
          const s = await navigator.mediaDevices.getUserMedia(c);
          s.getTracks().forEach((t) => stream.addTrack(t));
        } catch (err) {
          console.warn(`${kind} недоступен`, err);
          if (kind === "videoinput") this.camOn = false;
          if (kind === "audioinput") this.micOn = false;
        }
        // Keep the other kind's existing tracks
        if (this.localStream) {
          const keepKind = kind === "videoinput" ? "audio" : "video";
          this.localStream.getTracks().filter((t) => t.kind === keepKind).forEach((t) => stream.addTrack(t));
        }
      }

      for (const pc of this.pcs.values()) {
        const senders = pc.getSenders();
        for (const newTrack of stream.getTracks()) {
          const sender = senders.find((s) => s.track?.kind === newTrack.kind);
          if (sender) {
            await sender.replaceTrack(newTrack);
          } else {
            pc.addTrack(newTrack, stream);
          }
        }
      }

      stream.getVideoTracks().forEach((t) => (t.enabled = this.camOn));
      stream.getAudioTracks().forEach((t) => (t.enabled = this.micOn));

      this.localStream = stream;

      this.$nextTick(() => {
        const v = document.getElementById("vid-local");
        if (v) v.srcObject = stream;
      });

      if (this.pcs.size > 0) await this.negotiateAll();
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
          data: { name: this.name, camOn: this.camOn, micOn: this.micOn },
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
        this.send({ type: "join", room: this.room, data: { name: this.name, camOn: this.camOn, micOn: this.micOn } });
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
          for (const p of peersList) {
            const info = this.parseInfo(p);
            if (p.id === this.myId) continue;
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
          await pc.setRemoteDescription(new RTCSessionDescription(msg.data));
          const answer = await pc.createAnswer();
          await pc.setLocalDescription(answer);
          this.send({ type: "answer", to: peerId, data: answer });
          break;
        }

        case "answer": {
          const peerId = msg.from;
          const pc = this.pcs.get(peerId);
          if (pc) await pc.setRemoteDescription(new RTCSessionDescription(msg.data));
          break;
        }

        case "candidate": {
          const peerId = msg.from;
          const pc = this.pcs.get(peerId);
          if (pc && msg.data) {
            try {
              await pc.addIceCandidate(new RTCIceCandidate(msg.data));
            } catch (err) {
              console.error("addIceCandidate failed", err);
            }
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
        data: { name: this.name, camOn: this.camOn, micOn: this.micOn },
      });
    },

    async negotiateAll() {
      for (const [peerId, pc] of this.pcs) {
        try {
          const offer = await pc.createOffer();
          await pc.setLocalDescription(offer);
          this.send({ type: "offer", to: peerId, data: offer });
        } catch (err) {
          console.error("renegotiate failed", err);
        }
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
      if (!this.localStream) return;
      const track = this.localStream.getVideoTracks()[0];
      if (!track) return;
      track.enabled = !track.enabled;
      this.camOn = track.enabled;
      if (this.pcs.size > 0) await this.negotiateAll();
      this.broadcastState();
    },
  };
}
