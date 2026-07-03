# sdrctl architecture

```text
sdrctl      = local SDR node control plane
sdr-manager = external future orchestration/manager
systemd     = process owner
HTTP API    = integration boundary
```

`sdrctl` — самостоятельный локальный control plane для одного SDR-узла.
`sdr-manager` потом будет внешним orchestrator-ом, который общается с ним
через API.

## Components

One binary, several roles:

```text
sdrctl <command>   CLI: status/mode/devices/services/logs
sdrctl agent       daemon: HTTP API + state observer + MQTT publisher
                   (interactive terminal console reserved for later,
                    Bubble Tea / Lip Gloss; no web UI ever on the node)
```

Internal packages: `config` (static facts only), `systemd` (systemctl/journalctl
wrapper), `device` (sysfs USB detection), `core` (state model, mode switching),
`agent` (observer loop), `api` (HTTP), `mqtt` (telemetry), `cli` (cobra).

## State model — systemd owns everything

| Question | Source of truth | How |
|---|---|---|
| actual mode | systemd | which SDR unit is `active` |
| desired mode | systemd | which SDR unit is `enabled` |
| one dongle = one mode | systemd | `Conflicts=` between units of one device |
| crash recovery | systemd | `Restart=on-failure`, `RestartSec`, `StartLimit*` |
| restart counters, timestamps | systemd | `NRestarts`, `ActiveEnterTimestamp` |
| last error | journald | `journalctl -u <unit>` |
| physical presence | sysfs | `/sys/bus/usb/devices` (vendor/product/serial) |

**There is no state.json.** An early draft had one (desired mode, restart
counters, timestamps) plus an active reconcile loop. It was removed once every
field turned out to already exist in systemd. This eliminated the second
writer (CLI vs supervisor races), survives reboots for free (`enabled` units
start on boot) and matches the appliance principle: no runtime writes to
eMMC/disk.

`sdrctl mode set X` is therefore just:

```text
systemctl disable --now <competing units>   # clear desired + stop
systemctl enable  --now <unit of X>         # set desired + start
verify ActiveState
```

`idle` = disable everything. Idempotent: requesting the current mode is a no-op.

## Mode and health model

Mode is per **device**, not per host. Different dongles may run different
modes simultaneously; one dongle runs at most one.

Mode of a device: `idle` (nothing active), `<service>` (exactly one active),
`conflict` (more than one active), `unknown` (cannot query systemd).

Health per device:

```text
healthy   present, desired mode active
idle      present, nothing desired, nothing active
missing   configured device not found on USB
degraded  desired mode enabled but its unit is not active
conflict  competing units active/enabled for one device
unknown   cannot determine (no systemd/sysfs on this host)
```

Global: `ok = true` iff all required devices are healthy or idle. Devices
marked `optional: true` never break global health while absent.

## The observer (deliberately not a supervisor)

`sdrctl agent` polls state every `observer.interval_sec`, publishes changes
(MQTT) and serves snapshots (HTTP). It performs exactly one corrective action
— `auto_restore`: if a unit is enabled (desired), its dongle is present, but
the unit is inactive/failed (typically after re-plug exhausted StartLimit),
the agent does `reset-failed` + `restart`, at most once per device per 30 s.
Everything else — restarts, mutual exclusion, stop-on-unplug — is systemd's
job, declared in unit files (see `systemd/` and `udev/`).

## Async write API

Mode changes over HTTP follow accepted-async: validation is synchronous
(400/404/409), execution is not — the handler returns `202 Accepted` and the
transition (potentially ~10 s) runs in background; one transition per device
at a time. The outcome arrives via `GET /status` and MQTT retained topics +
events. The CLI uses the same transition code but waits and prints the result.

## MQTT boundary rule

```text
MQTT = telemetry, publish-only
HTTP = control (write API + bearer token)
```

sdrctl never subscribes. A second command channel via MQTT would mean a second
auth model and duplicated write logic; the integration boundary stays HTTP.

## Root and privileges

The agent runs as root (it drives systemctl). The CLI works without sudo for
the operator user via `polkit/50-sdrctl.rules` + membership in the
`systemd-journal` group (for `sdrctl logs`); read commands need no privileges
at all once D-Bus is present.

Findings from the first deployment (systemd 257 / polkitd 126):

- `manage-units` (start/stop/restart/reset-failed) carries the unit name in
  polkit details → scoped strictly to the SDR units.
- `manage-unit-files` (enable/disable) carries **no details at all** (neither
  unit nor verb), so per-unit scoping is impossible; the rule grants it
  wholesale to the operator user. Acceptable on a single-operator appliance:
  creating unit files still requires root.
- A `sudoers` whitelist of `systemctl ... rtl-*` was rejected: sudoers
  wildcards match spaces, so such a pattern also permits extra arguments.

This is the permanent privilege model (decided 2026-07-03), not an interim
step: the CLI always drives systemd directly, so the node stays controllable
even when the agent is down, and there is no writer conflict by construction —
CLI and agent both converge on the same desired state stored in systemd. The
HTTP write API remains the integration path for external callers
(sdr-manager), not a replacement for the local CLI.

## Multi-client rtl_tcp (future, separate project)

rtl_tcp accepts a single client. If several clients must share one dongle via
the rtl_tcp protocol, that is a **data-plane** proxy and lives outside sdrctl
(own repo, own unit), chained through systemd:

```text
clients ──> rtl-tcp-proxy@<id>.service ──> rtl-tcp@<id>.service ──> dongle
            (public port, BindsTo=rtl-tcp@<id>)   (listens on 127.0.0.1)
```

For sdrctl it is just one more unit in the config — no code changes. Before
building a proxy, evaluate SpyServer mode: it supports multiple clients
natively and is already a planned mode. Control commands in such a proxy
follow "last writer wins".

## Relationship to pi5-lab / sdr-manager

`sdr-manager` (currently in the pi5-lab repository) is the future orchestrator
and web panel. It integrates with nodes exclusively via this HTTP API and MQTT
telemetry. Long term, the same `sdrctl` binary can also run on the Pi itself,
taking over local mode switching, while sdr-manager keeps orchestration, UI
and container management.
