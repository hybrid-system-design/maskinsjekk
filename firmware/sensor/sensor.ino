// Machine vibration + temperature sensor node (Maskinsjekk).
//
// Read-only: clamps onto a machine and never touches its controls.
//
// - Samples an accelerometer at RATE_HZ (default 200) on a dedicated FreeRTOS
//   task: I2C ADXL345 / MPU6050 (auto-detected), or an analog ADXL335 (GY-61
//   breakout) read by the ESP32 ADC when `set accel adxl335` is configured.
// - Every second computes the vibration RMS with gravity/DC removed: the mean of
//   each axis over the 1 s window is subtracted, and the result is
//   sqrt(var_x + var_y + var_z) in g.
// - Publishes once per second to hsd/<site>/<machine>/<sensor>:
//     {"ts": <unix_ms>, "vib_rms": <g>, "temp_c": <float>}
//   ts is omitted until NTP has synced (the server then uses arrival time);
//   temp_c is omitted when no temperature source is configured/valid.
// - Retained hsd/<site>/<machine>/<sensor>/status: "online", or "offline" via
//   the MQTT last will when the node drops off.
// - WiFi and MQTT reconnect automatically; sampling never stops meanwhile.
// - mqtt_host may be an IP or an mDNS name ending in .local (e.g. fedora.local);
//   the name is re-resolved before every MQTT connect attempt, so the broker can
//   change IP without reconfiguring the node.
// - Network (OTA) updates via ArduinoOTA, hostname hsd-<machine>-<sensor>, once
//   an OTA password is set with `set ota_pass <secret>`; disabled until then.
// - Optional 128x64 SSD1306 OLED (I2C 0x3C/0x3D, auto-detected, shares the
//   accelerometer's bus) shows connection state, temperature, vibration and a
//   live x/y/z debug readout. Without an OLED, or without an accelerometer,
//   everything else still works.
//
// No credentials in code: everything lives in NVS and is set over the serial
// console (115200 baud). Type `help`. Example first-time setup:
//   set ssid <wifi-ssid>
//   set pass <wifi-password>
//   set mqtt_host fedora.local   # or a plain IP, e.g. 192.168.1.50
//   set site demo
//   set machine printer-1
//   set sensor esp32-1
//   set temp max6675
//   set accel adxl335       # only for the analog GY-61; omit for I2C sensors
//   set ota_pass <ota-password>  # enables network uploads
//   reboot
//
// GY-61 / ADXL335 wiring (ADC1 pins only; ADC2 is unusable while WiFi is on):
//   VCC -> 3V3 (not 5 V), GND -> GND, X -> GPIO36, Y -> GPIO39, Z -> GPIO34
//
// Libraries: PubSubClient (knolleary), Adafruit SSD1306 + Adafruit GFX (OLED).
// WiFi, Wire, Preferences, ESPmDNS, ArduinoOTA come with the ESP32 core. Accelerometers are driven
// by raw register access (no library).

#include <WiFi.h>
#include <ESPmDNS.h>
#include <ArduinoOTA.h>
#include <PubSubClient.h>
#include <Preferences.h>
#include <Wire.h>
#include <Adafruit_GFX.h>
#include <Adafruit_SSD1306.h>
#include <math.h>
#include <sys/time.h>
#include <time.h>

#include "types.h"

// MAX6675 thermocouple (bit-banged SPI) — same wiring as the temperature monitor.
#define MAX6675_SCK 18
#define MAX6675_CS  19
#define MAX6675_SO  5
// Calibration offset (°C). The reference unit read boiling water ~3 °C high.
#define MAX6675_OFFSET 3.0

// I2C accelerometer on the default pins (SDA 21, SCL 22).
#define I2C_HZ 400000

// Analog ADXL335 (GY-61) outputs. Must be ADC1 pins: ADC2 conflicts with WiFi.
#define ADXL335_X 36
#define ADXL335_Y 39
#define ADXL335_Z 34
// Sensitivity is ratiometric: ~Vs/10 per g, i.e. ~330 mV/g at 3.3 V; 0 g sits
// at Vs/2. Only the sensitivity matters here (the RMS removes the DC offset).
#define ADXL335_MV_PER_G 330.0f
#define ADXL335_OVERSAMPLE 4

