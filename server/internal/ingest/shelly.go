package ingest

import (
	"encoding/json"
	"fmt"
	"time"

	"maskinsjekk/internal/config"
	"maskinsjekk/internal/store"
)

// Shelly Gen2+ devices (Plus/Pro/Gen3/Gen4) publish their native component
// status when "Generic status update over MQTT" is enabled:
//
//	<prefix>/status/switch:<n>   relays with metering (plugs, Plus 1PM, Pro 4PM)
//	<prefix>/status/pm1:<n>      meter-only devices (Plus PM Mini)
//
// Payload (abridged):
//
//	{"id":0, "output":true, "apower":182.3, "voltage":231.2, "current":0.81,
//	 "freq":50.0, "aenergy":{"total":13.0, ...}, "temperature":{"tC":35.8, ...}}
//
// The machines file maps each prefix/channel to a machine and sensor, so the
// device's own topic names never leak into the data model.

// shellyStatus is the subset of a switch/pm1 status we store. Pointers tell
// "absent" (device model doesn't report it) apart from a real zero.
type shellyStatus struct {
	Output  *bool    `json:"output"`
	APower  *float64 `json:"apower"`
	Voltage *float64 `json:"voltage"`
	Current *float64 `json:"current"`
	Freq    *float64 `json:"freq"`
	PF      *float64 `json:"pf"`
	AEnergy *struct {
		Total *float64 `json:"total"`
	} `json:"aenergy"`
	Temperature *struct {
		TC *float64 `json:"tC"`
	} `json:"temperature"`
}

// shellyTopics returns the status topics a device's channel publishes on.
func shellyTopics(d config.ShellyDevice) []string {
	return []string{
		fmt.Sprintf("%s/status/switch:%d", d.Prefix, d.Channel),
		fmt.Sprintf("%s/status/pm1:%d", d.Prefix, d.Channel),
	}
}

// parseShellyStatus normalizes one Shelly status payload into measurements for
// the mapped machine/sensor. Shelly status carries no reading timestamp
// (aenergy.minute_ts is the start of the energy minute), so arrival time is
// used. Like parseSensorMessage it is pure and fuzzed; ok=false means drop.
//
// Metrics: power_w, voltage_v, current_a, freq_hz, pf, energy_wh (lifetime
// counter, Wh), device_temp_c (the meter's own temperature; named apart from
// a machine sensor's temp_c because series are keyed by machine + metric),
// output (relay 1/0, switch only).
func parseShellyStatus(raw []byte, d config.ShellyDevice, now time.Time) ([]store.Measurement, string, bool) {
	var st shellyStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, "bad json (want a Shelly switch/pm1 status object)", false
	}

	nowMS := now.UnixMilli()
	var out []store.Measurement
	add := func(metric string, v *float64) {
		if v == nil {
			return
		}
		out = append(out, store.Measurement{
			SiteID: d.SiteID, MachineID: d.MachineID, SensorID: d.SensorID,
			Metric: metric, Value: *v, TS: nowMS, ReceivedAt: nowMS,
		})
	}
	add("power_w", st.APower)
	add("voltage_v", st.Voltage)
	add("current_a", st.Current)
	add("freq_hz", st.Freq)
	add("pf", st.PF)
	if st.AEnergy != nil {
		add("energy_wh", st.AEnergy.Total)
	}
	if st.Temperature != nil {
		add("device_temp_c", st.Temperature.TC)
	}
	if st.Output != nil {
		v := 0.0
		if *st.Output {
			v = 1
		}
		add("output", &v)
	}
	if len(out) == 0 {
		return nil, "no known Shelly status fields", false
	}
	return out, "", true
}
