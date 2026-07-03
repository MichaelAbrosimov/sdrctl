# sdrctl HTTP API and MQTT topics

Default listen address: `0.0.0.0:8081` (`api.listen` in config).
All responses are JSON. Read endpoints need no auth.

## Read endpoints

### GET /health

```json
{
  "ok": true,
  "node": "wyse-sdr-01",
  "service": "sdrctl-agent",
  "health": "healthy",
  "devices": { "rtl-sdr-01": "healthy", "rtl-sdr-02": "missing" }
}
```

`ok` is true only if all required devices are `healthy` or `idle`; optional
missing devices do not affect it. HTTP status is always 200 while the agent is
alive — liveness is the transport, node health is the body.

### GET /status

Full snapshot: node, version, `generated_at`, `lan_ip`, `ok`, `health`,
`warnings`, and a `devices` array — per device: `id`, `type`, `label`,
`serial`, `present`, `presence_known`, `mode`, `desired_mode`, `health`,
`services` (name → status), `service_details` (unit, enabled, port,
restarts, since), `ports`, `usb` (matched sysfs entry).

Snapshots are cached by the observer (`observer.interval_sec`, default 5 s);
`generated_at` tells you the age.

### GET /mode

Mode of the default device:

```json
{ "device": "rtl-sdr-01", "mode": "rtl-tcp", "desired_mode": "rtl-tcp" }
```

`409` when several devices are configured and none is default.

### GET /devices — all devices (same shape as in /status)
### GET /devices/{id} — one device; `404` for unknown id
### GET /devices/{id}/mode — `{ "device", "mode", "desired_mode" }`
### GET /devices/{id}/health — `{ "device", "health", "present" }`

## Write endpoints (off by default)

```http
POST /mode/{mode}                    # default device
POST /devices/{id}/mode/{mode}       # specific device
Authorization: Bearer <api.token>
```

Preconditions: `api.write_enabled: true` **and** non-empty `api.token`
(refused with 403 otherwise; bad token → 401).

Mode changes are **asynchronous**. A transition can take ~10 s, so the handler
validates synchronously and executes in background:

| Response | Meaning |
|---|---|
| `202 {"accepted":true,"device":"rtl-sdr-01","requested_mode":"rtl-tcp"}` | valid, transition started |
| `200 {"mode":"rtl-tcp","changed":false,...}` | already in that mode, no-op (idempotent) |
| `400` | unknown mode for this device |
| `404` | unknown device |
| `409` | transition already in progress for this device |
| `401/403` | auth / write API disabled |

The **result** of an accepted transition is observable via `GET /status` /
`GET /devices/{id}/mode` and arrives as MQTT events; failures are logged to
journald and surface as device health `degraded`.

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  http://wyse-sdr-01:8081/devices/rtl-sdr-01/mode/rtl-tcp
```

## MQTT topics

Publish-only telemetry (`mqtt.enabled: true`); prefix defaults to
`sdr/<node id>`. QoS/retain from config (defaults 1/true).

| Topic | Payload | Retained |
|---|---|---|
| `<prefix>/availability` | `online` / `offline` (Last Will) | yes |
| `<prefix>/status` | full snapshot JSON (as GET /status) | yes |
| `<prefix>/health` | global health string | yes |
| `<prefix>/devices/<id>/mode` | mode string | yes |
| `<prefix>/devices/<id>/health` | health string | yes |
| `<prefix>/events` | change events | no |

Events:

```json
{ "ts": "2026-07-03T10:00:00Z", "device": "rtl-sdr-01",
  "type": "mode_changed", "from": "idle", "to": "rtl-tcp" }
{ "ts": "...", "device": "...", "type": "health_changed", "from": "healthy", "to": "degraded" }
```

Publishing happens on state change (plus optional `heartbeat_sec`). On
(re)connect all retained topics are refreshed, so subscribers recover full
state after broker downtime. A dead broker never affects the node: the client
retries in background, the core keeps working.
