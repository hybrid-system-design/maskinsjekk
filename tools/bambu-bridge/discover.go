package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"
)

// ssdpPort is where Bambu printers broadcast SSDP-style announcements every
// few seconds. Listening for them finds the printer even when DHCP has given
// it a new address.
const ssdpPort = 2021

// parseSSDP returns the announcement's headers with lower-cased keys.
func parseSSDP(data []byte) map[string]string {
	h := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		h[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return h
}

// announces reports whether an announcement comes from the printer with the
// given serial number.
func announces(h map[string]string, serial string) bool {
	return serial != "" && strings.EqualFold(h["usn"], serial)
}

// discover listens for the printer's announcement and returns its IP.
func discover(ctx context.Context, serial string, timeout time.Duration) (string, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: ssdpPort})
	if err != nil {
		return "", err
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return "", err
		}
		if announces(parseSSDP(buf[:n]), serial) {
			return from.IP.String(), nil
		}
	}
	return "", errors.New("no announcement from the printer (is it on, and is UDP 2021 open?)")
}
