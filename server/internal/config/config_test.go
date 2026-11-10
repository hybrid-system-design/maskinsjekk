package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMachinesMissingFileIsEmpty(t *testing.T) {
	m, err := LoadMachines(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || len(m.Sites) != 0 {
		t.Fatalf("m=%+v err=%v", m, err)
	}
}

func TestLoadMachines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machines.json")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(`{"sites":[{"id":"demo","name":"Demo","machines":[{"id":"printer-1","name":"3D-printer","sensors":[{"id":"esp32-1","kind":"esp32-vibration"}]}]}]}`)
	m, err := LoadMachines(path)
	if err != nil {
		t.Fatalf("valid file: %v", err)
	}
	if m.Sites[0].Machines[0].Sensors[0].ID != "esp32-1" {
		t.Fatalf("parsed wrong: %+v", m)
	}

	bad := map[string]string{
		"malformed":         `{"sites":[`,
		"uppercase id":      `{"sites":[{"id":"Demo"}]}`,
		"duplicate machine": `{"sites":[{"id":"a","machines":[{"id":"m"}]},{"id":"b","machines":[{"id":"m"}]}]}`,
		"bad sensor id":     `{"sites":[{"id":"a","machines":[{"id":"m","sensors":[{"id":"x/y"}]}]}]}`,
		"shelly no prefix":  `{"sites":[{"id":"a","machines":[{"id":"m","sensors":[{"id":"p","kind":"shelly-gen2"}]}]}]}`,
		"shelly wildcard":   `{"sites":[{"id":"a","machines":[{"id":"m","sensors":[{"id":"p","kind":"shelly-gen2","mqtt_prefix":"plug/#"}]}]}]}`,
		"shelly in hsd/":    `{"sites":[{"id":"a","machines":[{"id":"m","sensors":[{"id":"p","kind":"shelly-gen2","mqtt_prefix":"hsd/plug"}]}]}]}`,
		"shelly empty lvl":  `{"sites":[{"id":"a","machines":[{"id":"m","sensors":[{"id":"p","kind":"shelly-gen2","mqtt_prefix":"a//b"}]}]}]}`,
		"shelly channel":    `{"sites":[{"id":"a","machines":[{"id":"m","sensors":[{"id":"p","kind":"shelly-gen2","mqtt_prefix":"plug","channel":16}]}]}]}`,
		"shelly duplicate":  `{"sites":[{"id":"a","machines":[{"id":"m","sensors":[{"id":"p","kind":"shelly-gen2","mqtt_prefix":"plug"}]},{"id":"n","sensors":[{"id":"p","kind":"shelly-gen2","mqtt_prefix":"plug"}]}]}]}`,
		"prefix wrong kind": `{"sites":[{"id":"a","machines":[{"id":"m","sensors":[{"id":"p","kind":"shelly-plug","mqtt_prefix":"plug"}]}]}]}`,
	}
	for name, body := range bad {
		write(body)
		if _, err := LoadMachines(path); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestShellyDevices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machines.json")
	body := `{"sites":[{"id":"demo","machines":[
		{"id":"printer-1","sensors":[{"id":"esp32-1","kind":"esp32-vibration"},{"id":"shelly-1","kind":"shelly-gen2","mqtt_prefix":"shelly-printer-1"}]},
		{"id":"saw","sensors":[{"id":"pm","kind":"shelly-gen2","mqtt_prefix":"hall/pro4pm","channel":2}]},
		{"id":"lathe","sensors":[{"id":"pm","kind":"shelly-gen2","mqtt_prefix":"hall/pro4pm","channel":3}]}]}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMachines(path)
	if err != nil {
		t.Fatalf("LoadMachines: %v", err)
	}
	got := m.ShellyDevices()
	want := []ShellyDevice{
		{Prefix: "shelly-printer-1", SiteID: "demo", MachineID: "printer-1", SensorID: "shelly-1"},
		{Prefix: "hall/pro4pm", Channel: 2, SiteID: "demo", MachineID: "saw", SensorID: "pm"},
		{Prefix: "hall/pro4pm", Channel: 3, SiteID: "demo", MachineID: "lathe", SensorID: "pm"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("device %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestValidID(t *testing.T) {
	for _, ok := range []string{"a", "printer-1", "esp32_1", "0"} {
		if !ValidID(ok) {
			t.Errorf("ValidID(%q) = false", ok)
		}
	}
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	for _, bad := range []string{"", "A", "a b", "a/b", "+", "#", "æ", string(long)} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
}
