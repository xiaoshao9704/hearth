## Introduction

Hearth is a self-hosted room for voice, screen sharing, OBS ingest and chat — a private living room for a
handful of friends: open a channel, get a few people in, put your screen up.

One container holds all of it: API, media core, ingest endpoint and web UI. No redis, no second service,
no separate media server to install.

## Features

- **Voice**: group voice forwarded by an SFU, with noise suppression, push-to-talk, auto-mute when idle, and one account present from several devices at once
- **Screen sharing and ingest**: 1080p60 screen sharing at a real bitrate, system audio included; the camera rides the same line. OBS pushes in over plain WHIP with no plugin and the stream is forwarded untouched, so HEVC and AV1 pass through
- **Chat**: mentions, replies, reactions and direct file transfer, with history replayed from the server
- **Accounts and roles**: passkey login, invite-only registration by default, first account automatically becomes super admin
- **Built-in https**: one port serves both http and https; the certificate comes from a self-signed root, an external PEM file, or a console upload

## After installing

Create the first account (it automatically becomes super admin), from 1Panel's container terminal or over SSH:

```bash
docker exec -it 1Panel-hearth-<suffix> /app/hearth adduser alice change-me
```

Use the container name shown in the app list. Then open `http://<host>:<web port>`.

**Other devices on the LAN must install the root certificate once** at `https://<host>:<web port>/ca`
(plain http works too). Switch to the https address afterwards — microphone, screen share and
notifications all need a secure context.

## Ports

| Port | Purpose |
|---|---|
| web port (8080/tcp by default) | web UI, API, signalling and WHIP; also accepts https on the same port |
| 47720/udp | media port (voice and screen share share it), **must** be open on your firewall |
| 47720/tcp | ICE-TCP fallback, only needed where UDP is intercepted by middleboxes |

The media port is fixed at 47720 rather than exposed as a form field: the in-process media core writes the
UDP port it listens on into its ICE candidates, so a host port that differs from the container port would
advertise an unreachable one. Changing it means editing both the compose port mapping and
"Stage → media UDP port" in the Hearth admin console, keeping the two in sync.
