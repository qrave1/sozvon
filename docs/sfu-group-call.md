# SFU group-call behavior

Each client keeps dedicated outbound audio and video senders, even when a device
is initially unavailable. Incoming participant tracks use separate receive-only
transceivers in the browser. With N participants publishing audio and video, each
participant receives 2 * (N - 1) tracks.

The initial client offer establishes the transport and publication channels. The
server sends `joined` with its answer before negotiating subscriptions in a new
server offer. An SDP answer cannot introduce extra media sections beyond the offer.
Subscription changes during an outstanding offer remain pending until its answer.

| Scenario | Expected behavior | Verification |
| --- | --- | --- |
| Fourth participant joins three publishers | Receives all three audio and video tracks | Go integration and Chromium |
| Camera unavailable on join, enabled later | Uses the original outbound video sender | Chromium |
| Camera/device replaced in a group | Other participants keep decoding the same publication | Chromium with fake camera |
| Camera disabled | Remote audio keeps playing independently | Chromium |
| Bitrate changed while connected | Active sender parameters update immediately | Chromium |
| Camera replaced with screen, then restored | Remote video keeps decoding | Chromium with canvas screen source |
| Participant leaves and rejoins | Old streams disappear, new streams reach everyone | Go integration and Chromium |
| Chat message sent in a room | Current participants receive it; later participants receive unexpired history | Go integration |
| Camera acquisition finishes after leave | Acquired track stops and does not restore departed UI | Chromium |
| Subscriber requests a keyframe | PLI/FIR becomes PLI addressed to the publisher's SSRC | Go RTCP integration |
| Producer RTP contains transport extensions | Incoming MID/RID/transport extension IDs do not leak downstream | Go RTP integration |

Default Pion interceptors handle downstream retransmissions. Subscriber keyframe
feedback is explicitly routed upstream; transport-scoped extensions are stripped
before forwarding RTP. Device and screen changes also request a fresh video frame.

SFU chat history is held in server memory for 24 hours. A joining participant
receives the latest 100 unexpired messages for the room. History is lost when
the server process restarts; at most 1,000 room histories are retained, and
the oldest room history is evicted when that limit is exceeded. Empty and
messages longer than 2,000 Unicode code points are rejected. Expired history
is pruned when accessed and when capacity is reached.

## Run Checks

```powershell
go test ./... -count=1
go build -o .sfu-check.exe .
node --test tools/sfu_group.test.cjs
```

The browser check requires Chrome and an installed `playwright` module. Set
`PLAYWRIGHT_MODULE_PATH` to its absolute module path when using a bundled runtime.
The test starts and stops its own local server on HTTP 18003 / UDP 40003 and uses
isolated browser contexts with fake media devices. `SFU_TEST_BINARY` and
`SFU_TEST_HTTP_PORT` override the binary and HTTP port.

## Scope

Browser checks verify packet arrival, increasing decoded-frame counters and audio
playback. They do not verify production TURN traversal, physical camera switching
or Safari on a real phone. Screen capture uses a canvas track rather than the OS
picker. Screen sharing currently replaces camera video. Simultaneous camera and
screen publications, simulcast, subscriber bandwidth adaptation and automatic
ICE restart are separate work needed for a broader Discord-like experience.
