#!/usr/bin/env python3
"""Assertions for the siprec-receiver live interop test.

Reads the receiver's SQLite DB + WAV files after real FreeSWITCH /
mod_siprec calls (see interop.sh) and verifies, from the SRS side:

  main      one mixed leg, ~8 s of 440 Hz tone (4 s + 4 s; the 3 s
            pause is compressed out), then an AMD notification posted
             against its derived VoiceML CallSid merges onto the same row
  stop      one leg of ~2 s (recording stopped mid-call)
  separate  two legs carrying different dominant tones (440 vs 1000 Hz)

Usage: check.py <db-path> <http-base-url> <notify-secret>
Exit code 0 = all checks passed.
"""
import math
import sqlite3
import struct
import sys
import urllib.request
import urllib.parse
import wave

FAILURES = []


def check(name, ok, detail=""):
    print(f"  [{'PASS' if ok else 'FAIL'}] {name} {detail}")
    if not ok:
        FAILURES.append(name)


def goertzel(samples, rate, freq):
    n = len(samples)
    if n == 0:
        return 0.0
    k = int(0.5 + n * freq / rate)
    w = 2 * math.pi * k / n
    coeff = 2 * math.cos(w)
    s0 = s1 = s2 = 0.0
    for x in samples:
        s0 = x + coeff * s1 - s2
        s2, s1 = s1, s0
    return math.sqrt(s1 * s1 + s2 * s2 - coeff * s1 * s2) / n


def load_wav(path):
    with wave.open(path, "rb") as w:
        rate, n = w.getframerate(), w.getnframes()
        raw = w.readframes(n)
    return rate, list(struct.unpack(f"<{len(raw)//2}h", raw))


def rms(samples):
    if not samples:
        return 0.0
    return math.sqrt(sum(x * x for x in samples) / len(samples))


def main():
    db_path, http_base, secret = sys.argv[1], sys.argv[2], sys.argv[3]
    db = sqlite3.connect(db_path)
    db.row_factory = sqlite3.Row

    calls = db.execute(
        "SELECT * FROM calls ORDER BY started_at").fetchall()
    check("four calls recorded", len(calls) == 4, f"(got {len(calls)})")
    if len(calls) != 4:
        return 1

    legs = {}
    for c in calls:
        legs[c["id"]] = db.execute(
            "SELECT * FROM legs WHERE call_id=? ORDER BY id",
            (c["id"],)).fetchall()

    for c in calls:
        check(f"call {c['id']} completed", c["status"] == "completed",
              f"(status={c['status']!r})")
        check(f"call {c['id']} has fs_uuid",
               bool(c["fs_uuid"]), f"(fs_uuid={c['fs_uuid']!r})")
        check(f"call {c['id']} has derived VoiceML CallSid",
              c["call_sid"] == "CA" + c["fs_uuid"].replace("-", ""))
        check(f"call {c['id']} legs have no write error",
              all(not l["write_error"] for l in legs[c["id"]]))

    outbound_sid = "CA00112233445566778899aabbccddeeff"
    outbound = next((c for c in calls if c["call_sid"] == outbound_sid), None)
    check("outbound CallSid channel was recorded", outbound is not None)
    if outbound is None:
        return 1
    check("pre-recording AMD adopted by CallSid only", outbound["amd_answered_by"] == "human")
    check("late ringing did not erase terminal disposition",
          outbound["disp_status"] == "completed" and outbound["disp_duration_sec"] == 2)
    check("connector account alias was retained", outbound["account_sid"] == "ACinterop")
    check("outbound CallSid recording has one leg", len(legs[outbound["id"]]) == 1)
    by_legs = sorted([c for c in calls if c["call_sid"] != outbound_sid], key=lambda c: len(legs[c["id"]]))
    stop_call, main_call, sep_call = by_legs[0], by_legs[1], by_legs[2]
    # stop and main both have 1 leg; tell them apart by duration.
    one_leg = sorted([stop_call, main_call],
                     key=lambda c: c["duration_sec"])
    stop_call, main_call = one_leg[0], one_leg[1]

    check("separate-streams call has 2 legs",
          len(legs[sep_call["id"]]) == 2)

    rate, samples = load_wav(legs[main_call["id"]][0]["wav_path"])
    dur = len(samples) / rate
    check("main leg is 8 kHz", rate == 8000)
    check("main duration ~8s (pause compressed out)",
          6.0 <= dur <= 10.0, f"(got {dur:.1f}s)")
    check("main leg carries the 440 Hz tone",
          goertzel(samples, rate, 440) > 500 and rms(samples) > 500,
          f"(g440={goertzel(samples, rate, 440):.0f} rms={rms(samples):.0f})")

    rate, samples = load_wav(legs[stop_call["id"]][0]["wav_path"])
    dur = len(samples) / rate
    check("stop leg ~2s (stopped mid-call)",
          1.0 <= dur <= 3.5, f"(got {dur:.1f}s)")

    dom = []
    for leg in legs[sep_call["id"]]:
        rate, samples = load_wav(leg["wav_path"])
        g440 = goertzel(samples, rate, 440)
        g1000 = goertzel(samples, rate, 1000)
        dom.append(440 if g440 > g1000 else 1000)
    check("separate legs carry different tones (440 vs 1000)",
          sorted(dom) == [440, 1000], f"(dominant per leg: {dom})")

    # Ordinary VoiceML callbacks know CallSid, not the recording SIP Call-ID.
    form = urllib.parse.urlencode({
        "CallSid": main_call["call_sid"],
        "AnsweredBy": "human",
        "MachineDetectionDuration": "1500",
    }).encode()
    req = urllib.request.Request(
        http_base + "/api/notify/amd", data=form,
        headers={"Authorization": "Bearer " + secret,
                 "Content-Type": "application/x-www-form-urlencoded"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        body = resp.read().decode()
    check("AMD notify accepted + matched",
          '"matched":true' in body, f"({body.strip()})")

    row = db.execute("SELECT amd_answered_by, amd_duration_ms FROM calls "
                     "WHERE id=?", (main_call["id"],)).fetchone()
    check("AMD disposition stored on the recording",
          row["amd_answered_by"] == "human" and row["amd_duration_ms"] == 1500,
          f"(got {row['amd_answered_by']!r}/{row['amd_duration_ms']})")

    print("INTEROP " + ("FAIL" if FAILURES else "PASS"))
    return 1 if FAILURES else 0


if __name__ == "__main__":
    sys.exit(main())
