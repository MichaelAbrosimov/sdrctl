# Installing sdrctl on wyse-sdr-01

Target: Dell Wyse 3040 (x86_64, 2 GB RAM, 8 GB eMMC), DietPi/Debian,
RTL-SDR Blog V4. The node is an appliance: one binary, no Docker, no web UI,
minimal disk writes.

## 1. Prepare the OS

```bash
apt update && apt install -y rtl-sdr        # rtl_tcp, rtl_test, rtl_eeprom
# Minimal DietPi images lack D-Bus; without it systemctl queries (and thus
# sdrctl status) fail for non-root users:
apt install -y dbus && systemctl enable --now dbus.socket
# Kernel DVB drivers must not grab the dongle:
cat >/etc/modprobe.d/blacklist-rtlsdr.conf <<'EOF'
blacklist dvb_usb_rtl28xxu
blacklist rtl2832
blacklist rtl2830
EOF
# reboot or rmmod dvb_usb_rtl28xxu
```

Verify the dongle: `lsusb | grep -i 0bda` and (only while no SDR service is
active!) `rtl_test -t`.

## 2. Deploy the binary

On the dev machine:

```bash
make build-linux
scp bin/sdrctl-linux-amd64 root@wyse-sdr-01:/usr/local/bin/sdrctl
ssh root@wyse-sdr-01 chmod +x /usr/local/bin/sdrctl
```

## 3. Config and units

```bash
mkdir -p /etc/sdrctl
cp configs/config.example.yaml /etc/sdrctl/config.yaml    # edit node id etc.
cp systemd/rtl-tcp.service /etc/systemd/system/           # single-device form
cp systemd/sdrctl-agent.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now sdrctl-agent
```

`rtl-433.service` / `spyserver.service` are optional; while absent they show
as `not-installed` and break nothing.

## 4. No-sudo CLI for the operator user (optional)

To let the operator user run every sdrctl command (including `mode set`)
without sudo:

```bash
apt install -y polkitd
cp polkit/50-sdrctl.rules /etc/polkit-1/rules.d/   # adjust the user name inside
systemctl restart polkit
usermod -aG systemd-journal <user>                 # for sdrctl logs; re-login
```

See "Root and privileges" in docs/architecture.md for what exactly the rule
grants and why enable/disable cannot be scoped per unit.

## 5. Acceptance check

```bash
sdrctl status
sdrctl mode
sdrctl mode set rtl-tcp     # rtl-tcp.service becomes active+enabled
sdrctl mode set idle
sdrctl services
sdrctl logs
sdrctl device

curl http://127.0.0.1:8081/health
curl http://127.0.0.1:8081/status
curl http://127.0.0.1:8081/mode
```

Reboot test: set a mode, reboot — the mode must come back by itself (desired
state = enabled units; sdrctl needs no init/restore step).

## 6. Multiple dongles

**Mandatory first step:** RTL-SDR Blog V4 dongles all ship with the same USB
serial `00000001`. With two identical serials neither sdrctl nor rtl_tcp can
tell the devices apart (`sdrctl status` will warn). Assign unique serials one
dongle at a time (only one plugged in, no SDR service active):

```bash
sdrctl mode set idle
rtl_eeprom -s 00000002        # then re-plug the dongle
```

Then:

1. Switch `/etc/sdrctl/config.yaml` to the `devices:` form (see example),
   one entry per dongle with its serial; pick a `default: true` device.
2. `cp systemd/rtl-tcp@.service /etc/systemd/system/`
3. Per device: `mkdir -p /etc/sdrctl/devices` and create
   `/etc/sdrctl/devices/<id>.env` from `configs/device.env.example`
   (unique PORT per device, DEVICE_ARGS selecting the right dongle).
4. `systemctl daemon-reload`, then `sdrctl device <id> mode set rtl-tcp`.
5. Optional stop-on-unplug binding: see `udev/99-sdrctl-rtlsdr.rules`.

## 7. Optional: MQTT and write API

- MQTT telemetry: set `mqtt.enabled: true` and `mqtt.broker`, restart the
  agent, subscribe to `sdr/wyse-sdr-01/#`.
- Remote control (for sdr-manager): set `api.write_enabled: true` **and** a
  long random `api.token`; keep the node on a trusted LAN — the API has no
  TLS in v0.1.

## Notes

- Never run `rtl_test` while an SDR service is active — one dongle, one owner
  (`sdrctl device` reminds about this).
- All runtime state lives in systemd/journald; sdrctl writes nothing to disk
  at runtime (eMMC-friendly, same principle as other appliances in the lab).
