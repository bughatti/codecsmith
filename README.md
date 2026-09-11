# Transcoder

**One job: make large video files as small as possible while they still look good.**

This is not a general-purpose transcoder. There is no per-file preset picker,
no resolution ladder, no format conversion menu. You point it at your movie
and TV libraries and it works through them, re-encoding each file to HEVC
(or AV1) at a constant-quality setting that keeps the picture good and lets
the file size fall as far as the content allows. A 30 GB Blu-ray remux
becomes 4 to 6 GB. A 4 GB H.264 web release becomes 1.5 to 2 GB. A file that
is already small stays untouched.

It runs as a single container with a dashboard, uses whatever GPU you have
or the CPU if you have none, and never makes a file worse.

## What it does to a file

1. **Finds it.** Every library is walked on a schedule (hourly by default),
   and Sonarr or Radarr can push a file the moment it is imported.
2. **Decides whether it is worth it.** The file is queued when its video is
   not yet in the target codec, or when it already is but is larger than the
   size you consider "big" (`size_limit_gb`). Sources that cannot get smaller
   are skipped up front: AV1 sources, and anything already below a bitrate
   floor you set.
3. **Encodes at constant quality.** Not a target bitrate. The encoder spends
   bits where the picture needs them and saves them where it does not, so a
   dark drama and a bright action film both come out looking right at
   different sizes. Audio is re-encoded to stereo AAC (or copied, your
   choice) and only the languages you want are kept. Subtitles ride along as
   soft tracks.
4. **Checks the result.** If the output is not smaller than the source, the
   source is kept and the job is marked *skipped*. If the output is shorter
   than the source, the source is kept and the job is marked *failed*. Only
   an output that is both smaller and complete replaces the original, and the
   swap is done with a backup held until the new file is in place.

Typical savings on a home library, with the default profile:

| Source | Result |
|---|---|
| 1080p Blu-ray remux (VC-1, MPEG-2, H.264), 20–35 GB | 3–6 GB |
| 1080p H.264 web release, 2–5 GB | 0.8–2 GB |
| 1080p anime H.264, 1–2 GB per episode | 250–600 MB |
| 4K HEVC 50–70 GB | 10–20 GB |
| Anything already AV1 | left alone |

## What it will not do

- **Never grows a file.** Not by a byte. Same-size or larger output is discarded.
- **Never truncates.** Output shorter than the source is rejected.
- **Never burns in subtitles.** Soft tracks only, so you can still turn them off.
- **Never drops an audio track it cannot identify.** If none of the tracks
  are tagged with a language you asked for, every track is kept rather than
  guessing. A mislabeled release costs you some space, not the English audio.
- **Never changes resolution or frame rate.** Size comes from a better codec
  and constant-quality encoding, not from throwing away pixels.
- **Never touches music, books, or photos.** Video files only.

## Quick start (Docker)

```sh
mkdir transcoder && cd transcoder
curl -O https://raw.githubusercontent.com/bughatti/transcoder/main/docker-compose.yml
mkdir config
curl -o config/config.yaml https://raw.githubusercontent.com/bughatti/transcoder/main/config.example.yaml
# edit docker-compose.yml : media mounts, PUID/PGID, and one hardware block
# edit config/config.yaml : your libraries, paths as seen inside the container
docker compose up -d
```

Open <http://localhost:8090>. The first scan starts about twenty seconds
after boot and the dashboard shows every file as it is found, encoded, and
replaced, with speed, ETA, and how much space each one gave back.

Everything runs inside the one container: the binary, ffmpeg, and a SQLite
database under `./config`. Nothing is installed on the host except Docker
(plus the NVIDIA Container Toolkit if you use NVENC).

### Hardware

`encoder.backend: auto` tries each backend with a real test encode and uses
the first one that works, so the default config runs unchanged on any of
these.

| | Compose change | Notes |
|---|---|---|
| **NVIDIA** (NVENC) | uncomment the `deploy.resources` block | needs the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html). AV1 output needs RTX 40-series or newer. |
| **AMD** (VAAPI via Mesa) | uncomment `devices: /dev/dri`, set `PGIDS` to the `render`/`video` group ids | HEVC on GCN 3+ and RDNA, AV1 on RDNA 3+. GPU load comes from the `amdgpu` driver. |
| **Intel** (QSV or VAAPI) | same as AMD | Arc, and 7th-gen Core or newer. No load counters are exposed. |
| **CPU only** | nothing | `backend: software` (libx265 / SVT-AV1). Slower, smaller files. Set `worker.threads` to keep the box responsive. |
| **Apple** (VideoToolbox) | run the binary directly on macOS | not available inside Docker. |

Get the group ids with `getent group render video` on the host.

### Without Docker

```sh
go install github.com/bughatti/transcoder/cmd/transcoder@latest
cp config.example.yaml config.yaml   # edit
transcoder --config config.yaml
```

ffmpeg and ffprobe must be on `PATH` with the encoders you intend to use.

## Configuration

Everything lives in one `config.yaml` ([annotated example](config.example.yaml)).
The part that matters is libraries and profiles:

```yaml
libraries:
  - name: Movies
    path: /media/movies
    priority: 6              # higher runs first
  - name: Shows
    path: /media/shows
  - name: Anime
    path: /media/anime
    profile: anime

profiles:
  default:
    quality: 28              # constant quality: lower = better picture, bigger file
    speed: medium            # slower = smaller file at the same quality
    size_limit_gb: 5         # files already in the target codec are redone above this
    audio_languages: [eng]   # keep these; keep everything if none match
    subtitle_languages: [eng, und]
  anime:
    quality: 24
    max_bitrate: 4M
    tune: animation
    audio_languages: [jpn, eng]
    skip_below_kbps: 2500    # already efficient; do not bother
```

