// Package config loads runtime configuration from environment variables (with
// sensible defaults) plus an optional machines file describing sites, machines
// and sensors. Everything machine-specific lives in that file, never in code.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime settings for the service.
type Config struct {
	// HTTPAddr is the listen address for the HTTP API, e.g. ":8080".
	HTTPAddr string
	// MQTTURL is the broker URL, e.g. "tcp://localhost:1883".
	MQTTURL string
	// MQTTTopic is the sensor subscription filter: hsd/<site>/<machine>/<sensor>.
	MQTTTopic string
	// MQTTClientID is this subscriber's client id.
	MQTTClientID string
	// DBPath is the SQLite database file path.
	DBPath string
	// MachinesPath is the JSON machines file. A missing file is not an error.
	MachinesPath string
	// AutoRegister creates unknown sites/machines/sensors on first message.
	// When false, messages for machines not in the machines file are dropped.
	AutoRegister bool
	// RawRetention is how long raw (1 Hz) measurements are kept; the per-minute
	// rollups are kept indefinitely.
	RawRetention time.Duration
	// StaleThreshold: a machine whose last measurement is older than this is
	// reported as stale (sensor offline).
	StaleThreshold time.Duration
	// SanityPast / SanityFuture bound how far a device-reported timestamp may
	// deviate from server wall-clock before we fall back to arrival time.
	SanityPast   time.Duration
	SanityFuture time.Duration
	// MQTT reconnect tuning: interval between initial connect attempts, and the
	// cap on the auto-reconnect backoff after a dropped connection.
	MQTTConnectRetryInterval time.Duration
	MQTTMaxReconnectInterval time.Duration
}

// Load reads configuration from the environment, applying defaults.
func Load() Config {
	return Config{
		HTTPAddr:       getenv("HTTP_ADDR", ":8080"),
		MQTTURL:        getenv("MQTT_URL", "tcp://localhost:1883"),
		MQTTTopic:      getenv("MQTT_TOPIC", "hsd/+/+/+"),
		MQTTClientID:   getenv("MQTT_CLIENT_ID", "maskinsjekk-server"),
		DBPath:         getenv("DB_PATH", "maskinsjekk.db"),
		MachinesPath:   getenv("MACHINES_PATH", "machines.json"),
		AutoRegister:   getbool("AUTO_REGISTER", true),
		RawRetention:   getdur("RAW_RETENTION", 7*24*time.Hour),
		StaleThreshold: getdur("STALE_THRESHOLD", 30*time.Second),
		SanityPast:     getdur("SANITY_PAST", 50*time.Hour),
		SanityFuture:   getdur("SANITY_FUTURE", 5*time.Minute),

		MQTTConnectRetryInterval: getdur("MQTT_CONNECT_RETRY_INTERVAL", 5*time.Second),
		MQTTMaxReconnectInterval: getdur("MQTT_MAX_RECONNECT_INTERVAL", 30*time.Second),
	}
}

// Machines is the machines file: one customer = one site with many machines.
type Machines struct {
	Sites []Site `json:"sites"`
}

// Site is one customer location.
type Site struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Machines []Machine `json:"machines"`
}

// Machine is one monitored machine. Per-machine thresholds for the state
// machine will live here (Phase 2+).
type Machine struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Sensors []Sensor `json:"sensors"`
}

// Sensor is one data source attached to a machine (ESP32 node, power plug, ...).
type Sensor struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // free text, e.g. "esp32-vibration", "shelly-plug"
}

// LoadMachines reads the machines file. A missing file yields an empty set (the
// service then relies on auto-registration); a malformed one is an error.
func LoadMachines(path string) (Machines, error) {
	var m Machines
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, fmt.Errorf("read machines file: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("parse machines file %s: %w", path, err)
	}
	if err := m.validate(); err != nil {
		return m, fmt.Errorf("machines file %s: %w", path, err)
	}
	return m, nil
}

func (m Machines) validate() error {
	machines := map[string]bool{}
	for _, s := range m.Sites {
		if !ValidID(s.ID) {
			return fmt.Errorf("invalid site id %q", s.ID)
		}
		for _, mc := range s.Machines {
			if !ValidID(mc.ID) {
				return fmt.Errorf("invalid machine id %q in site %q", mc.ID, s.ID)
			}
			if machines[mc.ID] {
				return fmt.Errorf("duplicate machine id %q", mc.ID)
			}
			machines[mc.ID] = true
			for _, sn := range mc.Sensors {
				if !ValidID(sn.ID) {
					return fmt.Errorf("invalid sensor id %q on machine %q", sn.ID, mc.ID)
				}
			}
		}
	}
	return nil
}

// ValidID reports whether s is a usable site/machine/sensor/metric id:
// 1-64 chars of [a-z0-9_-] (lowercase keeps topics and URLs unambiguous).
func ValidID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getbool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func getdur(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		// Accept either a Go duration string ("120s") or a bare integer
		// number of seconds ("120").
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	return def
}
