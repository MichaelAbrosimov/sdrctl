# sdrctl v0.1 — consolidated specification

This is the agreed specification the initial implementation follows. It merges
the two original prompts (base project + multi-device/watchdog extension) with
the design decisions made during review. Where this document deviates from the
original prompts, the deviation is explicit and reasoned — this text wins.

## Purpose

Local control agent + CLI for headless SDR radio nodes. First node:
`wyse-sdr-01` (Dell Wyse 3040, DietPi/Debian, RTL-SDR Blog V4), role: quiet
SDR backend / radio appliance.

`sdrctl` is standalone. The future `sdr-manager` (external orchestrator,
currently prototyped in the pi5-lab repository) interacts with it **only**
through the HTTP API and MQTT telemetry — never as a code dependency.

## Stack

Go (single static binary, cross-compiled from macOS, no Docker, no web UI, no
heavy runtime deps). Cobra CLI, net/http, YAML config, Eclipse Paho MQTT.
Bubble Tea / Lip Gloss interactive console is architecturally reserved, not in
v0.1. Rationale vs C++/Rust/Python: deployment by scp of one binary, stdlib
HTTP/JSON, CLI/TUI ecosystem; the program is I/O-bound glue, no data plane.

## Core rules

1. systemd is the ONLY owner of SDR processes; sdrctl never spawns
   `rtl_tcp`/`rtl_433`/`spyserver` — only `systemctl` / `journalctl`.
2. Desired mode = systemd `enabled`; actual mode = systemd `active`;
   presence = USB sysfs. **No state.json** (deviation from original prompt:
   every field it would hold already exists in systemd/journald; removing it
   eliminates dual-writer races and runtime disk writes).
3. One device = one active SDR mode, enforced by `Conflicts=` in units.
4. Crash recovery = `Restart=`/`StartLimit*` in units (deviation: the original
   ModeSupervisor restart/backoff config duplicated systemd features).
5. The agent's observer is read-only except `auto_restore`: reset-failed +
   restart of a desired-but-stopped unit when the dongle is present (covers
   dongle-return after StartLimit exhaustion; ≤1 attempt / device / 30 s).
6. Stop-on-unplug: optional udev tag + `BindsTo=` drop-in (documented in
   `udev/`); deliberately NOT `ENV{SYSTEMD_WANTS}`, which would start services
   whose desired mode is idle.
7. Multi-device model from day one; single-device config is shorthand,
   normalized to one implicit default device. Devices have logical ids
   (`rtl-sdr-01`, ...), optional flag, per-device services; template units
   `rtl-tcp@<device-id>.service` for multi-dongle hosts.
8. Missing optional services/devices are reported (`not-installed`/`missing`)
   but are never errors and never break global health.
9. Duplicate USB serials produce a warning suggesting `rtl_eeprom`
   (all RTL-SDR Blog V4 ship as 00000001).
10. No web UI on the node, ever. Terminal console only (future).

## CLI v0.1

`status`, `mode`, `mode set <m>`, `devices`, `device [id] [status|logs|mode
[set <m>]]`, `services`, `logs [id]`, `agent`, `version`, `help`; global
`--config` (default `/etc/sdrctl/config.yaml`). Global commands act on the
default device; with several devices and no default they fail with the
"specify device id" message. `mode set` is synchronous for the human: it
waits for verification and prints the outcome.

## HTTP API v0.1

Read (always on with `api.enabled`): `/health`, `/status`, `/mode`,
`/devices`, `/devices/{id}`, `/devices/{id}/mode`, `/devices/{id}/health`.
`/health` returns transport-200 with `ok` + per-device health in the body;
`ok=true` iff all required devices healthy/idle.

Write (default OFF; requires `api.write_enabled` + Bearer `api.token`):
`POST /mode/{mode}`, `POST /devices/{id}/mode/{mode}`. Accepted-async
contract: synchronous validation (400 unknown mode / 404 unknown device /
409 transition in progress / 200 no-op idempotent), then `202 Accepted`;
result via `/status` + MQTT. One in-flight transition per device (in-memory
only; after agent restart systemd remains the truth).

## MQTT v0.1

Publish-only (rule: MQTT = telemetry out, HTTP = control in). Default OFF.
Retained: `<prefix>/availability` (LWT online/offline), `/status`, `/health`,
`/devices/<id>/mode`, `/devices/<id>/health`. Non-retained: `/events`
(mode_changed / health_changed). Publish on change only (+ optional
heartbeat). Broker outage never affects the node. Future (not v0.1): Home
Assistant MQTT discovery, host metrics.

## Health model

Per device: `healthy | idle | missing | degraded | conflict | unknown`
(see docs/architecture.md for exact derivation). Global `ok` per rule above.

## Config (`/etc/sdrctl/config.yaml`)

See `configs/config.example.yaml` — node id/role; api (enabled, listen,
write_enabled, token); mqtt (broker, prefix, qos, retain, heartbeat);
observer (interval_sec, auto_restore); services (single-device shorthand) or
devices[] (id, type, label, serial, usb ids, default, optional, services with
systemd unit + port + optional). Deviation from original prompt: no
`modes.stop/start` lists (derived from the service registry + `Conflicts=`)
and no `supervisor.restart_*` knobs (systemd `Restart=`/`StartLimit*`).

## Out of scope for v0.1 (recorded decisions)

- Interactive TUI console (architecture allows adding a `console` command).
- Write API via MQTT — rejected permanently (single control channel: HTTP).
- rtl_tcp multi-client proxy — separate future repository; for sdrctl it is
  just another unit chained via `BindsTo=` (rtl_tcp moves to 127.0.0.1, proxy
  exposes the public port). Evaluate SpyServer's native multi-client support
  first.
- polkit/sudoers hardening instead of root agent.

## Acceptance criteria

On `wyse-sdr-01`: all CLI commands above work; `rtl-tcp.service` is switched
via systemd only; the V4 dongle is detected via sysfs; `/health`, `/status`,
`/mode` return JSON; absent `rtl-433`/`spyserver` units show `not-installed`
without breaking anything; a set mode survives reboot without any sdrctl
"init" step; no web UI, no Docker, no runtime files on disk. Multi-device:
`sdrctl devices` lists all; per-device mode set works; two dongles can run
different modes; a crashed service is restarted by systemd; an unplugged
dongle degrades only its device (optional → global ok stays true); after
re-plug `auto_restore` brings the desired mode back.
