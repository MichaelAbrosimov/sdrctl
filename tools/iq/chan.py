#!/usr/bin/env python3
"""Shift, low-pass and decimate one channel out of a cu8 IQ capture.

Shifting alone is not enough: with both 868.10 and 868.50 still in the
band, rtl_433's FSK demodulator picks one peak from each channel and calls
them mark and space. Isolating the channel is what makes the bits readable.

  chan.py <in.iq> <out.iq> <shift_hz> <decim> [fs]
"""
import sys
import numpy as np

src, dst = sys.argv[1], sys.argv[2]
df = float(sys.argv[3])
D = int(sys.argv[4])
FS = float(sys.argv[5]) if len(sys.argv) > 5 else 1024000.0

NTAPS = 129
cut = 0.45 / D                                  # cycles/sample, before decim
k = np.arange(NTAPS) - (NTAPS - 1) / 2
h = (2 * cut) * np.sinc(2 * cut * k) * np.hamming(NTAPS)
h = (h / h.sum()).astype(np.float64)

raw = np.memmap(src, dtype=np.uint8, mode='r')
n = len(raw) // 2
CH = 4_000_000
w = 2 * np.pi * df / FS
tail = np.zeros(NTAPS - 1, dtype=np.complex128)
phase = 0

with open(dst, 'wb') as f:
    for pos in range(0, n, CH):
        m = min(CH, n - pos)
        seg = raw[2 * pos:2 * (pos + m)].astype(np.float64) - 127.5
        x = seg[0::2] + 1j * seg[1::2]
        x *= np.exp(1j * w * np.arange(pos, pos + m))
        # overlap-save: carry the filter's memory across chunk boundaries
        xx = np.concatenate([tail, x])
        tail = xx[-(NTAPS - 1):].copy()
        y = np.convolve(xx, h, mode='valid')
        # keep decimation phase continuous across chunks
        start = (-phase) % D
        y = y[start::D]
        phase = (phase + len(np.arange(start, len(xx) - NTAPS + 1, D)) * D
                 - (len(xx) - NTAPS + 1)) % D
        g = 4.0                                 # filtering removes most power
        out = np.empty(2 * len(y), dtype=np.uint8)
        out[0::2] = np.clip(np.real(y) * g + 127.5, 0, 255).astype(np.uint8)
        out[1::2] = np.clip(np.imag(y) * g + 127.5, 0, 255).astype(np.uint8)
        out.tofile(f)
print(f"wrote {dst}: shift {df/1e3:+.1f} kHz, decim {D} -> fs {FS/D/1e3:.0f} kHz")
