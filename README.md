# Transcoder

A small, single-binary media transcoder for home media servers. It watches
your libraries, re-encodes video to HEVC or AV1 using whatever hardware you
have, and only ever replaces a file with a smaller one.

- **One binary, one container.** Go, no runtime dependencies beyond ffmpeg.
  SQLite by default; Postgres when you want several workers sharing a queue.
- **Any encoder.** NVIDIA NVENC, Intel Quick Sync, VAAPI (Intel/AMD), Apple
  VideoToolbox, or software (libx265 / SVT-AV1). `backend: auto` probes each
  one with a real test encode and picks the first that works.
- **Never makes things worse.** Output larger than the source? The source is
  kept. Output shorter than the source? Kept. Audio in a language you asked
  for can't be identified? Every track is kept. Subtitles are copied as soft
  tracks, never burned in.
- **A dashboard you'll actually look at.** Live progress with speed and ETA,
  cancel, retry, queue a file by hand, per-library storage, a 30-day
  space-reclaimed chart, CPU/memory/GPU load, and the log. No external
  assets, works offline.
- **Fits the *arr stack.** Sonarr/Radarr "On Import" webhooks queue the new
  file immediately; SABnzbd's queue shows on the dashboard.

## Quick start (Docker)

```sh
mkdir transcoder && cd transcoder
curl -O https://raw.githubusercontent.com/bughatti/transcoder/main/docker-compose.yml
mkdir config
curl -o config/config.yaml https://raw.githubusercontent.com/bughatti/transcoder/main/config.example.yaml
# edit docker-compose.yml (media mounts, PUID/PGID, hardware block)
# edit config/config.yaml (libraries — paths as seen inside the container)
docker compose up -d
```

Open <http://localhost:8090>. The first scan starts about twenty seconds
after boot.

### Hardware

| Backend | Compose change | Notes |
|---|---|---|
| NVIDIA NVENC | uncomment the `deploy.resources` block | needs the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html). AV1 needs an RTX 40-series or newer. |
| Intel QSV / VAAPI | uncomment `devices: /dev/dri` and set `PGIDS` to the `render` and `video` group ids | `getent group render video` on the host |
| AMD VAAPI | same as Intel | HEVC on most cards, AV1 on RDNA3+ |
| Apple VideoToolbox | run the binary directly on macOS | not available inside Docker |
| Software | nothing | slow but universal; `speed: fast` recommended |

Everything runs inside the one container: the binary, ffmpeg, and the
SQLite database under `./config`. Hardware access is the only thing that
comes from the host, which is why the compose file has the two optional
blocks. Nothing needs to be installed on the host beyond Docker (plus the
NVIDIA Container Toolkit for NVENC).

**CPU only** — set `encoder.backend: software` in `config.yaml` (or leave
`auto`, which falls back to software when no GPU encoder works). libx265 and
SVT-AV1 are in the image; use `speed: fast` and `worker.threads` to keep the
box responsive. The dashboard's GPU panel then reports no counters.

**AMD** — VAAPI through Mesa (`radeonsi`), already in the image. Pass
`/dev/dri` and the `render` group id; `backend: auto` picks `vaapi`. GPU load
comes from the `amdgpu` driver's sysfs counters. HEVC on GCN 3+/RDNA, AV1 on
RDNA 3+.

**Intel** — QSV (oneVPL) or VAAPI through the `iHD` driver, both in the
image. Same `/dev/dri` mount. No load counters are exposed, so the GPU panel
stays empty.

### Without Docker

```sh
go install github.com/bughatti/transcoder/cmd/transcoder@latest
cp config.example.yaml config.yaml   # edit
transcoder --config config.yaml
```

ffmpeg and ffprobe must be on `PATH` with the encoders you intend to use.

## Configuration

Everything lives in `config.yaml` ([annotated example](config.example.yaml)).
The interesting parts:

```yaml
libraries:
  - name: Movies
    path: /media/movies
    priority: 6
  - name: Anime
    path: /media/anime
    profile: anime

profiles:
  default:
    quality: 28            # CRF/CQ; lower = better quality, bigger file
    speed: medium
    size_limit_gb: 5       # files already in the target codec are only redone above this
    audio_languages: [eng] # keep these tracks (all tracks if none match)
    subtitle_languages: [eng, und]
  anime:
    quality: 24
    max_bitrate: 4M
    tune: animation
    audio_languages: [jpn, eng]
```

Profiles inherit from `default`, so you only write what differs. Secrets can
come from the environment instead of the file: `TRANSCODER_DB_DSN`,
`TRANSCODER_API_KEY`, `SABNZBD_API_KEY`, `TRANSCODER_WEBHOOK_SECRET`.

### What gets queued

A file is queued when its video codec differs from the profile's target, or
when it is already the target codec but larger than `size_limit_gb`. Codecs
in `skip_codecs` (AV1 by default) are left alone: converting an AV1 source
to HEVC would not make it smaller. Files
newer than `min_file_age` (5 minutes) are skipped so importers can finish.
After encoding, the result must be smaller than the source or the source is
kept and the job is marked *skipped* — that is a feature, not a failure.

### Securing the dashboard

Set `web.api_key` (or `TRANSCODER_API_KEY`). Reads stay open; every change
(pause, cancel, retry, queue) needs the key as an `X-API-Key` header or
`?api_key=`. The dashboard asks for it once and keeps it in the browser.
Webhooks use `integrations.webhook_secret`, falling back to the API key.

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

## HTTP API

| Method | Path | |
|---|---|---|
| GET | `/api/status` | worker state, encoder, counts, libraries |
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
  8-bit decoder, exotic codec) it retries with software decode and hardware
  encode.
- Output goes to `temp_dir` (a tmpfs in the compose file) and is moved into
  place with a `.backup` of the original held until the move succeeds.
- A stuck job (no progress for `stale_after`) is failed; a worker that
  restarts re-queues its own in-flight jobs first.
- For hardware backends a null-frame encode runs every five minutes. After
  three failures in a row the process exits so the supervisor restarts it —
  this catches a driver update under a running container, which otherwise
  fails every job with `CUDA_ERROR_NO_DEVICE` while looking healthy.
- The container starts as root only to chown its data directory, then drops
  to `PUID`/`PGID` inside the Go process; there is no shell entrypoint.

## Development

```sh
go test ./...
go run ./cmd/transcoder --config config.yaml
docker build -t transcoder .
```

Tests use an in-process SQLite database; nothing external is required
except ffmpeg for the encoder probe test to be meaningful.

## License

MIT
