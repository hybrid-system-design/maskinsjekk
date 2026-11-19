# Maskinsjekk — design

What the system is meant to do and the decisions behind it. What is built so
far is tracked in [ROADMAP.md](../ROADMAP.md); the exact MQTT and HTTP
contracts are in the [README](../README.md).

## Goal
Monitor a machine without access to its controls. Clamp-on sensors measure
power and vibration; the system infers the machine's state from them and shows
uptime, stops and energy per job, and sends alerts to a phone.

A 3D printer stands in for a production machine in the demo. Nothing in the
system may be printer-specific: the same code must monitor a lathe, a press or
a packaging line by changing configuration only.

## Hardware
- ESP32 with an accelerometer (ADXL345 or MPU6050 on I2C, or an analog
  ADXL335) and an optional temperature sensor
- A Shelly Gen2+ power meter (e.g. Plus Plug S) with local MQTT, providing
  power (W) and energy (Wh)
- Optionally, the machine's own status as ground truth for validating state
  detection (for the demo printer: the Bambu bridge; OctoPrint/Moonraker would
  serve the same purpose). It is never used for monitoring itself.

## Architecture
ESP32 and Shelly → Mosquitto (MQTT) → Go service → SQLite → REST API → web
dashboard. Everything runs locally in Docker Compose; no cloud.

## Firmware (ESP32)
- Arduino core, chosen over ESP-IDF to keep the toolchain and CI simple
- Sample the accelerometer at about 200–400 Hz and compute RMS per second,
  with gravity/DC removed first
- Publish every second on `hsd/<site>/<machine>/<sensor>` as JSON:
  `{"ts": <unix_ms>, "vib_rms": <g>, "temp_c": <float>}`
- Wi-Fi and MQTT with automatic reconnect; configuration in NVS, never
  hardcoded credentials

## Go service
- Subscribe to the sensor topics and the Shelly status (`apower`,
  `aenergy.total`)
- Normalize everything into measurements: (machine, sensor, metric, value, ts)
- State machine per machine: OFF, HEATING, RUNNING, IDLE, UNPLANNED_STOP
  - Power is the primary signal; vibration confirms
  - Thresholds and debounce times in per-machine configuration, not in code
  - UNPLANNED_STOP = RUNNING → IDLE/OFF without the job completing (starting
    with a simple heuristic on duration and power profile)
- Store state changes as events, not only raw data
- Alerts on UNPLANNED_STOP and on temperature over a limit via ntfy
  (self-hosted or ntfy.sh)
- Optional validation: compare detected states with the ground-truth sensor
  and report the hit rate

## Data model (SQLite)
- sites, machines, sensors
- measurements (raw values kept for a limited time, plus per-minute rollups)
- state_events (machine, state, start_ts, end_ts)
- jobs (machine, start_ts, end_ts, energy_wh)

One customer = one site with several machines.

## API
- `GET /machines`, `GET /machines/{id}/state`
- `GET /machines/{id}/metrics?from&to` (uptime %, number of stops, stop time,
  kWh)
- `GET /machines/{id}/timeline?from&to` (states for a timeline)
- `GET /machines/{id}/jobs`

## Dashboard
- Current state per machine, clear and large
- Daily timeline with colour-coded states
- Key figures: uptime %, number of stops, total stop time, kWh and cost per job
- Works well on a phone or tablet next to the machine
- UI text in Norwegian

## Out of scope
- Controlling the machine: the system is read-only
- User management and multi-tenancy
- Cloud hosting: everything runs locally
