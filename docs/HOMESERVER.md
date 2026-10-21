# Running Maskinsjekk on a home server

Target: an old laptop or PC (e.g. i3-4010U, 8 GB, SSD) running Linux headless.
The stack (Mosquitto + Go service + SQLite) needs only a few percent CPU and
well under 1 GB RAM. An SSD is required: raw data is written at 1 Hz.

## 1. Host setup (Fedora; Debian/Ubuntu differ only in package commands)

```bash
# Docker CE (Fedora ships Podman; Docker is the least-surprise path for this repo)
sudo dnf config-manager addrepo --from-repofile=https://download.docker.com/linux/fedora/docker-ce.repo
sudo dnf install docker-ce docker-ce-cli containerd.io docker-compose-plugin
sudo systemctl enable --now docker

# Firewall: MQTT from the sensors, HTTP for the live page / API
sudo firewall-cmd --permanent --add-port=1883/tcp --add-port=8080/tcp
sudo firewall-cmd --reload
```

Laptop specifics:

- Keep it running with the lid closed: set `HandleLidSwitch=ignore` and
  `HandleLidSwitchExternalPower=ignore` in `/etc/systemd/logind.conf`, then
  `sudo systemctl restart systemd-logind`. Disable suspend if it is a desktop
  install: `sudo systemctl mask sleep.target suspend.target hibernate.target`.
- An old battery kept at 100 % wears out and can swell: set a charge limit
  (e.g. with TLP) or remove the battery.
- Enable automatic updates (`dnf-automatic`). Fedora releases need an upgrade
  about every 6 months; Debian stable / Ubuntu LTS need far less attention.

## 2. Start the stack

```bash
git clone https://github.com/hybrid-system-design/maskinsjekk
cd maskinsjekk/deploy
docker compose up -d --build
```

Both services use `restart: unless-stopped`, so they return after a reboot or
power cut. Bind mounts carry `:z` so SELinux allows the containers to read them.

## 3. Stable address for the sensors

The ESP32 stores the broker as an IP address, so the server's address must
never change:

- Create a **DHCP reservation** for the server's MAC in the router.
- After moving the stack, point each sensor at the new address
  (`set mqtt_host <ip>` over serial, then `reboot`).

## 4. Operations

- Remote access without opening ports: Tailscale.
- Back up the `maskinsjekkdb` volume (SQLite) and `deploy/config/machines.json`:
  `docker run --rm -v maskinsjekk_maskinsjekkdb:/data -v "$PWD":/backup alpine tar czf /backup/db-$(date +%F).tgz -C /data .`
- Mosquitto allows anonymous access: keep the server on a trusted LAN until
  authentication/TLS is added (see ROADMAP "Later").
