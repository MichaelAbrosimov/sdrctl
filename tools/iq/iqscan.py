#!/usr/bin/env python3
"""Find bursts in an rtl_sdr u8 IQ capture and classify their shape.

Stage 1 walks the file in chunks and builds a power envelope, so a 235 MB
capture never has to be resident. Stage 2 re-reads only the detected bursts
and asks one question per burst: does the instantaneous peak frequency ramp
linearly and wrap (a chirp, i.e. LoRa), or does it sit still (FSK/ASK)?
"""
import sys
import numpy as np

path = sys.argv[1]
FS = float(sys.argv[2]) if len(sys.argv) > 2 else 1024000.0
BLK = 1024                      # envelope resolution: 1 ms at 1.024 Msps

raw = np.memmap(path, dtype=np.uint8, mode='r')
nsamp = len(raw) // 2
nblk = nsamp // BLK
print(f"file: {len(raw)/1e6:.1f} MB   samples: {nsamp}   duration: {nsamp/FS:.1f} s")

# ---- stage 1: power envelope -------------------------------------------
env = np.empty(nblk, dtype=np.float32)
CH = 4_000_000                  # samples per chunk
pos = 0
while pos < nblk:
    n = min(CH // BLK, nblk - pos)
    seg = raw[2 * pos * BLK: 2 * (pos + n) * BLK].astype(np.float32) - 127.5
    i = seg[0::2]
    q = seg[1::2]
    p = (i * i + q * q).reshape(n, BLK).mean(axis=1)
    env[pos:pos + n] = p
    pos += n

envdb = 10 * np.log10(np.maximum(env, 1e-9))
floor = np.median(envdb)
mad = np.median(np.abs(envdb - floor))
thr = floor + max(6.0, 8 * mad)
print(f"noise floor: {floor:.1f} dB   MAD: {mad:.2f}   threshold: {thr:.1f} dB")

above = envdb > thr
idx = np.flatnonzero(np.diff(np.concatenate(([0], above.view(np.int8), [0]))))
starts, ends = idx[0::2], idx[1::2]

# merge bursts separated by less than 10 ms
if len(starts):
    ms, me = [starts[0]], [ends[0]]
    for s, e in zip(starts[1:], ends[1:]):
        if s - me[-1] < 10:
            me[-1] = e
        else:
            ms.append(s); me.append(e)
    starts, ends = np.array(ms), np.array(me)

dur_ms = (ends - starts) * BLK / FS * 1000
print(f"bursts detected: {len(starts)}")
if len(starts) == 0:
    sys.exit(0)
for lo, hi, name in [(0, 5, "<5 ms"), (5, 50, "5-50 ms"), (50, 500, "50-500 ms"),
                     (500, 5000, "0.5-5 s"), (5000, 1e9, ">5 s")]:
    c = int(((dur_ms >= lo) & (dur_ms < hi)).sum())
    if c:
        print(f"  {name:>10}: {c}")

# ---- stage 2: classify the strongest bursts ----------------------------
power = np.array([envdb[s:e].max() for s, e in zip(starts, ends)])
order = np.argsort(-(power + 10 * np.log10(dur_ms + 1)))[:12]

def iq(a, b):
    seg = raw[2 * a:2 * b].astype(np.float32) - 127.5
    return seg[0::2] + 1j * seg[1::2]


def spec(x, nf):
    nfr = len(x) // nf
    if nfr < 4:
        return None
    X = x[:nfr * nf].reshape(nfr, nf) * np.hanning(nf)
    return np.abs(np.fft.fftshift(np.fft.fft(X, axis=1), axes=1)) ** 2


print("\n  #   t(s)  dur(ms)  snr(dB)  ctr(kHz)  bw(kHz)  ramp  wraps  verdict")
for k, bi in enumerate(sorted(order, key=lambda b: starts[b])):
    s, e = starts[bi], ends[bi]
    a, b = s * BLK, min(e * BLK, nsamp)
    dur = (b - a) / FS

    # The right frame length depends on the chirp RATE, which is exactly
    # what we do not know in advance: too short and a slow sweep looks
    # static, too long and a fast one aliases. So sweep the frame length
    # and keep the view that makes the clearest case.
    xb = iq(a, b)
    best = None
    for NF in (64, 128, 256, 512, 1024, 2048, 4096):
        S = spec(xb, NF)
        if S is None:
            continue
        na = max(0, a - (b - a))
        N = spec(iq(na, a), NF) if a - na >= NF * 4 else None
        prof = S.mean(axis=0)
        nprof = N.mean(axis=0) if N is not None else np.median(prof)
        excess = np.maximum(prof - nprof, 0.0)
        if excess.max() <= 0:
            continue
        occ = excess > 0.25 * excess.max()
        bins = np.flatnonzero(occ)
        bw_i = len(bins) * FS / NF / 1000
        ctr_i = (bins.mean() - NF / 2) * FS / NF / 1000
        lo, hi = bins.min(), bins.max() + 1
        track = S[:, lo:hi].argmax(axis=1).astype(np.float64)
        d = np.diff(track)
        span = max(hi - lo, 1)
        wraps_i = int((d < -0.4 * span).sum())
        moving = d[d != 0]
        rising_i = float((moving > 0).mean()) if len(moving) else 0.0
        score = rising_i if (wraps_i >= 3 and span >= 8) else 0.0
        if best is None or score > best[0]:
            best = (score, rising_i, wraps_i, bw_i, ctr_i, NF)
    if best is None:
        continue
    _, rising, wraps, bw, ctr, NF = best

    if wraps >= 3 and rising > 0.7 and bw > 60:
        verdict = "CHIRP (LoRa-like)"
    elif bw < 40:
        verdict = "narrowband"
    else:
        verdict = "wideband, no chirp"
    print(f"{k:3d} {a/FS:6.1f} {dur*1000:8.1f} {power[bi]-floor:8.1f} "
          f"{ctr:9.1f} {bw:8.1f} {rising:5.2f} {wraps:6d}  {verdict} [NF={NF}]")
