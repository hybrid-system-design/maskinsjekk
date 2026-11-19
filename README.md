# Maskinsjekk — machine monitoring demo (HSD)

Machine monitoring **without access to the machine's controls**. Clamp-on
sensors (vibration + temperature on an ESP32, power from a Shelly smart plug)
feed a local service that infers machine state and reports uptime, stops and
energy per job. A 3D printer stands in for a production machine; nothing in the
system is printer-specific.

```
ESP32 (vibration, temp) ─┐
                         ├─MQTT─> Mosquitto ─> Go service ─> SQLite ─> REST API ─> dashboard
Shelly Plus Plug S ──────┘                          └─> ntfy alerts
```

Everything runs locally in Docker Compose. Read-only by design: the system never
controls the machine. Design: [docs/SPEC.md](docs/SPEC.md). Progress:
[ROADMAP.md](ROADMAP.md).

## Status

A working demo, not a finished product. Sensor ingest (ESP32 vibration and
temperature, Shelly power), storage with rollups, the HTTP API and a live page
with charts run end to end on real hardware, and the service infers each
machine's state (OFF / IDLE / HEATING / RUNNING) from configurable rules.
Jobs/kWh, unplanned-stop detection, the full dashboard and alerts are in
progress (see [ROADMAP.md](ROADMAP.md)).

The MQTT broker accepts anonymous clients: run the stack on a trusted LAN only.
Authentication and TLS come before any deployment on a production network.

## Repository layout

| Path | What it is |
|------|------------|
| `server/` | Go service: MQTT → normalized measurements in SQLite → HTTP API |
| `firmware/sensor/` | ESP32 sensor node (Arduino): accelerometer RMS + temperature over MQTT |
| `tools/simulator/` | Publishes a realistic machine profile (with ground-truth log), no hardware needed |
| `tools/bambu-bridge/` | Optional: a Bambu Lab printer's own status as a ground-truth sensor |
| `deploy/` | `docker-compose.yml`, Mosquitto config, `config/machines.json` |

## Quick start

```bash
cd deploy
docker compose up -d --build                      # broker :1883, API :8080
cd ../tools/simulator
go run . --hours 6 --live                         # 6h backfill, then 1 Hz live
curl localhost:8080/machines
curl "localhost:8080/machines/sim-printer/series?metric=vib_rms&bucket=300"
```

Host ports are overridable to run next to another stack:
`MQTT_PORT=1884 HTTP_PORT=8081 docker compose up -d`.

## Data contract

### MQTT (sensor → server)

- **Topic:** `hsd/<site>/<machine>/<sensor>` — ids are 1–64 chars of `[a-z0-9_-]`.
- **Payload** (JSON, 1 Hz):

  ```json
  { "ts": 1792411200000, "vib_rms": 0.0123, "temp_c": 24.5 }
  ```

  `ts` is unix **milliseconds** (optional — omitted until the device has NTP
  time). **Every other numeric field is stored as a metric**, so new sensor types
  need no server change. Non-numeric fields are ignored; at most 16 metrics per
  message. Malformed messages are logged and dropped, never fatal.
- **Status:** `hsd/<site>/<machine>/<sensor>/status` — retained `online`, or
  `offline` via MQTT last will.

### MQTT (Shelly Gen2+ power meters)

Shelly Plus/Pro/Gen3/Gen4 devices are read in their **native** format, so they
need no custom firmware or bridge. Map each one in `machines.json` with kind
`shelly-gen2`, the topic prefix set in the device's web UI, and (on
multi-channel devices) which channel:

```json
{ "id": "shelly-1", "kind": "shelly-gen2", "mqtt_prefix": "shelly-printer-1", "channel": 0 }
```

The server subscribes to `<prefix>/status/switch:<channel>` and
`<prefix>/status/pm1:<channel>` and stores, under that machine/sensor, whichever
of these the device reports:

| metric | source field | meaning |
|---|---|---|
| `power_w` | `apower` | active power, W |
| `energy_wh` | `aenergy.total` | lifetime energy counter, Wh |
| `voltage_v` / `current_a` / `freq_hz` / `pf` | `voltage` / `current` / `freq` / `pf` | mains readings |
| `device_temp_c` | `temperature.tC` | the meter's own temperature (not `temp_c`: series are keyed by machine + metric, so it would mix with a machine sensor's `temp_c`) |
| `output` | `output` | relay state 1/0 (switch devices only) |

