# IQ analysis tools

Offline analysis of raw captures, for signals `rtl_433` does not decode.
Recording first and analysing afterwards beats live listening: the file can
be re-examined with any tool any number of times, and it does not depend on
a device transmitting in the minute you happen to be listening. Two 15-min
live passes on 868 MHz found nothing; a 120 s recording of the same band
held 48 bursts.

Nothing here needs the dongle, so the node can stay in its normal mode.
Only numpy is required (the node has no numpy; pi5-lab does).

## Workflow

```sh
# 1. capture (needs the dongle: put the node in idle first)
rtl_sdr -f 868.3M -s 1024000 -n 122880000 - > 868.iq

# 2. what is in there?
python3 iqscan.py 868.iq 1024000

# 3. isolate one channel: shift it to DC, filter, decimate
#    (rtl_433's FSK demodulator otherwise takes one peak from each of two
#     channels as mark and space)
python3 chan.py 868.iq A256.iq 197000 4

# 4. read the bits of one burst
python3 fsk.py A256.iq 256000 <t_start> <duration> [force_symbol_samples]
```

## Validate before believing

`mksynth.py` writes a capture whose contents are known exactly: a fast
chirp, a slow chirp at ~7 dB SNR, a narrowband FSK burst and a wideband
noise block. Run the tool you are about to trust against it and check it
reproduces the truth.

```sh
python3 mksynth.py                      # -> /tmp/synth.iq
python3 iqscan.py /tmp/synth.iq 1024000 # both chirps must come back CHIRP
python3 fsk.py /tmp/synth.iq 1024000 4.99 0.05   # 60 symbols, 2.00 kbaud
```

This is not ceremony. Every tool in this directory failed its first
reference run, in ways that would have produced confident nonsense on real
data — a chirp detector that required a frequency jump larger than the
signal could make, a bandwidth estimate that returned the whole band
whenever SNR was low, and three successive symbol-rate estimators that were
all defeated by the same under-smoothed input. See `docs/sdr-tools.md`.