Profiles inherit from `default`, so you only write what differs.

### How small is "as small as possible"?

Constant-quality encoding means you choose the picture and the size
follows. For a two-hour 4K HDR film that starts at 50–70 GB:

| Profile | Result | What you give up |
|---|---|---|
| `quality: 26`, `speed: slow` | 14–20 GB | nothing you can see on a TV |
| **`quality: 28`, `speed: slow`** (default) | 10–15 GB | film grain is slightly softened |
| `quality: 30`, `speed: slow` | 7–11 GB | dark scenes lose some texture |
| `codec: av1`, `quality: 30` | 5–8 GB | same picture as HEVC 30, but only AV1-capable clients (Apple TV 4K 2022+, Shield, most 2023+ TVs) play it directly; older Roku and Fire TV make your server transcode on the fly |

1080p sources scale down about 4x: a 30 GB remux lands at 3–6 GB on the
default. These are worked examples in [`config.example.yaml`](config.example.yaml);
copy the one you want onto a library's profile.

The two knobs worth understanding:

- **`quality`** is the CRF/CQ scale every backend maps onto: 24 is very
  good, 28 is good, 32 is where you start to notice. The same number gives a
  similar picture on NVENC, QSV, VAAPI, and libx265; the file sizes differ.
- **`speed`** trades encode time for size. `slow` on a modern GPU is still
  several times realtime and produces noticeably smaller files than `fast`.

Secrets can come from the environment instead of the file:
`TRANSCODER_DB_DSN`, `TRANSCODER_API_KEY`, `SABNZBD_API_KEY`,
`TRANSCODER_WEBHOOK_SECRET`.

### Securing the dashboard

Set `web.api_key` (or `TRANSCODER_API_KEY`). Reads stay open; every change
(pause, cancel, retry, queue) needs the key as an `X-API-Key` header or
`?api_key=`. The dashboard asks once and keeps it in the browser. Webhooks
use `integrations.webhook_secret`, falling back to the API key.

### Sonarr / Radarr

Settings → Connect → Webhook:

- URL: `http://transcoder:8090/api/webhook/sonarr` (or `/radarr`), add
  `?key=YOUR_KEY` if a key is set
- Triggers: **On Import** and **On Upgrade**

The path in the payload must be inside a configured library as the
transcoder sees it, so mount media at the same paths in both containers.

### Postgres and several workers

```yaml
database:
  driver: postgres
  dsn: postgres://transcoder:pw@db:5432/transcoder?sslmode=disable
```

Run one container with `--mode=web` and any number with `--mode=worker`,
each with a unique `worker.id`. Workers claim jobs with `SKIP LOCKED` and
wake instantly via `LISTEN/NOTIFY`. Migrations run automatically on start;
`--mode=migrate` runs them and exits.

## Dashboard

- **Encoding now** with progress, speed, fps, ETA, and a cancel button.
- **Queue** in the order it will run, with remove.
- **Recent** completed / skipped / failed / cancelled, each with the reason
  and the space saved, and a retry button.
- **Space reclaimed** over the last 30 days, host CPU and memory, GPU load,
  encoder utilisation and VRAM, and per-library disk usage.
- **Log** tail, **Scan now**, **Pause**, and **Add file** for a one-off.

No external assets; it works with no internet access. Dark and light.

## HTTP API

| Method | Path | |
|---|---|---|
| GET | `/api/status` | worker state, encoder, GPU, counts, libraries |
| GET | `/api/jobs?status=active\|queued\|done\|completed,failed&limit=` | |
| GET | `/api/jobs/{id}` | |
| POST | `/api/jobs` `{"path": "...", "priority": 7}` | queue a file |
| POST | `/api/jobs/{id}/cancel` · `/retry` · DELETE `/api/jobs/{id}` | |
| POST | `/api/control/pause` · `/resume` · `/scan` · `/retry-failed?limit=` | |
| GET | `/api/stats` · `/api/metrics?minutes=` · `/api/storage` · `/api/downloads` · `/api/logs?lines=` · `/api/failures` | |
| POST | `/api/webhook`, `/api/webhook/sonarr`, `/api/webhook/radarr` | |
| GET | `/healthz` | |

## How it works

```
scan ──▶ jobs table ──▶ dispatcher ──▶ ffmpeg (hw decode → hw encode)
                            │              │ progress on stdout
   webhook ─────────────────┘              ▼
                                   size & duration checks
                                           │
                              smaller? ── yes ──▶ swap in place
                                 │ no
                                 ▼
                              skipped, source untouched
```

- Hardware decode is tried first; if ffmpeg fails (10-bit source on an
  8-bit decoder, an exotic codec) it retries with software decode and
  hardware encode.
- Output goes to `temp_dir` (a tmpfs in the compose file) and is moved into
  place with a `.backup` of the original held until the move succeeds.
- A stuck job (no progress for `stale_after`) is failed; a worker that
  restarts re-queues its own in-flight jobs first.
- For hardware backends a null-frame encode runs every five minutes. After
  three failures in a row the process exits so the supervisor restarts it.
  This catches a driver update under a running container, which otherwise
  fails every job while the container looks healthy.
- The container starts as root only to chown its data directory, then drops
  to `PUID`/`PGID` inside the Go process. There is no shell entrypoint.

## Development

```sh
go test ./...
go run ./cmd/transcoder --config config.yaml
docker build -t transcoder .
```

Tests use an in-process SQLite database; nothing external is required.

## License

MIT
