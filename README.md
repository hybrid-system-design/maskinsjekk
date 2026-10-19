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
controls the machine. Full brief: [docs/SPEC.md](docs/SPEC.md). Progress:
[ROADMAP.md](ROADMAP.md).

## Repository layout

| Path | What it is |
|------|------------|
| `server/` | Go service: MQTT → normalized measurements in SQLite → HTTP API |
| `firmware/sensor/` | ESP32 sensor node (Arduino): accelerometer RMS + temperature over MQTT |
| `tools/simulator/` | Publishes a realistic machine profile (with ground-truth log), no hardware needed |
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

**Timestamp rule:** a device `ts` is honored only within
`[now − SANITY_PAST, now + SANITY_FUTURE]` (default 50h / 5min); otherwise the
server's arrival time is used. That accepts a simulator backfill and rejects
1970 timestamps from a device that hasn't synced yet.

### Data model (SQLite)

`sites` → `machines` → `sensors`; `measurements` (raw, kept `RAW_RETENTION`,
default 7 days); `measurements_1m` (per-minute avg/min/max/n rollups, kept
forever, maintained on insert); `state_events` and `jobs` (populated from Phase
2/3). Schema changes are append-only migrations tracked by `PRAGMA user_version`.

Unknown sites/machines/sensors are auto-registered on first message
(`AUTO_REGISTER=true`); names and sensor kinds come from
`deploy/config/machines.json`, which wins over auto-registered placeholders.
Per-machine thresholds will live in that file too, never in code.

### HTTP API

| Endpoint | Returns |
|----------|---------|
| `GET /machines` | All machines with site, name, `last_seen`, `stale` (no data within `STALE_THRESHOLD`) and sensors |
| `GET /machines/{id}/series?metric=&from=&to=&bucket=` | Downsampled series: `points[] {t, ts, avg, min, max, n}`. `from`/`to` are RFC3339 or unix ms (default last 24h); `bucket` in seconds (default 60). Buckets ≥ 1 min come from rollups; finer ones from raw data. Max 5000 points. |
| `GET /healthz` | `{"ok":true}` |

Coming with later phases: `/machines/{id}/state`, `/metrics`, `/timeline`, `/jobs`.

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

## Sensor firmware (`firmware/sensor/`)

ESP32 (Arduino core) with an I2C accelerometer — **ADXL345 or MPU6050,
auto-detected** on the default pins (SDA 21, SCL 22) — and an optional
temperature source (MAX6675 thermocouple on SCK 18 / CS 19 / SO 5, or the
MPU6050 die temperature).

- A FreeRTOS task samples at `rate` Hz (default 200). Each 1 s window subtracts
  the per-axis mean (gravity/DC) and reports `vib_rms = sqrt(var_x+var_y+var_z)`
  in g.
- WiFi and MQTT reconnect on their own without blocking sampling. Time comes
  from NTP (UTC).
- **No credentials in code**: config lives in NVS and is set over the serial
  console (115200 baud):

  ```
  set ssid MyWifi
  set pass secret
  set mqtt_host 192.168.1.50
  set site demo
  set machine printer-1
  set sensor esp32-1
  set temp max6675        # none | max6675 | mpu6050
  reboot
  ```

  `show`, `status` and `help` are also available.

```bash
arduino-cli compile --fqbn esp32:esp32:esp32doit-devkit-v1 firmware/sensor
arduino-cli upload  --fqbn esp32:esp32:esp32doit-devkit-v1 --port COM4 firmware/sensor
```

Requires the `PubSubClient` library. CI compiles the sketch on every push.

## Simulator (`tools/simulator/`)

Models OFF → HEATING → RUNNING → IDLE → OFF with randomized durations and a
configurable share of jobs that stop unplanned (`--abort-rate`, default 0.25).
Every true state transition is logged (`truth ...`) as ground truth for testing
state detection. Same `--seed` = same schedule. Publishes as machine
`sim-printer` by default so it never mixes with real data.

Flags: `--hours` (backfill, default 6), `--step` (backfill interval, default 5s),
`--live`, `--site`, `--machine`, `--sensor`, `--seed`, `--abort-rate`.

## Testing

```bash
cd server
go test ./...                                                   # unit (fast, no Docker)
go test -run='^$' -fuzz=FuzzParseSensorMessage -fuzztime=30s ./internal/ingest
go test -tags integration -count=1 ./internal/integration/...   # needs Docker running
```

The integration suite runs a real Mosquitto via testcontainers and covers the
round trip, honoring device timestamps on backfill, bad-payload resilience,
`AUTO_REGISTER=false`, and reconnecting after a network drop. CI runs everything
above plus a Docker smoke test and the firmware compile.
