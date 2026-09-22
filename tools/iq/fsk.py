#!/usr/bin/env python3
"""Demodulate a 2-FSK burst straight from IQ and print its bits.

rtl_433's analyzer is built around the protocols it knows; when it has "no
clue" there is nothing left to tune. Instantaneous frequency is model-free:
angle(x[n] * conj(x[n-1])) is the frequency at that sample, and a 2-FSK
signal spends its time at two values of it.

  fsk.py <file.iq> <fs> <t_start> <t_dur>
"""
import sys
import numpy as np

path, FS, t0, td = sys.argv[1], float(sys.argv[2]), float(sys.argv[3]), float(sys.argv[4])

raw = np.memmap(path, dtype=np.uint8, mode='r')
a, b = int(t0 * FS), int((t0 + td) * FS)
seg = raw[2 * a:2 * b].astype(np.float64) - 127.5
x = seg[0::2] + 1j * seg[1::2]
amp = np.abs(x)

# --- gate on the burst itself ------------------------------------------
win = max(int(FS * 20e-6), 4)
sm = np.convolve(amp, np.ones(win) / win, mode='same')
# Gate on an ABSOLUTE level above the noise, not on percentiles of the
# window: a short packet inside a long window leaves the 90th percentile
# sitting in the noise, and the "burst" then swallows the whole window.
noise = float(np.median(sm))
spread = float(np.median(np.abs(sm - noise))) or 1e-9
on = sm > noise + max(4.0 * spread, 0.5 * noise)
edges = np.flatnonzero(np.diff(np.concatenate(([0], on.view(np.int8), [0]))))
bs, be = edges[0::2], edges[1::2]
if len(bs) == 0:
    print("no burst above the noise in this window")
    sys.exit(1)
# bridge gaps shorter than 1 ms, then keep the longest run
mb_s, mb_e = [bs[0]], [be[0]]
for u, v in zip(bs[1:], be[1:]):
    if u - mb_e[-1] < int(FS * 1e-3):
        mb_e[-1] = v
    else:
        mb_s.append(u); mb_e.append(v)
bs, be = np.array(mb_s), np.array(mb_e)
k = int(np.argmax(be - bs))
s, e = int(bs[k]), int(be[k])
if e - s < int(FS * 5e-4):
    print(f"burst too short: {(e-s)/FS*1000:.2f} ms")
    sys.exit(1)
print(f"burst: {(e-s)/FS*1000:.2f} ms  ({s} .. {e} samples)  "
      f"level {sm[s:e].mean():.1f} vs noise {noise:.1f}  "
      f"({len(bs)} candidate bursts in window)")

xb = x[s:e]
f = np.angle(xb[1:] * np.conj(xb[:-1])) * FS / (2 * np.pi)
# Smoothing must be a fraction of the SYMBOL, not a fixed few microseconds:
# under-smoothed, noise shatters every symbol into spurious runs and no
# timing estimator downstream can recover. Two passes — smooth coarsely,
# measure the symbol, then re-smooth at a tenth of it.
def smooth(sig, w):
    return np.convolve(sig, np.ones(w) / w, mode='same') if w > 1 else sig

fr = f.copy()
w = max(len(fr) // 400, 3)
f = smooth(fr, w)

# --- two frequency levels ----------------------------------------------
loF, hiF = np.percentile(f, 15), np.percentile(f, 85)
thr = (loF + hiF) / 2
print(f"FSK levels: {loF/1e3:+.1f} kHz / {hiF/1e3:+.1f} kHz  "
      f"deviation {(hiF-loF)/2e3:.1f} kHz  threshold {thr/1e3:+.1f} kHz")

bits = (f > thr).astype(np.int8)

# --- symbol rate from the shortest runs --------------------------------
chg = np.flatnonzero(np.diff(bits)) + 1
runs = np.diff(np.concatenate(([0], chg, [len(bits)])))
runs = runs[runs > 0]
# Run lengths cannot give the symbol: noise splits symbols into shorter
# runs, and every DIVISOR of the true period fits them equally well (the
# first attempt locked onto exactly one third of the real rate). The
# autocorrelation of random NRZ data instead falls as a triangle that
# reaches zero at precisely one symbol, and noise only spikes at lag 0.
# With the signal smoothed to a fraction of a symbol, the run lengths are
# finally meaningful: the shortest CLEAN run is one symbol. Take a low
# percentile rather than the minimum so one surviving glitch cannot set it,
# then re-smooth at a tenth of that and repeat once.
def measure(sig):
    bb = (sig > thr).astype(np.int8)
    ch = np.flatnonzero(np.diff(bb))
    rr = np.diff(np.concatenate(([0], ch + 1, [len(bb)])))
    rr = rr[rr > 0]
    if len(rr) < 6:
        return None, bb
    return float(np.percentile(rr, 10)), bb


force = float(sys.argv[5]) if len(sys.argv) > 5 else 0.0
unit, bits = measure(f)
if force > 0:
    f = smooth(fr, max(int(force / 4), 3))
    bits = (f > thr).astype(np.int8)
    unit = force
    print(f"symbol FORCED to {unit:.1f} samples = {FS/unit/1e3:.2f} kbaud")
if unit is None:
    print("too few runs to time")
    sys.exit(1)
for _ in range(0 if force > 0 else 2):
    f = smooth(fr, max(int(unit / 10), 3))
    u2, bits = measure(f)
    if u2 is None:
        break
    unit = u2
baud = FS / unit
print(f"symbol {unit:.1f} samples = {baud/1e3:.2f} kbaud "
      f"(smoothing {max(int(unit/10),3)} samples)")

# --- sample the middle of each symbol ----------------------------------
n = int(len(bits) / unit)
pos = (np.arange(n) + 0.5) * unit
sym = bits[np.clip(pos.astype(int), 0, len(bits) - 1)]
s = ''.join(str(int(v)) for v in sym)
print(f"symbols: {len(s)}")
print("bits:", s[:256])
by = [s[i:i+8] for i in range(0, len(s) - 7, 8)]
print("hex :", ' '.join(f"{int(v,2):02x}" for v in by[:40]))
# a preamble is a long alternating run; report where it ends
alt = 0
for i in range(1, len(s)):
    if s[i] != s[i-1]:
        alt += 1
    else:
        if alt >= 16:
            print(f"preamble of {alt} alternating symbols ends at bit {i}")
            break
        alt = 0
