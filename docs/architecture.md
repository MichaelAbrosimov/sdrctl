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
marked `optional: true` never break global health while their absence is
CONFIRMED; an unobservable device (no systemd/sysfs answer) is `unknown`
and breaks `ok` — "cannot observe" is never reported as "fine".

### Physical attribution (multi-device)

A physical dongle satisfies AT MOST one device configuration. Devices
sharing a VID/PID pair must therefore carry non-empty unique serials
(enforced by config validation; assign with `rtl_eeprom`). When attribution
is still unresolvable at runtime — one sysfs object matches two
configurations, or two factory-equal dongles match one — the affected
devices report `conflict` with an explaining warning instead of a guess:
presence built on a guess would quietly control the wrong dongle.

Known limitation: sdrctl matches dongles by USB serial, while `rtl_tcp`
selects them by librtlsdr index (`-d N` in the per-device env file). sdrctl
does not launch those processes and CANNOT guarantee the two identities
agree — after replugging or adding dongles, verify the mapping with
`sdrctl device <id>` against the actual port behaviour. Index drift is the
operator's to check; unique serials keep at least the control-plane side
honest.

## The observer (deliberately not a supervisor)

`sdrctl agent` polls state every `observer.interval_sec`, publishes changes
(MQTT) and serves snapshots (HTTP). It performs exactly one corrective action
— `auto_restore`: if a unit is enabled (desired), its dongle is present, but
the unit is not running, the agent re-runs the shared transition primitive
(`core.SetMode` with the fresh desired mode), at most once per device per
cooldown. Everything else — restarts, mutual exclusion, stop-on-unplug — is
systemd's job, declared in unit files (see `systemd/` and `udev/`).

Known limitation (observed on wyse-sdr, 2026-07-06): the state model trusts
systemd, and a process can lie to systemd. `rtl_tcp` without a connected
client SURVIVES a dongle unplug — the unit stays `active`, sdrctl reports
`healthy`, auto_restore correctly does nothing, and the stream is dead until
someone restarts the service. The udev `BindsTo` binding (see
`udev/99-sdrctl-rtlsdr.rules`) is therefore REQUIRED for hands-off replug
recovery, not an optional refinement: it makes systemd stop the unit on
unplug, which turns the lie into an honest `missing`/`degraded` that
auto_restore acts on. sdrctl deliberately does not probe the data plane.
Pair the binding with `TimeoutStopSec=5` in the same drop-in: the
dead-handle rtl_tcp ignores SIGTERM too, so the BindsTo stop would drain
for the default 90 s until SIGKILL — replug recovery waits exactly that
long (measured: ~1m50s end to end; with the short stop timeout it is
seconds). The observer skips restore attempts while the stop drains, so
those ticks cost nothing.

## Async write API

Mode changes over HTTP follow accepted-async: validation is synchronous
(400/404/409), execution is not — the handler returns `202 Accepted` and the
transition (potentially ~10 s) runs in background; one transition per device
at a time. The outcome arrives via `GET /status` and MQTT retained topics +
events. The CLI uses the same transition code but waits and prints the result.

## Modes, jobs and concurrency

**Status: mixed, marked per subsection.** Modes and their parameters are
implemented; **jobs are not** — no `sdrctl job`, no job units. The
concurrency rules below describe the intended behaviour once they exist,
except where a subsection says otherwise. Written down because "what happens
if I ask twice" came up twice and deserves an answer in the repo rather than
in a chat log.

### Two kinds of claim on a device

`rtlsdr_open()` is exclusive: one process owns a dongle at a time. Every
concept below exists to model that one physical fact honestly.

| | **Mode** | **Job** |
|---|---|---|
| Duration | indefinite | bounded, ends by itself |
| `enabled` (desired) | yes — this IS the mode | **never** |
| `active` | yes | yes, temporarily |
| Survives reboot | yes | no, and must not |
| On finish | nothing, keeps running | device returns to its mode |
| Examples | `rtl-tcp`, `rtl-433` | `survey`, `capture`, satellite pass |

A job is a systemd unit too (`capture@.service`), carrying the same
`Conflicts=` as the mode units. Exclusivity is then enforced by systemd, not
by the correctness of our code, and `RuntimeMaxSec=` bounds the job without
a watchdog of our own.

### Parameters belong to the mode's identity

**Status: implemented** (`systemd/rtl-433@.service`, `systemd/rtl-tcp@.service`).

A mode is not "rtl-433 plus a frequency stored somewhere": that somewhere
would be a second source of truth, and removing the first one (`state.json`)
is what made this design work. Parameters go into the unit instance name:

```text
rtl-433@433.service   enabled    <- the desired state includes the frequency
rtl-433@868.service   disabled
```

`%i` is an opaque **profile id** project-wide, and what it means is defined
by `/etc/sdrctl/profiles/<family>/<profile>.env`. Device selection is one of
those parameters (`DEVICE_ARGS=-d N`), deliberately not a second instance
axis — one meaning of `%i` keeps unit names and drop-ins predictable, and a
multi-dongle node just gives each dongle its own profile.

No Go code was needed: `core.SetMode` switches modes by disabling every other
service configured for the device, so an instance is simply another service
entry. Two consequences are worth knowing:

