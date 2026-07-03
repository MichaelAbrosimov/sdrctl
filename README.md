# sdrctl

`sdrctl` is the local control plane of a headless SDR radio node: a CLI and a
small agent that manage which SDR service owns the dongle(s), expose node
state over a read-only HTTP API and publish state changes to MQTT.

Primary target: `wyse-sdr-01` — Dell Wyse 3040, DietPi/Debian, RTL-SDR Blog V4.
The node is a quiet radio appliance: one static binary, no Docker, no web UI.

## sdrctl vs sdr-manager

```text
sdrctl      = local SDR node control plane (this repository, runs on the node)
sdr-manager = external future orchestrator (separate project, e.g. pi5-lab)
systemd     = process owner
HTTP API    = integration boundary
```

`sdr-manager` never imports this code and never runs on this node — it talks
to `sdrctl agent` over HTTP and listens to MQTT telemetry. One `sdrctl` binary
per node; the same binary can later serve other hosts (including a Pi).

## Design in one paragraph

systemd owns everything. A device's **actual mode** is which SDR unit is
`active`; its **desired mode** is which unit is `enabled`; mutual exclusion is
`Conflicts=` in the units; crash recovery is `Restart=`/`StartLimit*`. `sdrctl`
only calls `systemctl`/`journalctl`, derives state live and keeps **no state
files**. Details and the reasoning: [docs/architecture.md](docs/architecture.md).

## Install (short version)

Full walkthrough: [docs/install-wyse-sdr-01.md](docs/install-wyse-sdr-01.md).

```bash
# on the dev machine
make build-linux
scp bin/sdrctl-linux-amd64 root@wyse-sdr-01:/usr/local/bin/sdrctl

# on the node
mkdir -p /etc/sdrctl
cp configs/config.example.yaml /etc/sdrctl/config.yaml   # and edit
cp systemd/rtl-tcp.service /etc/systemd/system/
cp systemd/sdrctl-agent.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now sdrctl-agent
```

## CLI

```bash
sdrctl status                          # node + devices overview
sdrctl mode                            # current mode of the default device
sdrctl mode set rtl-tcp                # switch mode (systemctl enable --now)
sdrctl mode set idle                   # stop everything (disable --now)
sdrctl devices                         # list configured devices
sdrctl device rtl-sdr-01               # device detail incl. USB/lsusb
sdrctl device rtl-sdr-01 mode set idle # per-device control
sdrctl services                        # known SDR units and their state
sdrctl logs                            # journal of the active SDR service
sdrctl agent                           # run the agent (normally via systemd)
sdrctl version
```

With several dongles, global commands require a `default: true` device in the
config; otherwise they ask you to use `sdrctl device <id> ...`.

## HTTP API

Read-only endpoints are always on (when `api.enabled`); write endpoints are
off by default and require both `api.write_enabled: true` and a bearer token.
Mode changes are asynchronous: `202 Accepted` now, result via `GET /status`
and MQTT. Full reference: [docs/api.md](docs/api.md).

```bash
curl http://127.0.0.1:8081/health
curl http://127.0.0.1:8081/status
curl http://127.0.0.1:8081/mode
```

## MQTT

Publish-only telemetry (commands go through HTTP only): retained state topics
under `sdr/<node>/...` plus a Last Will `availability` topic, so the broker
itself announces node death. Disabled by default; see `mqtt:` in
[configs/config.example.yaml](configs/config.example.yaml) and
[docs/api.md](docs/api.md).

## Development

```bash
make build   # host binary in bin/
make test
make vet
```

Repository layout: `cmd/sdrctl` (entry point), `internal/*` (config, systemd,
device, core model, agent, api, mqtt, cli), `configs/`, `systemd/`, `udev/`
(deploy-time artifacts), `docs/`.
