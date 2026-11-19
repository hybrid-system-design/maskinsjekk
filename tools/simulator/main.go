// Command simulator publishes a realistic machine profile to the MQTT broker so
// demos and development never depend on the physical sensor or venue wifi.
//
// It models a machine cycling through OFF -> HEATING -> RUNNING -> IDLE -> OFF,
// with some jobs aborted mid-run (unplanned stops). The true state is logged on
// every transition, which doubles as ground truth for testing state detection.
//
// It publishes two sensors: vibration + temperature (as an ESP32 node would)
// and power (as a smart plug would), so the full state machine can be
// exercised.
//
// It first backfills the last N hours (each message carries its true past
// timestamp, which the server honors via its sanity window), then — with
// --live — keeps emitting fresh samples once per second.
//
//	go run . --broker tcp://localhost:1883                 # backfill as machine "sim-printer"
//	go run . --broker tcp://localhost:1883 --live          # backfill then keep emitting
package main

import (
	"encoding/json"
	"flag"
	"log"
	"math"
	"math/rand"
	"strconv"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

func main() {
	broker := flag.String("broker", "tcp://localhost:1883", "MQTT broker URL")
	site := flag.String("site", "demo", "site id")
	machineID := flag.String("machine", "sim-printer", "machine id (defaults to 'sim-printer' so demo data never mixes with a real machine)")
	sensor := flag.String("sensor", "esp32-1", "vibration/temperature sensor id")
	powerSensor := flag.String("power-sensor", "power", "power sensor id (empty = don't publish power)")
	hours := flag.Float64("hours", 6, "hours of history to backfill (0 = none)")
	step := flag.Duration("step", 5*time.Second, "interval between backfilled samples")
	live := flag.Bool("live", false, "after backfill, keep publishing a sample every second")
	seed := flag.Int64("seed", 1, "random seed (same seed = same job schedule)")
	abortRate := flag.Float64("abort-rate", 0.25, "fraction of jobs that stop unplanned mid-run")
	flag.Parse()

	topic := "hsd/" + *site + "/" + *machineID + "/" + *sensor
	powerTopic := ""
	if *powerSensor != "" {
		powerTopic = "hsd/" + *site + "/" + *machineID + "/" + *powerSensor
	}

	opts := mqtt.NewClientOptions().
		AddBroker(*broker).
		SetClientID("maskinsjekk-simulator-" + *machineID).
		SetAutoReconnect(true).
		SetConnectRetry(true)
	client := mqtt.NewClient(opts)
	if tok := client.Connect(); tok.Wait() && tok.Error() != nil {
		log.Fatalf("connect %s: %v", *broker, tok.Error())
	}
	defer client.Disconnect(250)
	log.Printf("connected to %s, publishing to %q", *broker, topic)

	now := time.Now()
	from := now.Add(-time.Duration(*hours * float64(time.Hour)))
	m := newMachine(rand.New(rand.NewSource(*seed)), from, *abortRate)

	count := 0
	for t := from; t.Before(now); t = t.Add(*step) {
		publish(client, topic, powerTopic, m.step(t, *step))
		count++
	}
	log.Printf("backfilled %d samples over %.1fh", count, *hours)

	if !*live {
		return
	}
	log.Printf("live mode: emitting every 1s (ctrl-c to stop)")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for tNow := range ticker.C {
		publish(client, topic, powerTopic, m.step(tNow, time.Second))
	}
}

// --- machine model ---

type state string

const (
	off     state = "OFF"
	heating state = "HEATING"
	running state = "RUNNING"
	idle    state = "IDLE"
)

type reading struct {
	ts     time.Time
	vibRMS float64 // g, gravity removed
	tempC  float64 // ambient near the machine
	powerW float64 // at the plug
}

// machine is a small stochastic state model. Segment lengths are drawn from the
// seeded RNG, so a given seed always produces the same schedule.
type machine struct {
	rng       *rand.Rand
	abortRate float64

	state    state
	until    time.Time // end of current segment
	aborting bool      // current RUNNING segment ends unplanned
	tempC    float64
	jobNo    int
}

func newMachine(rng *rand.Rand, start time.Time, abortRate float64) *machine {
	m := &machine{rng: rng, abortRate: abortRate, tempC: 22}
	m.enter(off, start)
	return m
}

func (m *machine) minutes(lo, hi float64) time.Duration {
	return time.Duration((lo + m.rng.Float64()*(hi-lo)) * float64(time.Minute))
}

func (m *machine) enter(s state, t time.Time) {
	m.state = s
	switch s {
	case off:
		m.until = t.Add(m.minutes(10, 45))
	case heating:
		m.jobNo++
		m.until = t.Add(m.minutes(3, 7))
	case running:
		m.aborting = m.rng.Float64() < m.abortRate
		d := m.minutes(30, 110)
		if m.aborting {
			d = time.Duration(float64(d) * (0.2 + 0.6*m.rng.Float64()))
		}
		m.until = t.Add(d)
	case idle:
		m.until = t.Add(m.minutes(3, 20))
	}
	note := ""
	if s == heating {
		note = " (job " + strconv.Itoa(m.jobNo) + ")"
	}
	if s == running && m.aborting {
		note = " (will stop unplanned)"
	}
	log.Printf("truth %s  %-7s until %s%s", t.Format(time.RFC3339), s, m.until.Format("15:04:05"), note)
}

// step advances the model to t and returns the sensor reading there.
func (m *machine) step(t time.Time, dt time.Duration) reading {
	for !t.Before(m.until) {
		switch m.state {
		case off:
			m.enter(heating, m.until)
		case heating:
			m.enter(running, m.until)
		case running:
			if m.aborting {
				log.Printf("truth %s  job %d aborted", m.until.Format(time.RFC3339), m.jobNo)
				// After an unplanned stop the operator either leaves it idling
				// or cuts power.
				if m.rng.Float64() < 0.5 {
					m.enter(idle, m.until)
				} else {
					m.enter(off, m.until)
				}
			} else {
				log.Printf("truth %s  job %d completed", m.until.Format(time.RFC3339), m.jobNo)
				m.enter(idle, m.until)
			}
		case idle:
			m.enter(off, m.until)
		}
	}

	noise := func(scale float64) float64 { return m.rng.NormFloat64() * scale }
	var vib, targetTemp, power float64
	switch m.state {
	case off:
		vib, targetTemp, power = 0.002+math.Abs(noise(0.0005)), 22, 0
	case heating:
		vib, targetTemp = 0.005+math.Abs(noise(0.001)), 27 // fans only
		power = 250 + noise(20)                            // heaters on full
	case running:
		// Motion-dominated, with slow variation as the toolpath changes.
		wobble := 0.03 * math.Sin(float64(t.Unix())/90)
		vib, targetTemp = math.Max(0.02, 0.08+wobble+noise(0.012)), 31
		power = 150 + noise(15) // heaters holding temperature, motors
	case idle:
		vib, targetTemp, power = 0.004+math.Abs(noise(0.001)), 26, 9+noise(0.5) // standby
	}
	// First-order thermal lag (~10 min time constant).
	alpha := 1 - math.Exp(-dt.Seconds()/600)
	m.tempC += (targetTemp - m.tempC) * alpha

	return reading{ts: t, vibRMS: vib, tempC: m.tempC + noise(0.05), powerW: math.Max(0, power)}
}

// --- publishing ---

func publish(client mqtt.Client, topic, powerTopic string, r reading) {
	send(client, topic, map[string]any{
		"ts":      r.ts.UnixMilli(),
		"vib_rms": round(r.vibRMS, 4),
		"temp_c":  round(r.tempC, 2),
	})
	if powerTopic != "" {
		send(client, powerTopic, map[string]any{"ts": r.ts.UnixMilli(), "power_w": round(r.powerW, 1)})
	}
}

func send(client mqtt.Client, topic string, msg map[string]any) {
	payload, err := json.Marshal(msg)
	if err != nil {
		log.Printf("marshal: %v", err)
		return
	}
	if tok := client.Publish(topic, 1, false, payload); tok.Wait() && tok.Error() != nil {
		log.Printf("publish: %v", tok.Error())
	}
}

func round(v float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(v*p) / p
}
