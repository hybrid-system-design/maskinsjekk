# Machine Data Demo (HSD) — brief

## Goal
Demonstrate machine monitoring without access to the machine's controls.
A 3D printer acts as a stand-in for a production machine.
The system infers machine state from power and vibration and shows uptime,
stops and energy per job. It also sends alerts to a phone.
This becomes the foundation for "Maskinsjekk", an entry offer for
manufacturing companies, so everything must be generic, not printer-specific.

## Hardware
- ESP32 with accelerometer (ADXL345 or MPU6050, I2C) and temperature sensor
- Shelly Plus Plug S (Gen2+) with local MQTT, provides power (W) and energy (Wh)
- The printer may run OctoPrint/Klipper, but it is used ONLY as ground truth for validation

## Architecture
ESP32 and Shelly -> Mosquitto (MQTT) -> Go service -> SQLite -> REST API -> web dashboard
Everything runs in Docker Compose. Reuse structure and code from the existing
temperature monitoring project (Go, MQTT, SQLite, Docker Compose) where it fits.

## Firmware (ESP32)
- Arduino core (decision 2026-10-19: keep Arduino rather than ESP-IDF, to reuse the existing toolchain/CI)
- Sample the accelerometer at about 200 to 400 Hz, compute RMS per second (remove gravity/DC first)
- Publish every second on `hsd/<site>/<machine>/<sensor>` as JSON:
  `{"ts": <unix_ms>, "vib_rms": <g>, "temp_c": <float>}`
- Wi-Fi and MQTT with automatic reconnect, config in NVS, no hardcoded credentials

## Go service
- Subscribe to the ESP32 topics and the Shelly status (`apower`, `aenergy.total`)
- Normalize everything into measurements: (machine_id, sensor_id, metric, value, ts)
- State machine per machine: OFF, HEATING, RUNNING, IDLE, UNPLANNED_STOP
  - Power is the primary signal, vibration confirms
  - Thresholds and debounce times in per-machine config, not in code
  - UNPLANNED_STOP = RUNNING -> IDLE/OFF without the job being marked complete
    (start with a simple heuristic, e.g. duration and power profile)
- Store state changes as events, not just raw data
- Alerts on UNPLANNED_STOP and temperature over limit via ntfy (self-hosted or ntfy.sh)
- Optional validation module: read printer status from the OctoPrint/Moonraker API
  and log the hit rate against our own state detection

## Data model (SQLite)
- sites, machines, sensors
- measurements (downsampled, keep raw values for a limited time)
- state_events (machine_id, state, start_ts, end_ts)
- jobs (machine_id, start_ts, end_ts, energy_wh)
Keep the model generic: one customer = one site with multiple machines.

## API
- GET /machines, GET /machines/{id}/state
- GET /machines/{id}/metrics?from&to  (uptime %, number of stops, stop time, kWh)
- GET /machines/{id}/timeline?from&to (states for a timeline)
- GET /machines/{id}/jobs

## Dashboard (TypeScript)
- Current state per machine, clear and large
- Daily timeline with color-coded states
- Key figures: uptime %, number of stops, total stop time, kWh and cost per job
- Must work well on mobile, since it is shown live in customer meetings
- UI text in Norwegian

## Phases
1. Firmware + MQTT + storing measurements (vibration and temperature)
2. State machine and events based on vibration
3. Shelly integration, power as primary signal, jobs and kWh
4. API and dashboard
5. Alerts and validation module
Timeframe: about 2 weeks. Prioritize a working end-to-end system before polish.

## Out of scope
- No control of the machine, the system is read-only
- No user management or multi-tenancy yet
- No cloud hosting, everything runs locally
