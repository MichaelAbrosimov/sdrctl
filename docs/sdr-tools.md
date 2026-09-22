# SDR tools on the node: what each one is for

Reference for deciding what deserves to be an sdrctl mode. Verified on
wyse-sdr (RTL-SDR Blog V4, Atom x5-Z8350).

Installed with the `rtl-sdr` package: `rtl_433` `rtl_tcp` `rtl_power`
`rtl_fm` `rtl_adsb` `rtl_sdr` `rtl_test` `rtl_eeprom` `rtl_biast`.
Available in apt, not installed: `readsb` (ADS-B), `satdump` (satellites),
`rtl-ais` (ships), `multimon-ng` (pagers).

The hardware constraint behind every decision: the tuner is one, the window
is ≤ 2.4 MHz, and whoever opens the dongle owns it exclusively. So the real
question is never "which modes are possible" but "what does the node do by
default" — everything else borrows the dongle temporarily.

## rtl_tcp — raw IQ server → `rtl-tcp.service`

| Flag | Effect |
|---|---|
| `-a <addr> -p <port>` | listen address (ours: `0.0.0.0:1234`) |
| `-f <Hz>` | start frequency; the CLIENT retunes afterwards |
| `-s <Hz>` | sample rate = window width = network traffic |
| `-g <dB>` | gain (0 = auto) |
| `-n <count>` | buffer size, against stutter on a slow network |

```sh
rtl_tcp -a 0.0.0.0 -p 1234 -s 2400000
```

Use: SDR++/GQRX/OpenWebRX from another machine. ~38 Mbit/s, one client at a
time. Perfect shape for a mode.

## rtl_433 — sensor decoder → `rtl-433.service`

| Flag | Effect |
|---|---|
| `-f <Hz>` | band; may be given SEVERAL times |
| `-H <sec>` | hop interval between those `-f` — 433 and 868 in one unit |
| `-R <n>` | only the listed protocols (silences false positives) |
| `-F kv\|json\|mqtt://…` | where and in what shape the data goes |
| `-M level\|stats` | attach RSSI/SNR and statistics to events |
| `-C si` | units (Fahrenheit otherwise) |
| `-s <Hz>` | receive bandwidth, 250 kHz by default |
| `-Y autolevel` | adaptive threshold, helps with weak signals |

```sh
rtl_433 -C si -f 433.92M -f 868.95M -H 60 -F mqtt://pi5.lab:1883
```

Use: sensors → MQTT around the clock. The multi-`-f` + `-H` form covers two
bands with one unit at the cost of hearing each half the time — bursts are
missed accordingly.

## rtl_power — occupancy meter → candidate for a "job", not a mode

| Flag | Effect |
|---|---|
| `-f low:high:bin` | range and resolution. **Finer bins = fewer hops = faster** |
| `-i <sec>` | averaging interval = one CSV row |
| `-e <time>` | when to stop (`10m`, `2h`) |
| `-g <dB>` | gain; keep fixed so runs stay comparable |
| `-c <%>` | crop window edges (hides the roll-off) |

```sh
rtl_power -f 24M:1700M:25k -g 30 -i 20 -e 10m out.csv
```

Measured on this node: sweep time ≈ hops × 53 ms (the per-hop floor is
retune + USB drain). 1 MHz bins → 1676 hops → 90 s per sweep; 25 kHz bins →
599 hops → 32 s per sweep with 40× the resolution. Storage is the catch:
a wide 25 kHz scan writes ~75 MB/hour, so a long survey must aggregate on
the fly instead of keeping raw CSV (eMMC).

## rtl_fm — demodulator → no mode

| Flag | Effect |
|---|---|
| `-M fm\|wbfm\|am\|usb\|lsb` | modulation: radios, airband (am), broadcast (wbfm) |
| `-f 118M:137M:25k` | SCAN a range with squelch |
| `-l <level>` | squelch — silent until a signal appears |
| `-E deemp,dc,direct` | corrections; `direct` reaches HF below 24 MHz |
| `-r <Hz>` | audio output rate |

```sh
rtl_fm -M wbfm -f 97.2M -s 200k -r 48k - | aplay -r 48k -f S16_LE
```

Use: listening by hand; feeding `multimon-ng` (POCSAG) or `direwolf`
(APRS). As a mode it makes little sense on a headless node — audio has
nowhere to go — unless paired with a decoder, which would be that decoder's
mode instead.

## rtl_adsb — simple ADS-B decoder (installed)

| Flag | Effect |
|---|---|
| `-V` | verbose decoded frames |
| `-Q <0..2>` | timing strictness (NOT a CRC check) |
| `-e <n>` | tolerated errors per frame |
| `-S` | show short frames |

**It does not validate frames at all** — see the integrity section below.
Alone it cannot answer "are there aircraft?"; pipe it through a CRC filter:

```sh
rtl_adsb -g 49 | python3 ~/adsb-crc.py     # only CRC-valid frames survive
```

Too bare for permanent duty (no network feed, no map): that is `readsb`'s
job, and readsb does check CRC itself.

## Trust the integrity field, not the output

Field lesson (2026-07-30). `rtl_adsb` on 1090 MHz produced a steady stream
of plausible-looking frames on a node whose airspace is closed to civil
aviation. All of it was noise:

