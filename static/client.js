function callApp() {
  "use strict";

  const ICE_SERVERS = [
    { urls: "stun:stun.l.google.com:19302" },
  ];

  return {
    room: "test",
    status: "не подключено",
    connected: false,
    micOn: true,
    camOn: true,
    localStream: null,
    peers: [],

    ws: null,
    myId: null,
    iceServers: ICE_SERVERS,
    pcs: new Map(),

    async init() {
      window.addEventListener("beforeunload", () => this.leave());
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

    addPeer(id) {
      if (!this.peers.find((p) => p.id === id)) {
        this.peers.push({ id, mode: "" });
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
        if (pc.iceConnectionState === "connected" || pc.iceConnectionState === "completed") {
          let mode = "direct";
          const sel = pc.getReceivers()[0]?.transport?.getSelectedCandidatePair?.();
          const local = sel?.local ?? pc.sctp?.transport?.getSelectedCandidatePair?.()?.local;
          if (local && local.candidateType === "relay") mode = "relay";
          this.setPeerMode(peerId, mode);
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
      this.localStream = await navigator.mediaDevices.getUserMedia({ video: true, audio: true });
      this.$nextTick(() => {
        const v = document.getElementById("vid-local");
        if (v) v.srcObject = this.localStream;
      });
    },

    send(obj) {
      if (this.ws && this.ws.readyState === WebSocket.OPEN) {
        this.ws.send(JSON.stringify(obj));
      }
    },

    async join() {
      const room = (this.room || "").trim();
      if (!room) return;

      this.status = "запрос камеры/микрофона...";
      try {
        await this.startMedia();
      } catch (err) {
        this.status = "нет доступа к камере/микрофону";
        console.error(err);
        return;
      }

      await this.loadIceServers();

      const proto = location.protocol === "https:" ? "wss" : "ws";
      this.ws = new WebSocket(`${proto}://${location.host}/ws`);

      this.ws.onopen = () => {
        this.connected = true;
        this.status = "в созвоне: " + room;
        this.send({ type: "join", room });
      };

      this.ws.onclose = () => {
        this.status = "соединение закрыто";
        this.connected = false;
      };

      this.ws.onmessage = async (ev) => {
        const msg = JSON.parse(ev.data);
        await this.handleMessage(msg);
      };
    },

    async handleMessage(msg) {
      switch (msg.type) {
        case "joined": {
          this.myId = msg.data.id;
          const peersList = msg.data.peers || [];
          for (const peerId of peersList) {
            if (peerId === this.myId) continue;
            this.addPeer(peerId);
            this.createPeer(peerId, true);
          }
          break;
        }

        case "user_joined": {
          const peerId = msg.from;
          this.addPeer(peerId);
          this.createPeer(peerId, false);
          break;
        }

        case "user_left": {
          this.removePeer(msg.from);
          break;
        }

        case "offer": {
          const peerId = msg.from;
          let pc = this.pcs.get(peerId);
          if (!pc) {
            this.addPeer(peerId);
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

    toggleMic() {
      if (!this.localStream) return;
      const track = this.localStream.getAudioTracks()[0];
      if (!track) return;
      track.enabled = !track.enabled;
      this.micOn = track.enabled;
    },

    toggleCam() {
      if (!this.localStream) return;
      const track = this.localStream.getVideoTracks()[0];
      if (!track) return;
      track.enabled = !track.enabled;
      this.camOn = track.enabled;
    },
  };
}
