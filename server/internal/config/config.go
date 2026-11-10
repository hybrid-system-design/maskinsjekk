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
	"strings"
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
	Kind string `json:"kind"` // free text, e.g. "esp32-vibration"; KindShellyGen2 is special
	// MQTTPrefix and Channel apply only to KindShellyGen2: the device's MQTT
	// topic prefix as set in its web UI, and which switch/pm1 component to read
	// (0 on single-channel devices such as plugs).
	MQTTPrefix string `json:"mqtt_prefix,omitempty"`
	Channel    int    `json:"channel,omitempty"`
}

// KindShellyGen2 marks a Shelly Gen2+ device (Plus/Pro/Gen3/Gen4) publishing
// its native status over MQTT. The server subscribes to its prefix and maps it
// to this machine/sensor, so the device needs no custom firmware or bridge.
const KindShellyGen2 = "shelly-gen2"

// maxShellyChannel bounds Channel; the largest Shelly devices have 4 channels.
const maxShellyChannel = 15

// ShellyDevice is a Shelly sensor resolved to where its data is stored.
type ShellyDevice struct {
	Prefix    string
	Channel   int
	SiteID    string
	MachineID string
	SensorID  string
}

// ShellyDevices lists every KindShellyGen2 sensor in the machines file.
func (m Machines) ShellyDevices() []ShellyDevice {
	var out []ShellyDevice
	for _, s := range m.Sites {
		for _, mc := range s.Machines {
			for _, sn := range mc.Sensors {
				if sn.Kind == KindShellyGen2 {
					out = append(out, ShellyDevice{
						Prefix: sn.MQTTPrefix, Channel: sn.Channel,
						SiteID: s.ID, MachineID: mc.ID, SensorID: sn.ID,
					})
				}
			}
		}
	}
	return out
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
	shelly := map[string]bool{}
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
				if sn.Kind != KindShellyGen2 {
					if sn.MQTTPrefix != "" || sn.Channel != 0 {
						return fmt.Errorf("sensor %s/%s: mqtt_prefix/channel only apply to kind %q", mc.ID, sn.ID, KindShellyGen2)
					}
					continue
				}
				if !validTopicPrefix(sn.MQTTPrefix) {
					return fmt.Errorf("sensor %s/%s: invalid mqtt_prefix %q", mc.ID, sn.ID, sn.MQTTPrefix)
				}
				if sn.Channel < 0 || sn.Channel > maxShellyChannel {
					return fmt.Errorf("sensor %s/%s: channel %d out of range 0-%d", mc.ID, sn.ID, sn.Channel, maxShellyChannel)
				}
				key := fmt.Sprintf("%s#%d", sn.MQTTPrefix, sn.Channel)
				if shelly[key] {
					return fmt.Errorf("sensor %s/%s: mqtt_prefix %q channel %d is used twice", mc.ID, sn.ID, sn.MQTTPrefix, sn.Channel)
				}
				shelly[key] = true
			}
		}
	}
	return nil
}

// validTopicPrefix accepts a concrete MQTT topic prefix: 1-128 chars, no
// wildcards or empty levels, and outside the hsd/ namespace so it can't collide
// with the sensor subscription.
func validTopicPrefix(p string) bool {
	if len(p) == 0 || len(p) > 128 || strings.ContainsAny(p, "+#\x00") {
		return false
	}
	if p == "hsd" || strings.HasPrefix(p, "hsd/") || strings.HasPrefix(p, "$") {
		return false
	}
	for _, level := range strings.Split(p, "/") {
		if level == "" {
			return false
		}
	}
	return true
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
