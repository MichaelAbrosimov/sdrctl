#!/usr/bin/env python3
"""Synthesise a reference capture with KNOWN content, to validate iqscan.py.

Ground truth written into 10 s at 1.024 Msps:
  t=2.0 s  LoRa-like sawtooth chirp, 125 kHz BW, centred +150 kHz, 400 ms
  t=5.0 s  narrowband FSK burst, ~20 kHz, centred -200 kHz, 30 ms
  t=7.5 s  wideband steady carrier block, 200 kHz, centred 0, 300 ms
  t=11.0 s slow LoRa-like chirp SF12, 125 kHz BW, -167 kHz, 2 s, ~8 dB SNR
Anything that fails to separate these three is not fit to judge real data.
"""
import numpy as np

FS = 1024000.0
T = 14.0
n = int(FS * T)
rng = np.random.default_rng(0)
x = (rng.normal(0, 1, n) + 1j * rng.normal(0, 1, n)) * 0.6

def at(t):
    return int(t * FS)

# --- chirp: sawtooth sweeping 125 kHz, repeating every 1 ms -------------
d = at(0.400)
tt = np.arange(d) / FS
bw, sym = 125e3, 1e-3
ph = np.cumsum(2 * np.pi * (bw * ((tt % sym) / sym - 0.5) + 150e3) / FS)
x[at(2.0):at(2.0) + d] += 12 * np.exp(1j * ph)

# --- narrowband FSK ------------------------------------------------------
d = at(0.030)
tt = np.arange(d) / FS
bits = np.repeat(rng.integers(0, 2, 60), d // 60)[:d]
ph = np.cumsum(2 * np.pi * (-200e3 + (bits * 2 - 1) * 10e3) / FS)
x[at(5.0):at(5.0) + d] += 12 * np.exp(1j * ph)

# --- wideband steady block (noise-like, no frequency ramp) ---------------
d = at(0.300)
blk = (rng.normal(0, 1, d) + 1j * rng.normal(0, 1, d))
f = np.fft.fftfreq(d, 1 / FS)
B = np.fft.fft(blk)
B[np.abs(f) > 100e3] = 0
x[at(7.5):at(7.5) + d] += 9 * np.fft.ifft(B) * np.sqrt(d / (2 * 100e3 / FS * d))

# --- SLOW chirp at LOW SNR: the SF12 case the first detector was blind to
# 125 kHz swept over a 32.8 ms symbol, 2 s long, only ~8 dB over noise.
d = at(2.0)
tt = np.arange(d) / FS
bw, sym = 125e3, 32.8e-3
ph = np.cumsum(2 * np.pi * (bw * ((tt % sym) / sym - 0.5) - 167e3) / FS)
x[at(11.0):at(11.0) + d] += 1.55 * np.exp(1j * ph)

out = np.empty(2 * n, dtype=np.uint8)
out[0::2] = np.clip(np.real(x) * 8 + 127.5, 0, 255).astype(np.uint8)
out[1::2] = np.clip(np.imag(x) * 8 + 127.5, 0, 255).astype(np.uint8)
out.tofile('/tmp/synth.iq')
print("wrote /tmp/synth.iq  expect: chirp@2.0s(+150k,125k), fsk@5.0s(-200k,20k), block@7.5s(0,200k)")
