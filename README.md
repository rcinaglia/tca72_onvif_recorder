# tca72_onvif_recorder

Vibe-coded Go binary that records RTSP streams from a TP-Link Tapo TC72 camera on ONVIF motion-detection events (detection runs entirely on-camera).

**Built with the help of Claude Code**

## Why?

I needed something lightweight that specifically records on the camera's ONVIF detection, without doing any frame processing on the host machine — the camera already handles that. None of the well-known opensource NVR software options support listening for ONVIF motion events.
If you want something complete check out [**Frigate NVR** ](https://frigate.video/) (note: it does *not* support ONVIF events either).

## Config

On first run, the program writes a default `config.json` next to itself and exits
so you can fill in the camera section:

```json
{
  "camera": {
    "xaddr": "",
    "username": "",
    "password": ""
  },
  "recording": {
    "output_dir": "./recordings",
    "event_idle_timeout_seconds": 10,
    "max_storage_bytes": "10GB"
  },
  "database": {
    "path": "./nvr.db"
  }
}
```

- `event_idle_timeout_seconds` — how many seconds without a new ONVIF event
  before an in-progress recording is stopped.
- `max_storage_bytes` — size cap for `output_dir`; accepts a plain byte count
  or a human string like `"500MB"` / `"10GB"`. Once at or over this limit the
  oldest finished recording is deleted, repeatedly, until back under the cap.

Both values are read fresh each run — edit the file and restart to change
them; a symbol-only change to `config.json` doesn't need a rebuild.

Pass `-config /path/to/config.json` to use a config file somewhere else.

## Recording format

ffmpeg is run with `-c copy`: it repackages the camera's existing
H.264/H.265 stream into a file without decoding or re-encoding it, so CPU
cost is minimal — a copy, not a transcode. The container is Matroska
(`.mkv`) rather than MP4, because MP4 only becomes playable once its index is
written at a clean close; a `.mkv` file stays playable even if ffmpeg is cut
off mid-write, which happens by design every time an event burst ends.

## Recording filenames

Each file is named after the local start time of the recording, e.g.
`2026-08-18_21-30-05.mkv`.

## Storage bookkeeping

Recordings are tracked in the SQLite file at `database.path`: start/end
time, path, size, and status. This is what lets the janitor find "the least
recent recording" to delete, and lets a crash mid-recording be recovered on
the next start (the dangling row is closed out against whatever ended up on
disk, or dropped if the file is gone).

## Requirements

- `ffmpeg` on `PATH`.
- Network access to the camera's ONVIF and RTSP ports.

## Build / test

```
go build -o nvr .
go test ./...
```

