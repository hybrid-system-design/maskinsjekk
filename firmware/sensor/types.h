// Types shared across the sketch. They live in a header because the Arduino
// preprocessor emits function prototypes above any struct defined in the .ino.
#pragma once

#include <Arduino.h>

struct Config {
  String ssid, pass;
  String mqttHost; // IP, or an mDNS name ending in .local (resolved on each connect)
  uint16_t mqttPort;
  String otaPass;  // empty = network OTA disabled
  String site, machine, sensor;
  uint16_t rateHz;
  String accel; // auto (I2C ADXL345/MPU6050 detection) | adxl335 (analog, e.g. GY-61)
  String temp; // none | max6675 | mpu6050 (die temperature: board, not ambient)
};

enum AccelType { ACCEL_NONE, ACCEL_ADXL345, ACCEL_MPU6050, ACCEL_ADXL335 };

struct VibWindow {
  float rms;          // g, NAN if too few samples
  float dieTemp;      // mean MPU6050 die temperature, NAN otherwise
  uint32_t n;         // good samples in the window
  uint32_t errors;    // failed I2C reads in the window
  uint32_t endMillis; // millis() at window end, to back-date ts precisely
};