- **`Conflicts=` cannot cover siblings of one template.** It takes literal
  names and a template cannot enumerate its own instances. Cross-family
  exclusion still comes from `Conflicts=`; within a family, exclusion is
  sdrctl's transition (which is what actually switches modes) plus
  `flock -n` on a per-DEVICE lock in `ExecStart`, which stops a hand-run
  `systemctl start` of a second instance. `flock` fails fast rather than
  queueing: a second claimant belongs in `failed`, where sdrctl reports it,
  not waiting invisibly for a dongle that may never be freed.
- **A drop-in on the template covers every instance.** The `BindsTo` binding
  that makes replug recovery work is therefore installed once, at
  `/etc/systemd/system/rtl-433@.service.d/`, not per frequency — but it IS
  still required, and deploy does not install it (see the install doc).

The shorthand resolver treats instances as ordinary names, so with both
instances configured `sdrctl mode rtl-433` is **refused as ambiguous** and
names both candidates, while `sdrctl mode 868` resolves. A half-named mode
would hide which frequency the node actually went to.

### A job never restores anything

The obvious design is "remember the mode, run, put it back". It is fragile:
whoever remembers has to survive to the end. Instead:

```text
job stops    active     (systemctl stop)
job leaves   enabled    untouched
```

There is then nothing to restore. Desired state never changed, so the
existing `auto_restore` sees enabled-but-inactive and converges. Kill the
agent mid-job, lose power, drop the ssh session — the node still comes back
to its mode, because the intent lives in systemd rather than in a process's
memory.

### Concurrency

**Status: partly implemented** — today's refusal is real; last-write-wins is
not yet built, and jobs do not exist.

`BeginTransition` today refuses a second concurrent change
("mode change already in progress"). That stays the rule for jobs; for
modes it becomes last-write-wins, because a mode is a declared intent and
the newest declaration supersedes the older one. Writing `enabled` is
instant and idempotent, and a single serialized worker converges `active`
toward whatever `enabled` says when its iteration starts — so **`enabled`
is the queue**, depth one, with no new state to store.

| In progress | Requested | Behaviour |
|---|---|---|
| mode change | another mode | last wins; superseded intents are dropped |
| mode change | job | job waits out the transition (bounded by `mode_set_timeout_sec`) |
| job | mode change | `enabled` updates now, applies when the job ends; work is not cut short |
| job | another job | refused, naming the running job and its remaining time (`--wait` to queue) |

The third row needs no code of its own: a job does not touch `enabled`, a
mode change touches only `enabled`, and `auto_restore` closes the gap. Two
unrelated mechanisms compose because both rest on the `active`/`enabled`
split.

Open items before implementing:

- **Ping-pong.** Last-write-wins lets rapid commands thrash the dongle.
  systemd's `StartLimit*` and our `restore_cooldown_sec` already throttle;
  verify the converging worker does not fight them.
- **`--follow` during a supersede.** The follower must say "superseded by a
  newer request" instead of silently tailing someone else's transition.
- **Cancellation.** `sdrctl job cancel` must be a plain `systemctl stop`, so
  the return path is identical to a normal finish.

## MQTT boundary rule

```text
MQTT = telemetry, publish-only
HTTP = control (write API + bearer token)
```

sdrctl never subscribes. A second command channel via MQTT would mean a second
auth model and duplicated write logic; the integration boundary stays HTTP.

## Root and privileges

The agent runs as root (it drives systemctl). The CLI works without sudo for
any user in the `sdrctl` group: the socket file grants the primary path, and
`polkit/50-sdrctl.rules` authorizes the same group for the direct-systemctl
fallback (no usernames hardcoded anywhere). `systemd-journal` membership is
additionally needed for `sdrctl logs`; read commands need no privileges at
all once D-Bus is present.

Findings from the first deployment (systemd 257 / polkitd 126):

- `manage-units` (start/stop/restart/reset-failed) carries the unit name in
  polkit details → scoped strictly to the SDR units.
- `manage-unit-files` (enable/disable) carries **no details at all** (neither
  unit nor verb), so per-unit scoping is impossible; the rule grants it
  wholesale to the operator user. Acceptable on a single-operator appliance:
  creating unit files still requires root.
- A `sudoers` whitelist of `systemctl ... rtl-*` was rejected: sudoers
  wildcards match spaces, so such a pattern also permits extra arguments.

## Socket-first CLI (v0.2, implemented)

Since v0.2 the agent is the core and the CLI is its client, docker/tailscale
style. The agent listens on a unix socket (`/run/sdrctl/sdrctl.sock`,
`root:sdrctl` 0660 — file permissions instead of a token) where read AND
write endpoints are always available, independent of the network API
(`internal/api/socket.go`; `RuntimeDirectory=sdrctl` in the agent unit
provides `/run/sdrctl`).

- **Writes**: `sdrctl mode set` goes through the socket — the agent is the
  single executor of transitions, so CLI and API changes share one inflight
  guard and one synchronous code path (the socket returns the final result,
  unlike the async-202 network API). When the agent is unreachable the CLI
  falls back to driving systemctl directly with a warning — the node must
  stay controllable during recovery. An *error reply* from a running agent
  never triggers fallback: the agent is the authority.
- **Reads**: `status`, `devices`, `services`, `mode`, `device` prefer the
  agent's snapshot (identical to what the network API and MQTT publish) and
  fall back silently to deriving it locally — reads need no privileges.
- The polkit rule stays permanently as the fallback path's privilege
  mechanism; the operator additionally joins the `sdrctl` group for the
  socket. The network write API (token) remains the integration path for
  external callers such as sdr-manager.

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
