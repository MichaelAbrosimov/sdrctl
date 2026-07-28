# The rtl-433 mode

Second mode of the node: instead of streaming raw IQ like `rtl-tcp`, the
dongle decodes ISM-band (433.92 MHz) sensor packets and emits structured
events. One dongle, one mode — switching stays `sdrctl mode set rtl-433` /
`sdrctl mode set rtl-tcp`.

Unit: `systemd/rtl-433.service` (package `rtl-433` from apt), with the same
`Conflicts=` semantics and the same `BindsTo` dongle binding as every SDR
unit.

## Configuration

Arguments come from an optional env file, so the unit is never edited:

```sh
# /etc/sdrctl/rtl-433.env
RTL433_ARGS=-C si -F kv -F mqtt://pi5.lab:1883
```

- `-C si` — Celsius (default output is Fahrenheit).
- `-F kv` — keep the human-readable journal output. **Any explicit `-F`
  disables the default output**, so list `kv` alongside `mqtt`.
- `-F mqtt://<host>:<port>` — publish to the broker.
- `-R -<protocol>` — disable a protocol; the practical use is silencing
  false positives (below).

## What the data looks like

Cheap ISM sensors are one-way: no acknowledgements, no encryption (except
rolling-code remotes). Reliability comes from repetition — a sensor sends
the same packet 3–6 times in a row, so **duplicate decodes within the same
second are normal**, not a bug.

Every decode carries an `Integrity` field, and it is the trust level of the
reading:

| Value | Meaning |
|---|---|
| `CRC` | strong checksum — false positives are practically impossible |
| `CHECKSUM` | weaker, occasionally wrong |
| `PARITY` | weakest; **noise can pass it** |

Field example: a "Govee-Water" leak sensor decoded ~10 times in 25 minutes
with `Raw Code fffffffffffe` (all bits set) and `event: Unknown` — that is
noise passing a parity check, not a device. Silence such protocols with
`-R -<number>` rather than trying to interpret them.

Expect the majority of traffic to be one nearby sensor and a long tail of
neighbourhood devices: TPMS from passing cars, garage-door remotes, power
monitors, weather stations.

## MQTT structure

Each event is published three ways:

```text
rtl_433/<node>/events                                  full packet as JSON
rtl_433/<node>/devices/<model>/<channel>/<id>/<field>  one value per topic
rtl_433/<node>/states                                  summary
```

`devices/…` suits Home Assistant and graphing (one value = one sensor);
`events` suits logging and debugging (full context in one JSON).

**No retain by default.** The broker stores nothing: a subscriber sees only
what arrives after it connects — an MQTT client shows an empty tree until
the next transmission. Enabling `retain=1` fixes that for your own sensors
but is a trap with TPMS: every passing car creates a permanent topic.
Enable it selectively, together with a protocol filter.

Wipe accumulated retained topics if it ever comes to that:

```sh
mosquitto_sub -h <broker> -t 'rtl_433/#' --retained-only -W 5 -F '%t' \
  | while read t; do mosquitto_pub -h <broker> -t "$t" -r -n; done
```

## Broker notes (pi5.lab, 2026-07-28)

`eclipse-mosquitto:2.0.22` in Docker, anonymous, no TLS — fine on a trusted
LAN. Nothing accumulates: `mosquitto.db` stays a few KB because no messages
are retained, and `log_type` excludes `publish`/`subscribe`, so the broker
log does not grow with traffic. No maintenance needed as long as retain
stays off.

This is a separate channel from sdrctl's own telemetry (`sdr/<node>/#`,
`mqtt.enabled` in the agent config) — the prefixes do not collide, and both
can share one broker.
