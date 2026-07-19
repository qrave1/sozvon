(() => {
  "use strict";

  let ICE_SERVERS = [
    { urls: "stun:stun.l.google.com:19302" },
  ];

  async function loadIceServers() {
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
      ICE_SERVERS = servers;
    } catch (err) {
      console.warn("failed to load TURN config, using STUN only", err);
    }
  }

  const roomInput = document.getElementById("room");
  const joinBtn = document.getElementById("join");
  const leaveBtn = document.getElementById("leave");
  const micBtn = document.getElementById("mic");
  const camBtn = document.getElementById("cam");
  const statusEl = document.getElementById("status");
  const videosEl = document.getElementById("videos");

  let ws = null;
  let localStream = null;
  let myId = null;
  const peers = new Map();

  function setStatus(text) {
    statusEl.textContent = text;
  }

  function addTile(id, stream) {
    let tile = document.getElementById("tile-" + id);
    if (!tile) {
      tile = document.createElement("div");
      tile.className = "tile";
      tile.id = "tile-" + id;

      const video = document.createElement("video");
      video.autoplay = true;
      video.playsInline = true;

      const label = document.createElement("div");
      label.className = "label";
      label.textContent = id.slice(0, 8);

      tile.appendChild(video);
      tile.appendChild(label);
      videosEl.appendChild(tile);
    }

    const video = tile.querySelector("video");
    if (stream) video.srcObject = stream;
  }

  function removeTile(id) {
    const tile = document.getElementById("tile-" + id);
    if (tile) tile.remove();
  }

  function clearTiles() {
    videosEl.innerHTML = "";
  }

  function send(obj) {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(obj));
    }
  }

  function createPeer(peerId, initiator) {
    const pc = new RTCPeerConnection({ iceServers: ICE_SERVERS });

    if (localStream) {
      for (const track of localStream.getTracks()) {
        pc.addTrack(track, localStream);
      }
    }

    pc.onicecandidate = (e) => {
      if (e.candidate) {
        send({ type: "candidate", to: peerId, data: e.candidate });
      }
    };

    pc.ontrack = (e) => {
      const [stream] = e.streams;
      addTile(peerId, stream);
    };

    peers.set(peerId, pc);

    if (initiator) {
      pc.onnegotiationneeded = async () => {
        try {
          const offer = await pc.createOffer();
          await pc.setLocalDescription(offer);
          send({ type: "offer", to: peerId, data: offer });
        } catch (err) {
          console.error("offer failed", err);
        }
      };
    }

    return pc;
  }

  async function startMedia() {
    localStream = await navigator.mediaDevices.getUserMedia({ video: true, audio: true });
    addTile("local", localStream);
  }

  async function joinRoom() {
    const room = roomInput.value.trim();
    if (!room) return;

    setStatus("запрос камеры/микрофона...");
    try {
      await startMedia();
    } catch (err) {
      setStatus("нет доступа к камере/микрофону");
      console.error(err);
      return;
    }

    const proto = location.protocol === "https:" ? "wss" : "ws";
    ws = new WebSocket(`${proto}://${location.host}/ws`);

    await loadIceServers();

    ws.onopen = () => {
      setStatus("в созвоне: " + room);
      joinBtn.disabled = true;
      leaveBtn.disabled = false;
      roomInput.disabled = true;
      send({ type: "join", room });
    };

    ws.onclose = () => {
      setStatus("соединение закрыто");
    };

    ws.onmessage = async (ev) => {
      const msg = JSON.parse(ev.data);
      await handleMessage(msg);
    };
  }

  async function handleMessage(msg) {
    switch (msg.type) {
      case "joined": {
        myId = msg.data.id;
        const peersList = msg.data.peers || [];
        for (const peerId of peersList) {
          if (peerId === myId) continue;
          createPeer(peerId, true);
        }
        break;
      }

      case "user_joined": {
        const peerId = msg.from;
        createPeer(peerId, false);
        break;
      }

      case "user_left": {
        const peerId = msg.from;
        const pc = peers.get(peerId);
        if (pc) {
          pc.close();
          peers.delete(peerId);
        }
        removeTile(peerId);
        break;
      }

      case "offer": {
        const peerId = msg.from;
        let pc = peers.get(peerId);
        if (!pc) pc = createPeer(peerId, false);
        await pc.setRemoteDescription(new RTCSessionDescription(msg.data));
        const answer = await pc.createAnswer();
        await pc.setLocalDescription(answer);
        send({ type: "answer", to: peerId, data: answer });
        break;
      }

      case "answer": {
        const peerId = msg.from;
        const pc = peers.get(peerId);
        if (pc) await pc.setRemoteDescription(new RTCSessionDescription(msg.data));
        break;
      }

      case "candidate": {
        const peerId = msg.from;
        const pc = peers.get(peerId);
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
  }

  function leaveRoom() {
    if (ws) {
      ws.close();
      ws = null;
    }
    for (const pc of peers.values()) pc.close();
    peers.clear();
    if (localStream) {
      localStream.getTracks().forEach((t) => t.stop());
      localStream = null;
    }
    clearTiles();
    myId = null;
    joinBtn.disabled = false;
    leaveBtn.disabled = true;
    roomInput.disabled = false;
    setStatus("не подключено");
  }

  joinBtn.onclick = joinRoom;
  leaveBtn.onclick = leaveRoom;

  micBtn.onclick = () => {
    if (!localStream) return;
    const track = localStream.getAudioTracks()[0];
    if (!track) return;
    track.enabled = !track.enabled;
    micBtn.textContent = "Микрофон: " + (track.enabled ? "вкл" : "выкл");
  };

  camBtn.onclick = () => {
    if (!localStream) return;
    const track = localStream.getVideoTracks()[0];
    if (!track) return;
    track.enabled = !track.enabled;
    camBtn.textContent = "Камера: " + (track.enabled ? "вкл" : "выкл");
  };
})();