Shelly status has no reading timestamp, so arrival time is used. Device setup:
Settings → MQTT → enable, server `<broker-host>:1883`, no TLS, a readable client
id / prefix, and **"Generic status update over MQTT"** on. Set the relay's
power-on default to ON (or restore last) so the machine gets power back after an
outage; the system never switches it.

**Timestamp rule:** a device `ts` is honored only within
`[now − SANITY_PAST, now + SANITY_FUTURE]` (default 50h / 5min); otherwise the
server's arrival time is used. That accepts a simulator backfill and rejects
1970 timestamps from a device that hasn't synced yet.

### Data model (SQLite)

`sites` → `machines` → `sensors`; `measurements` (raw, kept `RAW_RETENTION`,
default 7 days); `measurements_1m` (per-minute avg/min/max/n rollups, kept
forever, maintained on insert); `state_events` (one row per detected state
interval, `end_ts` NULL for the current one) and `jobs` (populated from Phase 3). Schema changes are append-only migrations tracked by `PRAGMA user_version`.

Unknown sites/machines/sensors are auto-registered on first message
(`AUTO_REGISTER=true`); names and sensor kinds come from
`deploy/config/machines.json`, which wins over auto-registered placeholders.
Per-machine state rules live in that file too, never in code (see
[State detection](#state-detection)).

### HTTP API

| Endpoint | Returns |
|----------|---------|
| `GET /machines` | All machines with site, name, `last_seen`, `stale` (no data within `STALE_THRESHOLD`) and sensors |
| `GET /machines/{id}/series?metric=&from=&to=&bucket=` | Downsampled series: `points[] {t, ts, avg, min, max, n}`. `from`/`to` are RFC3339 or unix ms (default last 24h); `bucket` in seconds (default 60). Buckets ≥ 1 min come from rollups; finer ones from raw data. Max 5000 points. |
| `GET /machines/{id}/state` | Current detected state: `{machine_id, state, since, since_ts, reason}`; `state` is null for a machine without state rules |
| `GET /machines/{id}/timeline?from=&to=` | Detected state intervals overlapping the range (default last 24h), clipped to it: `segments[] {state, start, start_ts, end, end_ts}`; `end`/`end_ts` are null for the ongoing state |
| `GET /healthz` | `{"ok":true}` |

Coming with later phases: `/metrics`, `/jobs`.

## State detection

Each machine with a `state` block in `machines.json` is evaluated every
`STATE_INTERVAL` (default 5s). Example (the demo printer):

```json
"state": {
  "signals": {
    "power": { "sensor": "shelly-1", "metric": "power_w", "window": "60s", "max_age": "10m" },
    "vib":   { "sensor": "esp32-1", "metric": "vib_rms", "window": "60s", "max_age": "2m", "optional": true }
  },
  "rules": [
    { "state": "OFF",     "when": { "power": { "below": 3 } },  "enter_after": "60s" },
    { "state": "IDLE",    "when": { "power": { "below": 20 } }, "enter_after": "60s" },
    { "state": "HEATING", "when": { "vib": { "below": 0.02 } }, "enter_after": "60s" },
    { "state": "RUNNING", "enter_after": "60s" }
  ]
}
```

- A **signal** is a metric (optionally from one `sensor`), averaged over
  `window`. With no sample in the window the latest one is used if it is at
  most `max_age` old (default `window`); power meters only report on change, so
  give them a generous `max_age`. Older than that, the signal is missing.
- **Rules** are tried in order; the first whose conditions all hold
  (`at_least` ≤ value < `below`) gives the candidate state. A rule without
  `when` always holds, so put the default last. States: `OFF`, `IDLE`,
  `HEATING`, `RUNNING`.
- A missing **required** signal gives `UNKNOWN`. A missing `optional` one only
  makes the rules that test it not match (above: no vibration data → a
  powered machine counts as RUNNING, not HEATING).
- **Debounce:** the candidate must hold for `enter_after` before the state
  changes; the new state is then backdated to when the candidate first
  appeared, so the timeline lines up with the data. `UNKNOWN` is entered at
  once (`max_age` already delayed it).

Power is the primary signal: it tells off, standby and working apart cleanly.
Vibration only separates heating up from motion. On the demo printer vibration
stays high for ~15 min after a print (fans) while power is back at standby, so
a vibration-only rule would stretch every job.

Tune rules offline with `cmd/replay`: it fetches history from a running
service, runs a local machines file over it with the same code, and prints the
timeline, time per state and (with `--truth`) agreement with a 0/1
ground-truth metric such as the Bambu bridge's `active`:

```bash
cd server
go run ./cmd/replay --url http://<host>:8080 --machine printer-1 \
  --machines ../deploy/config/machines.json --from 2026-11-12T13:00:00Z --truth active
```

Use `--bucket 60` for history older than `RAW_RETENTION`. On five days of the
demo printer (two prints) the shipped rules agree with `active` in 99.99% of
10 s buckets; the two prints are also a regression test
(`server/internal/state/testdata`).

## Configuration (server env vars)

| Var | Default | Meaning |
|-----|---------|---------|
| `HTTP_ADDR` | `:8080` | HTTP listen address |
| `MQTT_URL` | `tcp://localhost:1883` | broker URL |
| `MQTT_TOPIC` | `hsd/+/+/+` | sensor subscription filter |
| `MQTT_CLIENT_ID` | `maskinsjekk-server` | MQTT client id |
| `DB_PATH` | `maskinsjekk.db` | SQLite file |
| `MACHINES_PATH` | `machines.json` | machines file (missing = auto-register only) |
| `AUTO_REGISTER` | `true` | accept messages from machines not in the machines file |
| `RAW_RETENTION` | `168h` | how long raw 1 Hz data is kept |
| `STALE_THRESHOLD` | `30s` | silence after which a machine is reported stale |
| `SANITY_PAST` / `SANITY_FUTURE` | `50h` / `5m` | device timestamp acceptance window |
| `STATE_INTERVAL` | `5s` | how often machine states are evaluated |

## Sensor firmware (`firmware/sensor/`)

ESP32 (Arduino core) with an accelerometer — an I2C **ADXL345 or MPU6050,
auto-detected** on the default pins (SDA 21, SCL 22), or an analog **ADXL335
(GY-61 breakout)** enabled with `set accel adxl335` — and an optional
temperature source (MAX6675 thermocouple on SCK 18 / CS 19 / SO 5, or the
MPU6050 die temperature). An optional 128x64 SSD1306 OLED (I2C, auto-detected)
shows connection state, temperature, vibration and a live x/y/z debug readout.

GY-61 wiring (ADC1 pins only; ADC2 is unusable while WiFi is on): VCC → 3V3,
GND → GND, X → GPIO36, Y → GPIO39, Z → GPIO34.

- A FreeRTOS task samples at `rate` Hz (default 200). Each 1 s window subtracts
  the per-axis mean (gravity/DC) and reports `vib_rms = sqrt(var_x+var_y+var_z)`
  in g.
- WiFi and MQTT reconnect on their own without blocking sampling. Time comes
  from NTP (UTC).
- `mqtt_host` is a plain IP or an mDNS name ending in `.local` (e.g.
  `fedora.local`, served by avahi on the broker host). The name is re-resolved
  before every MQTT connect attempt, so a broker that changes IP is picked up
  without touching the node; if a lookup fails the last known IP is tried.
- Network (OTA) uploads via ArduinoOTA once `ota_pass` is set (disabled until
  then). The node advertises itself as `hsd-<machine>-<sensor>.local`, port
  3232, and shows `OTA nn%` on the OLED during an update.
- **No credentials in code**: config lives in NVS and is set over the serial
  console (115200 baud):

  ```
  set ssid <wifi-ssid>
  set pass <wifi-password>
  set mqtt_host fedora.local   # or a plain IP, e.g. 192.168.1.50
  set site demo
  set machine printer-1
  set sensor esp32-1
  set temp max6675        # none | max6675 | mpu6050
  set accel adxl335       # auto (I2C, default) | adxl335 (analog GY-61)
  set ota_pass <ota-password>  # enables network uploads (masked in `show`)
  reboot
  ```

  `show`, `status` and `help` are also available.

```bash
arduino-cli compile --fqbn esp32:esp32:esp32doit-devkit-v1 firmware/sensor
arduino-cli upload  --fqbn esp32:esp32:esp32doit-devkit-v1 --port COM4 firmware/sensor
```

Over the network, once `ota_pass` is set. arduino-cli only uploads to a
network port its mDNS discovery has seen, so first check that
`arduino-cli board list` shows the node (`hsd-<machine>-<sensor>`, protocol
`network`) and use the IP it lists:

```bash
arduino-cli upload --fqbn esp32:esp32:esp32doit-devkit-v1 --port <node-ip> --protocol network --upload-field password=<ota-password> --discovery-timeout 10s firmware/sensor
```

If discovery doesn't see the node (`port not found: <ip> network`), push the
compiled image with the core's `espota` tool directly (`espota.exe` on
Windows, `espota.py` elsewhere, in the esp32 core's `tools/` directory;
compile with `--output-dir <dir>` first, outside the repo):

```bash
espota.exe -r -i <node-ip> -p 3232 --auth=<ota-password> -f <dir>/sensor.ino.bin
```

The sketch must keep fitting the default partition scheme's OTA app slot
(1.25 MB).

Requires the `PubSubClient`, `Adafruit SSD1306` and `Adafruit GFX Library` libraries. CI compiles the sketch on every push.

## Simulator (`tools/simulator/`)

Models OFF → HEATING → RUNNING → IDLE → OFF with randomized durations and a
configurable share of jobs that stop unplanned (`--abort-rate`, default 0.25).
Every true state transition is logged (`truth ...`) as ground truth for testing
state detection. Same `--seed` = same schedule. Publishes as machine
`sim-printer` by default so it never mixes with real data: `vib_rms` and
`temp_c` on sensor `esp32-1`, and `power_w` on sensor `power` (like a smart
plug), which the `sim-printer` state rules use.

Flags: `--hours` (backfill, default 6), `--step` (backfill interval, default 5s),
`--live`, `--site`, `--machine`, `--sensor`, `--power-sensor` (empty = no
power), `--seed`, `--abort-rate`.

## Bambu bridge (`tools/bambu-bridge/`)

Ground-truth labels for real data: reads a Bambu Lab printer's status from its
local MQTT server (TLS 8883, user `bblp`, LAN access code) and republishes it
as an ordinary sensor, by default `hsd/demo/printer-1/bambu`. The server needs
no changes: every numeric field becomes a metric next to `vib_rms`/`temp_c`.
All printer-specific code stays in this tool.

| metric | meaning |
|---|---|
| `printer_reachable` | 1 while the printer reports; 0 otherwise (then nothing else is sent) |
| `state_code` | `gcode_state`: 0 IDLE, 1 PREPARE, 2 RUNNING, 3 PAUSE, 4 FINISH, 5 FAILED, 6 SLICING, -1 other |
| `active` | 1 in PREPARE / RUNNING / PAUSE / SLICING |
| `stage` | Bambu's `stg_cur` print sub-stage (e.g. heating, bed leveling), raw |
| `percent`, `remaining_min`, `layer`, `total_layers` | job progress |
| `nozzle_c`, `nozzle_target_c`, `bed_c`, `bed_target_c`, `chamber_c` | temperatures |
| `door_open` | 0/1 on enclosed printers that report it |
| `error_code` | `print_error` (non-zero after a failure) |

Published every `INTERVAL` (5 s) and immediately on a state, stage or door
change. Config is by environment: `BAMBU_SERIAL`, `BAMBU_ACCESS_CODE`
(required), `BAMBU_HOST` (empty = find the printer by its UDP 2021
announcements, so DHCP changes don't matter), `MQTT_URL`, `TOPIC`, `INTERVAL`.

In the compose stack it is opt-in: copy `deploy/.env.example` to `deploy/.env`
on the server, set `COMPOSE_PROFILES=bambu` and the serial/access code, and
redeploy. It uses host networking to hear the announcements, which arrive unsolicited on
UDP 2021. Fedora Workstation's default zone already allows 1025-65535; on a
stricter firewall open it (`sudo firewall-cmd --permanent --add-port=2021/udp`)
or set `BAMBU_HOST` instead.
Some Bambu models accept only a few local MQTT clients at once (the Bambu app
and e.g. Home Assistant count too).

## Testing

```bash
cd server
go test ./...                                                   # unit (fast, no Docker)
go test -run='^$' -fuzz=FuzzParseSensorMessage -fuzztime=30s ./internal/ingest
go test -run='^$' -fuzz=FuzzParseShellyStatus -fuzztime=30s ./internal/ingest
go test -tags integration -count=1 ./internal/integration/...   # needs Docker running
```

The integration suite runs a real Mosquitto via testcontainers and covers the
round trip, native Shelly status mapping, honoring device timestamps on backfill, bad-payload resilience,
`AUTO_REGISTER=false`, and reconnecting after a network drop. CI runs everything
above plus the Bambu bridge tests (`cd tools/bambu-bridge && go test ./...`),
a Docker smoke test and the firmware compile.

## Licence

Copyright © 2026 Hybrid Systemdesign AS. All rights reserved. The source is
published so it can be read; using, copying or distributing it needs written
permission. See [LICENSE](LICENSE).
