# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Working preferences

- **Do not create or suggest pull requests on your own.** After finishing a piece of work, present it and let the user check/verify it first. Only open a PR (or even propose opening one) when the user explicitly asks. The same goes for pushing and merging — wait for the user's go-ahead.

## What this is

Maskinsjekk machine-monitoring demo (HSD). Read-only machine monitoring: ESP32 vibration/temperature + Shelly power → Mosquitto → Go service → SQLite → REST API → dashboard. Foundation for a product offer to manufacturing companies, so **everything must stay generic** (no printer-specific logic; thresholds in `deploy/config/machines.json`, not code). The brief is in `docs/SPEC.md`; progress in `ROADMAP.md`; contracts in `README.md`.

Structure and conventions were carried over from the user's temperature-monitor project (`../connected_systems`): same Go layout (`internal/config|store|ingest|api|integration`), pure-Go SQLite, log-and-drop ingestion with pure, fuzzed parsers, testcontainers integration tests.

## Commands

```
cd server && go test ./...                                          # unit
cd server && go test -run TestName ./internal/store                 # single test
cd server && go test -run='^$' -fuzz=FuzzParseSensorMessage -fuzztime=30s ./internal/ingest
cd server && go test -run='^$' -fuzz=FuzzParseShellyStatus -fuzztime=30s ./internal/ingest
cd server && go test -tags integration -count=1 ./internal/integration/...   # needs Docker
cd deploy && docker compose up -d --build                           # MQTT_PORT/HTTP_PORT env override host ports
cd tools/simulator && go run . --hours 6 --live
arduino-cli compile --fqbn esp32:esp32:esp32doit-devkit-v1 firmware/sensor
```

CI (`.github/workflows/ci.yml`) also enforces `gofmt -l server tools` (must be empty), `go vet`, `staticcheck`, and `-race` unit tests; run these before handing work over.

## Architecture

`server` and `tools/simulator` are **separate Go modules** (`maskinsjekk`, `maskinsjekk-simulator`); there is no go.work, so run go commands from inside each directory.

Data flow in `server/cmd/maskinsjekk/main.go`: `config.Load` (env vars) + `config.LoadMachines` (`machines.json`) → `store.Open` (migrates) + `SyncRegistry` → HTTP API (`internal/api`) and MQTT `ingest.Ingestor` run side by side, plus a `pruneLoop` for raw retention.

- `ingest`: topic `hsd/<site>/<machine>/<sensor>`; JSON payload where **every numeric field becomes a metric** (new sensor types need no server change). Parsing (`parseSensorMessage`, `resolveEventTime`) is pure and fuzzed; bad messages are logged and dropped. Device `ts` is only trusted inside the sanity window, else arrival time is used. Shelly Gen2+ meters are the one native-format exception: sensors of kind `shelly-gen2` in `machines.json` map an `mqtt_prefix`/`channel` to a machine, and `parseShellyStatus` (`shelly.go`, also fuzzed) reads `<prefix>/status/switch:N|pm1:N`.
- `store`: `InsertMeasurements` writes raw rows and maintains `measurements_1m` rollups in the same transaction; `Series` serves buckets ≥ 1 min from rollups, finer from raw. Machines file entries win over auto-registered placeholders.
- The MQTT payload and HTTP shapes are the contract (see README.md); change them deliberately.
- Planned (see ROADMAP.md): state machine from vibration → Shelly power ingest → jobs/kWh → dashboard → ntfy alerts. `state_events` and `jobs` tables already exist but are unpopulated.

## Conventions

- All timestamps are unix **milliseconds** UTC in storage and MQTT.
- Ids (site/machine/sensor/metric) are `[a-z0-9_-]{1,64}`, enforced on both server and firmware.
- Schema changes: append a new entry to `migrations` in `store.go`; never edit an applied one.
- Firmware is Arduino (not ESP-IDF). Config lives in NVS via the serial console; never hardcode credentials. Shared structs go in `types.h` (the Arduino prototype generator breaks structs defined in the .ino).
- UI text (dashboard) is Norwegian; code, comments and docs are English.
