# Roadmap

Working end-to-end before polish. One line per item, ticked when done.

## Phase 1 — Firmware + MQTT + storing measurements
- [x] Generic data model: sites/machines/sensors, measurements (+1-min rollups), state_events, jobs
- [x] Go service: `hsd/<site>/<machine>/<sensor>` → normalized measurements in SQLite
- [x] Raw retention + per-minute rollups; versioned migrations
- [x] Machines file (names/kinds) + auto-registration
- [x] API: `/machines`, `/machines/{id}/series`, `/healthz`
- [x] Simulator with a realistic machine profile + ground-truth log
- [x] ESP32 firmware: ADXL345/MPU6050 auto-detect, 200 Hz sampling, 1 s RMS (DC removed), temperature
- [x] Firmware: NVS config over serial, WiFi/MQTT reconnect, NTP timestamps, online/offline status
- [x] Tests (unit, fuzz, integration) + CI + Docker Compose
- [x] Verified on real hardware (sensor mounted on the printer)

## Phase 2 — State machine
- [x] Per-machine signals, rules and debounce times in machines.json
- [x] States: OFF, IDLE, HEATING, RUNNING (+ UNKNOWN when data is missing) → `state_events`
- [x] Bambu bridge (`tools/bambu-bridge`): printer status stored as sensor `bambu` → ground-truth labels for real data
- [ ] Collect labelled data (several days of prints, idle and off): so far three prints with power + labels (2026-11-13, 2026-11-19)
- [x] Replay tool (`server/cmd/replay`) + regression test on the real prints: >99% agreement with Bambu `active`, printing = RUNNING
- [ ] Validate against the simulator's ground truth (the simulator now publishes `power_w`)

## Phase 3 — Shelly: power as the primary signal
- [x] Ingest Shelly Gen2 status (`apower`, `aenergy.total`) mapped to a machine
- [x] Power primary (demo printer: power only; its vibration sensor mostly hears the air filter)
- [ ] Jobs with start/end and kWh; UNPLANNED_STOP heuristic (duration + power profile)

## Phase 4 — API + dashboard
- [x] `/machines/{id}/state`, `/timeline` (+ detected state on the live page)
- [ ] `/metrics`, `/jobs`
- [ ] TypeScript dashboard (Norwegian, mobile-first): current state, daily timeline, key figures, kWh + cost per job

## Phase 5 — Alerts + validation
- [ ] ntfy alerts: UNPLANNED_STOP, temperature over limit
- [ ] Optional: OctoPrint/Moonraker ground truth → hit rate of our state detection

## Later — maybe
- [ ] Buffer readings on the ESP32 while offline
- [ ] MQTT auth / TLS before a customer deployment

## Principles
- Read-only: never control the machine.
- Generic, not printer-specific: one customer = one site with many machines.
- Thresholds and timings in config, not code.
- The MQTT payload + HTTP shapes are the contract; change them deliberately.