// Optional SSD1306 OLED on the same bus (address 0x3C or 0x3D).
#define OLED_W 128
#define OLED_H 64

static const char *FW_VERSION = "0.2.0";

// --- config (NVS) -----------------------------------------------------------

Preferences prefs;
Config cfg;

void loadConfig() {
  cfg.ssid     = prefs.getString("ssid", "");
  cfg.pass     = prefs.getString("pass", "");
  cfg.mqttHost = prefs.getString("mqtt_host", "");
  cfg.mqttPort = prefs.getUShort("mqtt_port", 1883);
  cfg.otaPass  = prefs.getString("ota_pass", "");
  cfg.site     = prefs.getString("site", "");
  cfg.machine  = prefs.getString("machine", "");
  cfg.sensor   = prefs.getString("sensor", "");
  cfg.rateHz   = prefs.getUShort("rate", 200);
  cfg.temp     = prefs.getString("temp", "none");
  cfg.accel    = prefs.getString("accel", "auto");
}

bool configComplete() {
  return cfg.ssid.length() && cfg.mqttHost.length() && cfg.site.length() &&
         cfg.machine.length() && cfg.sensor.length();
}

// validId matches the server's rule: 1-64 chars of [a-z0-9_-].
bool validId(const String &s) {
  if (s.length() == 0 || s.length() > 64) return false;
  for (size_t i = 0; i < s.length(); i++) {
    char c = s[i];
    if (!((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_')) return false;
  }
  return true;
}

// validRate: 1000 must divide evenly so the 1 ms FreeRTOS tick gives an exact period.
bool validRate(long r) { return r >= 50 && r <= 500 && 1000 % r == 0; }

// --- accelerometer ----------------------------------------------------------

AccelType accelType = ACCEL_NONE;
uint8_t accelAddr = 0;

// The sampler task (accelerometer) and loop() (OLED refresh) share the I2C bus.
SemaphoreHandle_t i2cMutex;

bool accelUsesI2C() { return accelType == ACCEL_ADXL345 || accelType == ACCEL_MPU6050; }

// Latest raw sample, for the OLED debug readout (written by the sampler task).
volatile float dbgG[3] = {NAN, NAN, NAN};
volatile int dbgMv[3] = {0, 0, 0}; // ADXL335 only

bool i2cWrite(uint8_t addr, uint8_t reg, uint8_t val) {
  Wire.beginTransmission(addr);
  Wire.write(reg);
  Wire.write(val);
  return Wire.endTransmission() == 0;
}

bool i2cRead(uint8_t addr, uint8_t reg, uint8_t *buf, size_t len) {
  Wire.beginTransmission(addr);
  Wire.write(reg);
  if (Wire.endTransmission(false) != 0) return false;
  if (Wire.requestFrom(addr, (uint8_t)len) != len) return false;
  for (size_t i = 0; i < len; i++) buf[i] = Wire.read();
  return true;
}

// For an RMS measurement aliasing is harmless (folded energy still counts), so
// both sensors run with a wide internal bandwidth rather than a tight
// anti-alias filter.
bool initAdxl345(uint8_t addr) {
  return i2cWrite(addr, 0x2C, cfg.rateHz <= 200 ? 0x0C : 0x0D) && // ODR 400 / 800 Hz
         i2cWrite(addr, 0x31, 0x0B) &&                            // full resolution, ±16 g
         i2cWrite(addr, 0x2D, 0x08);                              // measure mode
}

bool initMpu6050(uint8_t addr) {
  return i2cWrite(addr, 0x6B, 0x01) && // wake, PLL clock
         i2cWrite(addr, 0x19, 0x00) && // sample rate divider: 1 kHz
         i2cWrite(addr, 0x1A, 0x01) && // DLPF: accel bandwidth 184 Hz
         i2cWrite(addr, 0x1C, 0x08);   // ±4 g
}

void detectAccel() {
  if (cfg.accel == "adxl335") {
    analogReadResolution(12);
    const uint8_t pins[] = {ADXL335_X, ADXL335_Y, ADXL335_Z};
    for (uint8_t p : pins) {
      pinMode(p, INPUT);
      analogSetPinAttenuation(p, ADC_11db); // ~0-3.1 V, covers the 1.65 V ± 1 V swing
    }
    accelType = ACCEL_ADXL335;
    Serial.printf("accel: ADXL335 analog on GPIO %d/%d/%d\n", ADXL335_X, ADXL335_Y, ADXL335_Z);
    return;
  }
  uint8_t id;
  const uint8_t adxl[] = {0x53, 0x1D};
  for (uint8_t a : adxl) {
    if (i2cRead(a, 0x00, &id, 1) && id == 0xE5 && initAdxl345(a)) {
      accelType = ACCEL_ADXL345;
      accelAddr = a;
      Serial.printf("accel: ADXL345 at 0x%02X\n", a);
      return;
    }
  }
  const uint8_t mpu[] = {0x68, 0x69};
  for (uint8_t a : mpu) {
    // Genuine MPU6050 reports 0x68; common clones/MPU6500 report 0x70/0x72/0x98.
    if (i2cRead(a, 0x75, &id, 1) && (id == 0x68 || id == 0x70 || id == 0x72 || id == 0x98) && initMpu6050(a)) {
      accelType = ACCEL_MPU6050;
      accelAddr = a;
      Serial.printf("accel: MPU6050-compatible (WHO_AM_I 0x%02X) at 0x%02X\n", id, a);
      return;
    }
  }
  Serial.println("accel: none found on I2C — vib_rms will not be published");
}

// readAccel returns acceleration in g; dieTemp is set for MPU6050 (else NAN).
bool readAccel(float &x, float &y, float &z, float &dieTemp) {
  uint8_t b[8];
  dieTemp = NAN;
  if (accelType == ACCEL_ADXL335) {
    const uint8_t pins[] = {ADXL335_X, ADXL335_Y, ADXL335_Z};
    float g[3];
    for (int i = 0; i < 3; i++) {
      uint32_t sum = 0;
      for (int k = 0; k < ADXL335_OVERSAMPLE; k++) sum += analogReadMilliVolts(pins[i]);
      float mv = (float)sum / ADXL335_OVERSAMPLE;
      dbgMv[i] = (int)mv;
      g[i] = (mv - 1650.0f) / ADXL335_MV_PER_G;
      dbgG[i] = g[i];
    }
    x = g[0]; y = g[1]; z = g[2];
    return true;
  }
  if (accelType == ACCEL_ADXL345) {
    if (!i2cRead(accelAddr, 0x32, b, 6)) return false;
    const float s = 0.0039f; // g/LSB in full-resolution mode
    x = (int16_t)(b[1] << 8 | b[0]) * s;
    y = (int16_t)(b[3] << 8 | b[2]) * s;
    z = (int16_t)(b[5] << 8 | b[4]) * s;
    dbgG[0] = x; dbgG[1] = y; dbgG[2] = z;
    return true;
  }
  if (accelType == ACCEL_MPU6050) {
    if (!i2cRead(accelAddr, 0x3B, b, 8)) return false;
    const float s = 1.0f / 8192.0f; // ±4 g
    x = (int16_t)(b[0] << 8 | b[1]) * s;
    y = (int16_t)(b[2] << 8 | b[3]) * s;
    z = (int16_t)(b[4] << 8 | b[5]) * s;
    dieTemp = (int16_t)(b[6] << 8 | b[7]) / 340.0f + 36.53f;
    dbgG[0] = x; dbgG[1] = y; dbgG[2] = z;
    return true;
  }
  return false;
}

// --- sampling task ----------------------------------------------------------

QueueHandle_t windowQueue;

// samplerTask reads the accelerometer at a fixed rate and emits one VibWindow
// per second. Sums are taken relative to the window's first sample to avoid
// cancellation error when subtracting the (≈1 g) mean.
void samplerTask(void *) {
  const TickType_t period = pdMS_TO_TICKS(1000 / cfg.rateHz);
  TickType_t lastWake = xTaskGetTickCount();
  uint32_t windowStart = millis();

  double sx = 0, sy = 0, sz = 0, sxx = 0, syy = 0, szz = 0, sTemp = 0;
  float ox = 0, oy = 0, oz = 0;
  uint32_t n = 0, nTemp = 0, errors = 0;

  for (;;) {
    vTaskDelayUntil(&lastWake, period);

    float x, y, z, dieTemp;
    bool got;
    if (accelUsesI2C()) { // the ADC needs no bus lock, so the OLED can't stall it
      xSemaphoreTake(i2cMutex, portMAX_DELAY);
      got = readAccel(x, y, z, dieTemp);
      xSemaphoreGive(i2cMutex);
    } else {
      got = readAccel(x, y, z, dieTemp);
    }
    if (got) {
      if (n == 0) { ox = x; oy = y; oz = z; }
      double dx = x - ox, dy = y - oy, dz = z - oz;
      sx += dx; sy += dy; sz += dz;
      sxx += dx * dx; syy += dy * dy; szz += dz * dz;
      n++;
      if (!isnan(dieTemp)) { sTemp += dieTemp; nTemp++; }
    } else if (accelType != ACCEL_NONE) {
      errors++;
    }

    uint32_t now = millis();
    if (now - windowStart >= 1000) {
      VibWindow w;
      w.n = n;
      w.errors = errors;
      w.endMillis = now;
      w.dieTemp = nTemp ? (float)(sTemp / nTemp) : NAN;
      if (n >= 10) {
        double var = (sxx / n - (sx / n) * (sx / n)) +
                     (syy / n - (sy / n) * (sy / n)) +
                     (szz / n - (sz / n) * (sz / n));
        w.rms = var > 0 ? (float)sqrt(var) : 0.0f;
      } else {
        w.rms = NAN;
      }
      xQueueSend(windowQueue, &w, 0); // drop if the main loop is badly behind

      sx = sy = sz = sxx = syy = szz = sTemp = 0;
      n = nTemp = errors = 0;
      windowStart += 1000;
      if (now - windowStart >= 1000) windowStart = now; // resync after a stall
    }
  }
}

// --- temperature ------------------------------------------------------------

bool max6675FirstSkipped = false;

// readMax6675 returns °C, or NAN if the thermocouple is open/disconnected.
float readMax6675() {
  digitalWrite(MAX6675_CS, LOW);
  delayMicroseconds(10);
  uint16_t v = shiftIn(MAX6675_SO, MAX6675_SCK, MSBFIRST);
  v <<= 8;
  v |= shiftIn(MAX6675_SO, MAX6675_SCK, MSBFIRST);
  digitalWrite(MAX6675_CS, HIGH);
  if (v & 0x4) return NAN; // no thermocouple attached
  return (v >> 3) * 0.25f - MAX6675_OFFSET;
}

float readTemperature(const VibWindow &w) {
  if (cfg.temp == "max6675") {
    float t = readMax6675();
    if (!max6675FirstSkipped) { // the first conversion after power-up is unreliable
      max6675FirstSkipped = true;
      return NAN;
    }
    return t;
  }
  if (cfg.temp == "mpu6050") return w.dieTemp;
  return NAN;
}

// --- network ----------------------------------------------------------------

WiFiClient net;
PubSubClient mqtt(net);
String dataTopic, statusTopic, clientId;
unsigned long lastWifiAttempt = 0, lastMqttAttempt = 0;
bool ntpStarted = false;

// Broker address. setServer(const char*) keeps the pointer, so the host string
// is a private copy that never changes after setup() (cfg is reloaded by `set`).
String brokerHost;     // as configured: IP or name
String brokerMdnsName; // name without ".local" when using mDNS, else empty
IPAddress brokerIp;    // last mDNS result, kept as a fallback if a query fails
bool mdnsStarted = false;
bool otaEnabled = false;

bool timeSynced() { return time(nullptr) > 1700000000; }

uint64_t nowUnixMs() {
  struct timeval tv;
  gettimeofday(&tv, nullptr);
  return (uint64_t)tv.tv_sec * 1000ULL + tv.tv_usec / 1000;
}

void maintainWifi() {
  if (WiFi.status() == WL_CONNECTED) {
    if (!ntpStarted) {
      configTime(0, 0, "pool.ntp.org", "time.google.com"); // UTC; SNTP re-syncs periodically
      ntpStarted = true;
      Serial.print("wifi: connected, ip ");
      Serial.println(WiFi.localIP());
    }
    if (!mdnsStarted) startNetworkServices();
    return;
  }
  // The core auto-reconnects; kick it explicitly if it has been down a while.
  if (millis() - lastWifiAttempt > 30000) {
    lastWifiAttempt = millis();
    Serial.println("wifi: (re)connecting");
    WiFi.disconnect();
    WiFi.begin(cfg.ssid.c_str(), cfg.pass.c_str());
  }
}

// resolveBroker points the MQTT client at the broker. For an mDNS name it
// queries the network (blocking up to ~1.5 s; the sampler task keeps running
// and the window queue absorbs the gap). Returns false if there is no address.
bool resolveBroker() {
  if (brokerMdnsName.length() == 0) return true; // plain IP/host, set in setup()
  if (!mdnsStarted) return false;
  IPAddress ip = MDNS.queryHost(brokerMdnsName, 1500);
  if (ip != IPAddress((uint32_t)0)) {
    if (ip != brokerIp) {
      Serial.printf("mdns: %s -> %s\n", brokerHost.c_str(), ip.toString().c_str());
      brokerIp = ip;
    }
  } else if (brokerIp != IPAddress((uint32_t)0)) {
    Serial.printf("mdns: %s not answering, trying last known %s\n", brokerHost.c_str(), brokerIp.toString().c_str());
  } else {
    Serial.printf("mdns: could not resolve %s\n", brokerHost.c_str());
    return false;
  }
  mqtt.setServer(brokerIp, cfg.mqttPort);
  return true;
}

void maintainMqtt() {
  if (WiFi.status() != WL_CONNECTED || mqtt.connected()) return;
  if (millis() - lastMqttAttempt < 5000) return; // backoff; sampling continues meanwhile
  lastMqttAttempt = millis();
  if (!resolveBroker()) return;
  if (mqtt.connect(clientId.c_str(), nullptr, nullptr, statusTopic.c_str(), 1, true, "offline")) {
    mqtt.publish(statusTopic.c_str(), "online", true);
    Serial.println("mqtt: connected");
  } else {
    Serial.printf("mqtt: connect failed, rc=%d\n", mqtt.state());
  }
}

uint32_t published = 0, dropped = 0;
VibWindow lastWindow = {NAN, NAN, 0, 0, 0};
float lastTemp = NAN;

void publishWindow(const VibWindow &w) {
  lastWindow = w;
  float temp = readTemperature(w);
  lastTemp = temp;

  char payload[128];
  int len = snprintf(payload, sizeof(payload), "{");
  bool first = true;
  if (timeSynced()) {
    uint64_t ts = nowUnixMs() - (millis() - w.endMillis);
    len += snprintf(payload + len, sizeof(payload) - len, "\"ts\":%llu", (unsigned long long)ts);
    first = false;
  }
  if (!isnan(w.rms)) {
    len += snprintf(payload + len, sizeof(payload) - len, "%s\"vib_rms\":%.5f", first ? "" : ",", w.rms);
    first = false;
  }
  if (!isnan(temp)) {
    len += snprintf(payload + len, sizeof(payload) - len, "%s\"temp_c\":%.2f", first ? "" : ",", temp);
    first = false;
  }
  snprintf(payload + len, sizeof(payload) - len, "}");
  if (first) return; // nothing measurable this second

  if (mqtt.connected() && mqtt.publish(dataTopic.c_str(), payload)) {
    published++;
  } else {
    dropped++;
  }
  if ((published + dropped) % 10 == 0) {
    Serial.printf("%s %s  [published %u, dropped %u, i2c errors %u]\n",
                  dataTopic.c_str(), payload, published, dropped, w.errors);
  }
}

// --- OLED -------------------------------------------------------------------

// Clock is kept at I2C_HZ after each transfer so the library doesn't slow the
// accelerometer's bus down to its 100 kHz default.
Adafruit_SSD1306 oled(OLED_W, OLED_H, &Wire, -1, I2C_HZ, I2C_HZ);
bool oledPresent = false;

void initOled() {
  const uint8_t addrs[] = {0x3C, 0x3D};
  for (uint8_t a : addrs) {
    Wire.beginTransmission(a);
    if (Wire.endTransmission() != 0) continue;
    if (oled.begin(SSD1306_SWITCHCAPVCC, a)) {
      oledPresent = true;
      oled.setTextColor(SSD1306_WHITE);
      Serial.printf("oled: SSD1306 at 0x%02X\n", a);
      return;
    }
  }
  Serial.println("oled: none found");
}

// oledMessage shows two short lines (boot / config problems).
void oledMessage(const char *l1, const char *l2) {
  if (!oledPresent) return;
  xSemaphoreTake(i2cMutex, portMAX_DELAY);
  oled.clearDisplay();
  oled.setTextSize(1);
  oled.setCursor(0, 0);
  oled.println("Maskinsjekk");
  oled.setCursor(0, 20);
  oled.println(l1);
  oled.println(l2);
  oled.display();
  xSemaphoreGive(i2cMutex);
}

// updateOled redraws 4x/s so the x/y/z readout is live (debug for now). With an
// I2C accelerometer each redraw briefly holds the bus, costing it a few samples;
// the analog ADXL335 is unaffected. Raise the interval if that matters.
void updateOled() {
  static unsigned long last = 0;
  if (!oledPresent || millis() - last < 250) return;
  last = millis();

  char buf[24];
  xSemaphoreTake(i2cMutex, portMAX_DELAY);
  oled.clearDisplay();
  oled.setTextSize(1);

  oled.setCursor(0, 0);
  oled.print(cfg.machine.substring(0, 21));

  oled.setCursor(0, 9);
  oled.print("WiFi ");
  if (WiFi.status() == WL_CONNECTED) oled.print(WiFi.localIP()); else oled.print("--");

  oled.setCursor(0, 18);
  oled.printf("MQTT %s  NTP %s", mqtt.connected() ? "ok" : "--", timeSynced() ? "ok" : "--");

  // Temperature (left, 2x) and vibration RMS (right).
  if (isnan(lastTemp)) snprintf(buf, sizeof(buf), "--.- C");
  else snprintf(buf, sizeof(buf), "%.1f C", lastTemp);
  oled.setTextSize(2);
  oled.setCursor(0, 28);
  oled.print(buf);

  oled.setTextSize(1);
  oled.setCursor(78, 28);
  oled.print("vib rms");
  oled.setCursor(78, 37);
  if (accelType == ACCEL_NONE) oled.print("no sensor");
  else if (isnan(lastWindow.rms)) oled.print("--");
  else oled.printf("%.4f g", lastWindow.rms);

  // Debug: latest raw sample in g, and for the analog sensor the pin voltages.
  oled.setCursor(0, 46);
  if (accelType == ACCEL_NONE) {
    oled.print("accel: none");
  } else {
    oled.printf("x%+.2f y%+.2f z%+.2f", (float)dbgG[0], (float)dbgG[1], (float)dbgG[2]);
    oled.setCursor(0, 55);
    if (accelType == ACCEL_ADXL335) oled.printf("mV %d %d %d", (int)dbgMv[0], (int)dbgMv[1], (int)dbgMv[2]);
    else oled.printf("n %u err %u", (unsigned)lastWindow.n, (unsigned)lastWindow.errors);
  }

  oled.display();
  xSemaphoreGive(i2cMutex);
}

// --- mDNS + OTA -------------------------------------------------------------

String otaHostname() {
  String h = "hsd-" + cfg.machine + "-" + cfg.sensor;
  return h.substring(0, 63); // DNS label limit
}

// startNetworkServices runs once WiFi is first up. ArduinoOTA.begin() starts
// mDNS itself (and advertises the upload service); without OTA we start it
// plainly so .local broker names still resolve. While an upload runs,
// ArduinoOTA.handle() blocks loop(); the sampler task keeps running and its
// surplus windows are dropped, and the node reboots when the upload completes.
void startNetworkServices() {
  mdnsStarted = true;
  String host = otaHostname();
  if (cfg.otaPass.length() == 0) {
    if (!MDNS.begin(host)) Serial.println("mdns: failed to start");
    Serial.println("ota: disabled (set ota_pass to enable)");
    return;
  }
  ArduinoOTA.setHostname(host.c_str());
  ArduinoOTA.setPassword(cfg.otaPass.c_str());
  ArduinoOTA
      .onStart([]() {
        Serial.println("ota: update started");
        oledMessage("OTA update", "starting...");
      })
      .onProgress([](unsigned int done, unsigned int total) {
        static int lastPct = -1;
        int pct = total ? (int)(100ULL * done / total) : 0;
        if (pct == lastPct) return;
        lastPct = pct;
        char buf[12];
        snprintf(buf, sizeof(buf), "OTA %d%%", pct);
        oledMessage("OTA update", buf); // takes i2cMutex
        if (pct % 10 == 0) Serial.printf("ota: %d%%\n", pct);
      })
      .onEnd([]() {
        Serial.println("ota: done, rebooting");
        oledMessage("OTA done", "rebooting...");
      })
      .onError([](ota_error_t e) {
        Serial.printf("ota: error %u\n", (unsigned)e);
        oledMessage("OTA failed", e == OTA_AUTH_ERROR ? "bad password" : "see serial");
      });
  ArduinoOTA.begin();
  otaEnabled = true;
  Serial.printf("ota: enabled as %s.local\n", host.c_str());
}

// --- serial console ---------------------------------------------------------

String line;

void printHelp() {
  Serial.println(
      "commands:\n"
      "  show                 print config (password masked)\n"
      "  status               connection + last reading\n"
      "  set <key> <value>    keys: ssid pass mqtt_host mqtt_port site machine sensor rate temp accel ota_pass\n"
      "                       mqtt_host: IP, or mDNS name ending in .local (e.g. fedora.local)\n"
      "                       ota_pass: enables network (OTA) uploads; unset = disabled\n"
      "                       rate: 50..500 Hz dividing 1000 (default 200)\n"
      "                       temp: none | max6675 | mpu6050\n"
      "                       accel: auto (I2C ADXL345/MPU6050) | adxl335 (analog GY-61)\n"
      "  clear                erase all config\n"
      "  reboot               restart and apply config");
}

void printConfig() {
  Serial.printf("ssid=%s pass=%s mqtt=%s:%u ota_pass=%s\nsite=%s machine=%s sensor=%s rate=%uHz temp=%s accel=%s\n",
                cfg.ssid.c_str(), cfg.pass.length() ? "****" : "(empty)", cfg.mqttHost.c_str(), cfg.mqttPort,
                cfg.otaPass.length() ? "****" : "(empty)",
                cfg.site.c_str(), cfg.machine.c_str(), cfg.sensor.c_str(), cfg.rateHz, cfg.temp.c_str(), cfg.accel.c_str());
}

void handleCommand(String cmd) {
  cmd.trim();
  if (cmd.length() == 0) return;
  int sp = cmd.indexOf(' ');
  String verb = sp < 0 ? cmd : cmd.substring(0, sp);

  if (verb == "help") { printHelp(); return; }
  if (verb == "show") { printConfig(); return; }
  if (verb == "reboot") { Serial.println("rebooting"); delay(100); ESP.restart(); }
  if (verb == "clear") { prefs.clear(); loadConfig(); Serial.println("config erased; reboot to apply"); return; }
  if (verb == "status") {
    Serial.printf("fw %s, oled %s, wifi %s, mqtt %s, ntp %s, accel %s\n", FW_VERSION,
                  oledPresent ? "yes" : "no",
                  WiFi.status() == WL_CONNECTED ? "up" : "down", mqtt.connected() ? "up" : "down",
                  timeSynced() ? "synced" : "not synced",
                  accelType == ACCEL_ADXL345 ? "ADXL345" : accelType == ACCEL_MPU6050 ? "MPU6050" :
                    accelType == ACCEL_ADXL335 ? "ADXL335 (analog)" : "none");
    String broker = brokerHost;
    if (brokerMdnsName.length()) {
      broker += " -> ";
      broker += brokerIp != IPAddress((uint32_t)0) ? brokerIp.toString() : String("unresolved");
    }
    String ota = otaEnabled ? "enabled as " + otaHostname() + ".local"
                            : String(cfg.otaPass.length() ? "pending (wifi/reboot)" : "disabled");
    Serial.printf("broker %s, ota %s\n", broker.c_str(), ota.c_str());
    Serial.printf("last window: vib_rms=%.5f g, n=%u, i2c errors=%u, temp_c=%.2f\n",
                  lastWindow.rms, lastWindow.n, lastWindow.errors, lastTemp);
    return;
  }
  if (verb != "set" || sp < 0) { Serial.println("unknown command; type help"); return; }

  String rest = cmd.substring(sp + 1);
  int sp2 = rest.indexOf(' ');
  if (sp2 < 0) { Serial.println("usage: set <key> <value>"); return; }
  String key = rest.substring(0, sp2);
  String val = rest.substring(sp2 + 1); // may contain spaces (SSIDs)

  if (key == "ssid" || key == "pass" || key == "mqtt_host" || key == "ota_pass") {
    prefs.putString(key.c_str(), val);
  } else if (key == "site" || key == "machine" || key == "sensor") {
    if (!validId(val)) { Serial.println("ids must be 1-64 chars of a-z 0-9 - _"); return; }
    prefs.putString(key.c_str(), val);
  } else if (key == "mqtt_port") {
    long p = val.toInt();
    if (p <= 0 || p > 65535) { Serial.println("invalid port"); return; }
    prefs.putUShort("mqtt_port", (uint16_t)p);
  } else if (key == "rate") {
    long r = val.toInt();
    if (!validRate(r)) { Serial.println("rate must be 50..500 and divide 1000 (e.g. 100, 200, 250, 500)"); return; }
    prefs.putUShort("rate", (uint16_t)r);
  } else if (key == "temp") {
    if (val != "none" && val != "max6675" && val != "mpu6050") { Serial.println("temp: none | max6675 | mpu6050"); return; }
    prefs.putString("temp", val);
  } else if (key == "accel") {
    if (val != "auto" && val != "adxl335") { Serial.println("accel: auto | adxl335"); return; }
    prefs.putString("accel", val);
  } else {
    Serial.println("unknown key; type help");
    return;
  }
  loadConfig();
  Serial.println("saved; reboot to apply");
}

void pollSerial() {
  while (Serial.available()) {
    char c = Serial.read();
    if (c == '\r') continue;
    if (c == '\n') {
      handleCommand(line);
      line = "";
    } else if (line.length() < 160) {
      line += c;
    }
  }
}

// --- main -------------------------------------------------------------------

bool running = false;

void setup() {
  Serial.begin(115200);
  delay(200);
  Serial.printf("\nmaskinsjekk sensor fw %s\n", FW_VERSION);

  prefs.begin("hsd", false);
  loadConfig();
  if (!validRate(cfg.rateHz)) cfg.rateHz = 200;

  i2cMutex = xSemaphoreCreateMutex();
  Wire.begin();
  Wire.setClock(I2C_HZ);
  initOled();

  if (!configComplete()) {
    Serial.println("config incomplete — set ssid, mqtt_host, site, machine, sensor (type help)");
    printConfig();
    oledMessage("Config missing", "see serial console");
    return; // console only until configured and rebooted
  }
  printConfig();
  oledMessage("Starting...", FW_VERSION);

  pinMode(MAX6675_CS, OUTPUT);
  pinMode(MAX6675_SCK, OUTPUT);
  pinMode(MAX6675_SO, INPUT);
  digitalWrite(MAX6675_CS, HIGH);

  detectAccel();

  String base = "hsd/" + cfg.site + "/" + cfg.machine + "/" + cfg.sensor;
  dataTopic = base;
  statusTopic = base + "/status";
  clientId = "hsd-" + cfg.machine + "-" + cfg.sensor;

  WiFi.mode(WIFI_STA);
  WiFi.setAutoReconnect(true);
  WiFi.begin(cfg.ssid.c_str(), cfg.pass.c_str());
  lastWifiAttempt = millis();

  brokerHost = cfg.mqttHost;
  brokerHost.trim();
  if (brokerHost.endsWith(".local")) {
    brokerMdnsName = brokerHost.substring(0, brokerHost.length() - 6); // resolved per attempt
  } else {
    mqtt.setServer(brokerHost.c_str(), cfg.mqttPort);
  }
  mqtt.setKeepAlive(15);
  mqtt.setSocketTimeout(5); // bound how long a dead broker can block loop()

  windowQueue = xQueueCreate(5, sizeof(VibWindow));
  // Core 1 alongside loop(), above its priority; WiFi runs on core 0.
  xTaskCreatePinnedToCore(samplerTask, "sampler", 4096, nullptr, 5, nullptr, 1);
  running = true;
}

void loop() {
  pollSerial();
  if (!running) {
    static unsigned long lastHint = 0;
    if (millis() - lastHint > 15000) {
      lastHint = millis();
      Serial.println("waiting for config (type help)");
    }
    delay(10);
    return;
  }

  maintainWifi();
  if (otaEnabled) ArduinoOTA.handle();
  maintainMqtt();
  mqtt.loop();

  VibWindow w;
  while (xQueueReceive(windowQueue, &w, 0) == pdTRUE) {
    publishWindow(w);
  }
  updateOled();
  delay(5);
}