- **0 of 64 frames passed the Mode S CRC.** Real frames always check out.
- **The downlink formats were uniformly spread over DF16–DF31.** Real
  traffic is ~95 % DF17 plus DF11/4/5/20; DF22–31 do not occur.
- **64 frames, 64 distinct ICAO addresses.** One real aircraft sends
  several messages per second under the SAME address.

The mechanism: rtl_adsb sees a burst, reads the first bit, and if it is 1
declares a 112-bit "long frame" and prints it verbatim. With automatic gain
on 1090 MHz, noise supplies such bursts constantly.

So the rule when judging any decoder: **check whether it verifies
integrity, and only then believe its output.**

- `rtl_433` prints `Integrity: CRC | CHECKSUM | PARITY` per decode — that
  is why we trust it, and why PARITY-only protocols still leak noise (the
  "Govee-Water" ghost in docs/rtl-433-mode.md).
- `rtl_adsb` prints nothing of the sort because it verifies nothing.
- `rtl_power` makes no claims at all: it reports loudness, and loudness is
  always "true".

A validated CRC checker lives on the node at `~/adsb-crc.py` (its own
correctness is verified against the canonical `8D4840D6…` reference frame
and a deliberately corrupted copy — validate the tool before trusting the
verdict).

## A negative result is a claim about the instrument first

Field lesson (2026-09-21, the 868 MHz survey). Thirty minutes on 868.95
produced zero events, and the number was worthless four times over before
it meant anything:

1. **Wrong bandwidth.** `-s 1024k` while decoder 104 (wM-Bus Mode C&T)
   documents `-s 1200k` for its 100 kbps signalling.
2. **Wrong centre.** Modes S and T live on 868.3, not 868.95 — they were
   outside the captured band entirely.
3. **Wrong antenna.** A 144/433 dipole at 868 is badly mismatched; λ/4 here
   is 8.2 cm.
4. **Device contention.** A control run launched while the survey still
   held the dongle reported zero because it never got the device — the
   number measured a collision, not the air.

Each was invisible in the output: rtl_433 exits 0 and writes an empty file
in all four cases, exactly as it does for a genuinely quiet band.

**So every zero needs a positive control — a signal known to be present,
measured through the same chain.** Rules that follow:

- **Validate what actually changed.** After the antenna was re-cut for 868,
  the antenna is the suspect; re-running the survey proves nothing.
- **The control must not depend on a source that can be legitimately
  absent.** TPMS on 433.92 was the daytime baseline (2 events / 5 min), but
  a 15-minute repeat at 01:00 caught nothing — TPMS transmits only while
  wheels turn. The validator failed for reasons unrelated to the rig.
- **Prefer a control that is always on air.** Broadcast FM works at any
  hour: `rtl_power -f 88M:108M:100k -i 10 -1` and look for *discrete peaks
  at station frequencies* (here ~33 dB over a −32 dB floor). A flat rise is
  not proof; stray FM couples into a dongle with no antenna at all.
- **Know each control's reach.** FM clears the tuner, USB and sample path.
  It does NOT prove the antenna is connected — only a band where the
  antenna is matched can do that.

Verdict recorded with its own caveat: 868 measured clean on the second
attempt (C&T at 1200k, S+T at 868.3, matched 8.2 cm dipole) and both passes
were empty — but the antenna connection is still unconfirmed, so the result
is pending a daytime 433 control.

## Service tools — not modes

| Command | Purpose | When |
|---|---|---|
| `rtl_test -p` | measure crystal error (ppm) | once; then pass `-p` to every mode |
| `rtl_test -s 2.4M` | sample-loss test | when USB or power is suspect |
| `rtl_eeprom -s 00000002` | write a unique serial | REQUIRED before a second dongle |
| `rtl_biast -b 1` | bias-T power for an LNA | with an active antenna → `ExecStartPre` in the unit |
| `rtl_sdr -f 433.92M -n 10M f.iq` | record raw IQ | offline analysis (`rtl_433 -r f.iq`) |

## Verdict: what should take which shape

| Candidate | Shape | Why |
|---|---|---|
| rtl-tcp | mode (exists) | long-running service |
| rtl-433 on 433 MHz | mode (exists) | proven value |
| rtl-433 + 868 via `-H` | PROFILE of the same mode | one env line, no code; costs missed packets |
| 868 as its own mode | **not justified** | two clean 15-min passes (C&T, S+T) caught nothing; pending the 433 control above |
| survey (rtl_power) | **job** | it ends; returns via `Conflicts=` + auto_restore |
| IQ recording (rtl_sdr) | job | same shape, different tool |
| ADS-B (readsb) | mode — but test with `rtl_adsb` first | pointless if no aircraft are receivable |
| satdump | **scheduled job** | passes are minutes long, a few times a day |
| rtl_fm / multimon-ng | skip | legally murky, little value here |
| rtl_test / eeprom / biast | by hand | one-off operations |

The architecturally valuable next step is `survey` as a job: it introduces
the missing notion of temporarily borrowing the dongle, which satdump and
IQ recording will need too. See the settling design note in CODE_REVIEW.md
for how the return to the previous mode works without touching `SetMode`.
