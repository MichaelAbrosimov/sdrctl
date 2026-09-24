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
groupadd -f sdrctl                                        # control socket group
systemctl daemon-reload
systemctl enable --now sdrctl-agent
```

`rtl-433.service` / `spyserver.service` are optional; while absent they show
as `not-installed` and break nothing.

**Stop-on-unplug binding (required for hands-off replug recovery):** install
the udev rule and the `BindsTo` drop-in from `udev/99-sdrctl-rtlsdr.rules`
(single-device variant). Without it `rtl_tcp` survives an unplug with the
unit still `active`, sdrctl truthfully reports what systemd sees (`healthy`),
and the dead stream never resumes on its own — verified in the field.

## 4. No-sudo CLI for the operator user

Primary path (v0.2): the CLI talks to the agent over
`/run/sdrctl/sdrctl.sock`; membership in the `sdrctl` group is the whole
authorization:

```bash
usermod -aG sdrctl <user>                          # control socket; re-login
usermod -aG systemd-journal <user>                 # for sdrctl logs; re-login
```

Fallback path (agent down — `mode set` then drives systemctl directly and
needs polkit):

```bash
apt install -y polkitd
cp polkit/50-sdrctl.rules /etc/polkit-1/rules.d/   # group-based, no edits needed
systemctl restart polkit
```

The rule authorizes the same `sdrctl` group as the socket — one group is the
whole "may control this node" concept on both paths.

See "Root and privileges" and "Socket-first CLI" in docs/architecture.md for
what exactly the rule grants and why enable/disable cannot be scoped per unit.

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
curl --unix-socket /run/sdrctl/sdrctl.sock http://localhost/health
```

`sdrctl status` must show `Agent: running — /run/sdrctl/sdrctl.sock`. If it
says `unreachable`, the CLI still works but drives systemd directly
(check group membership and that sdrctl-agent is active).

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
- Remote control (for sdr-manager): set `api.write_enabled: true` and put a
  long random token into the agent-only secrets overlay:

  ```bash
  install -m 0600 -o root -g root configs/secrets.example.yaml /etc/sdrctl/secrets.yaml
  # edit api.token inside; keep config.yaml itself secret-free
  ```

  The agent refuses to start if a file supplying an ACTIVE secret (token
  with write API on, MQTT credentials with MQTT on) is readable beyond its
  owner — migrating an old token out of a 0640 config.yaml is mandatory,
  not cosmetic. Keep the node on a trusted LAN — the API has no TLS.

## 8. Adding a parameter profile (templated mode)

A mode's parameters live in the unit INSTANCE name, so a new frequency is an
env file plus a config line — no unit editing on the node. Deploy ships the
templates (`rtl-433@.service`, `rtl-tcp@.service`) but deliberately does not
create instances, profiles or the device binding: those are node decisions.

```sh
# 1. the profile itself
sudo mkdir -p /etc/sdrctl/profiles/rtl-433
sudo cp /path/to/repo/configs/profiles/rtl-433/868.env \
        /etc/sdrctl/profiles/rtl-433/868.env

# 2. REQUIRED once per template: bind every instance to the dongle, or a
#    replug will not recover (see udev/99-sdrctl-rtlsdr.rules for why).
#    A drop-in on the TEMPLATE applies to all of its instances.
sudo mkdir -p /etc/systemd/system/rtl-433@.service.d
sudo tee /etc/systemd/system/rtl-433@.service.d/bind.conf >/dev/null <<'CONF'
[Unit]
BindsTo=sys-subsystem-usb-devices-rtl\x2dsdr\x2d01.device
After=sys-subsystem-usb-devices-rtl\x2dsdr\x2d01.device
[Service]
TimeoutStopSec=5
CONF
sudo systemctl daemon-reload

# 3. make it a selectable mode
sudo vi /etc/sdrctl/config.yaml     # services: rtl-433@868: {systemd: rtl-433@868.service}
sudo systemctl restart sdrctl-agent

# 4. use it
sdrctl mode 868
```

Gotchas worth knowing before you hit them:

- **The family name stops working as a shorthand.** With `rtl-433@433` and
  `rtl-433@868` both configured, `sdrctl mode rtl-433` is refused as
  ambiguous (so is `433`, which is a substring of both). Name the instance,
  or use the part that is unique — `868`, `@433`.
- **Migrating off the plain unit.** `rtl-433.service` and `rtl-433@868` are
  separate units. Disable the old one before configuring the new, or a
  reboot brings back a mode you thought you had replaced:
  `sudo systemctl disable --now rtl-433.service`.
- **A second instance fails fast instead of queueing.** `ExecStart` runs
  under `flock -n` on a per-device lock, so hand-starting a second profile
  lands in `failed` rather than fighting for the dongle. That is the
  intended, visible outcome.

## Notes

- Never run `rtl_test` while an SDR service is active — one dongle, one owner
  (`sdrctl device` reminds about this).
- All runtime state lives in systemd/journald; sdrctl writes nothing to disk
  at runtime (eMMC-friendly, same principle as other appliances in the lab).
