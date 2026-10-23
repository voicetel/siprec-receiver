#!/usr/bin/env python3
"""Assertions for the siprec-receiver live interop test.

Reads the receiver's SQLite DB + WAV files after real FreeSWITCH /
mod_siprec calls (see interop.sh) and verifies, from the SRS side:

  main      one mixed leg, ~8 s of 440 Hz tone (4 s + 4 s; the 3 s
            pause is compressed out), then an AMD notification posted
             against its derived VoiceML CallSid merges onto the same row
  stop      one leg of ~2 s (recording stopped mid-call)
  separate  two legs carrying different dominant tones (440 vs 1000 Hz)
  long      one leg of ~12 s: not cut off by the 6 s profile RTP timeout

then VoiceML/callBroadcast-shaped callbacks against those recordings:
a signed StatusCallback (X-Twilio-Signature with an API key stored
through the admin UI), a GET callback with ?secret=, a <Dial> child leg
linked by ParentCallSid with its Dial-leg AMD, and the refusals (wrong
key, another account's call). Last, the hot-swap probe's result.

Usage: check.py <db-path> <http-base-url> <notify-secret> <artifacts-dir>
Exit code 0 = all checks passed.
"""
import base64
import hashlib
import hmac
import http.cookiejar
import math
import re
import sqlite3
import struct
import sys
import urllib.error
import urllib.request
import urllib.parse
import wave
from email.utils import formatdate

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


def sign(key, url, fields):
    """X-Twilio-Signature as VoiceML sends it: base64 HMAC-SHA1 of the
    URL plus each form field's name and value in name order."""
    data = url + "".join(k + fields[k] for k in sorted(fields))
    return base64.b64encode(hmac.new(key.encode(), data.encode(), hashlib.sha1).digest()).decode()


def post(url, fields, headers):
    req = urllib.request.Request(
        url, data=urllib.parse.urlencode(fields).encode(),
        headers={"Content-Type": "application/x-www-form-urlencoded", **headers})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def get(url):
    try:
        with urllib.request.urlopen(url, timeout=10) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def store_api_key(http_base, account, key):
    """Save a VoiceML API key the way an administrator does: sign in
    (the dev bypass, loopback only), then post the Integrations form."""
    jar = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
    opener.open(http_base + "/auth/login", timeout=10).read()
    page = opener.open(http_base + "/admin/integrations", timeout=10).read().decode()
    csrf = re.search(r'name="csrf_token" value="([0-9a-f]+)"', page).group(1)
    form = urllib.parse.urlencode({"account_sid": account, "api_key": key, "csrf_token": csrf}).encode()
    with opener.open(http_base + "/admin/integrations", data=form, timeout=10) as resp:
        return "VoiceML key saved" in resp.read().decode()


def status_callback(call_sid, account, status, seq, call_duration, **extra):
    """The StatusCallback form VoiceML (callBroadcast) sends."""
    fields = {
        "CallSid": call_sid, "AccountSid": account, "ApiVersion": "2010-04-01",
        "Direction": "inbound", "From": "+12165550100", "To": "+12165550101",
        "Caller": "+12165550100", "Called": "+12165550101", "CallStatus": status,
        "Timestamp": formatdate(usegmt=False), "CallbackSource": "call-progress-events",
        "SequenceNumber": str(seq), "FromCity": "", "FromState": "", "FromZip": "",
        "FromCountry": "US", "ToCity": "", "ToState": "", "ToZip": "", "ToCountry": "US",
    }
    if status in ("completed", "busy", "failed", "no-answer", "canceled"):
        fields["CallDuration"] = str(call_duration)
        fields["Duration"] = str((call_duration + 59) // 60)
    fields.update(extra)
    return fields


def check_callbacks(db, http_base, secret, main_call, stop_call, outbound):
    account, key = "AC0123456789abcdef0123456789abcdef", "interop-account-api-key"
    check("API key stored through the admin UI", store_api_key(http_base, account, key))

    url = http_base + "/api/notify/disposition"
    fields = status_callback(main_call["call_sid"], account, "completed", 7, 8)
    code, body = post(url, fields, {"X-Twilio-Signature": sign(key, url, fields)})
    check("signed StatusCallback accepted + matched", code == 200 and '"matched":true' in body, f"({code} {body.strip()})")
    row = db.execute("SELECT disp_status, disp_duration_sec, account_sid FROM calls WHERE id=?", (main_call["id"],)).fetchone()
    check("signed outcome stored with CallDuration seconds, not Duration minutes",
          row["disp_status"] == "completed" and row["disp_duration_sec"] == 8, f"(got {tuple(row)})")

    bad = status_callback(main_call["call_sid"], account, "busy", 8, 1)
    code, _ = post(url, bad, {"X-Twilio-Signature": sign("not-the-key", url, bad)})
    check("callback signed with the wrong key refused", code == 401, f"({code})")

    foreign = status_callback(outbound["call_sid"], account, "busy", 9, 1)
    code, _ = post(url, foreign, {"X-Twilio-Signature": sign(key, url, foreign)})
    check("signature cannot touch another account's call", code == 403, f"({code})")

    q = status_callback(stop_call["call_sid"], "", "completed", 5, 3)
    q["secret"] = secret
    code, body = get(url + "?" + urllib.parse.urlencode(q))
    check("GET StatusCallback with ?secret= accepted + matched", code == 200 and '"matched":true' in body, f"({code} {body.strip()})")
    row = db.execute("SELECT disp_duration_sec, disp_raw FROM calls WHERE id=?", (stop_call["id"],)).fetchone()
    check("GET outcome stored, secret kept out of the raw payload",
          row["disp_duration_sec"] == 3 and secret not in row["disp_raw"], f"(got {row['disp_duration_sec']})")

    child_sid = "CAfeedfacefeedfacefeedfacefeedface"
    child = status_callback(child_sid, account, "completed", 4, 5, ParentCallSid=main_call["call_sid"])
    code, body = post(url, child, {"X-Twilio-Signature": sign(key, url, child)})
    check("<Dial> child leg callback accepted as its own call", code == 200 and '"matched":false' in body, f"({code} {body.strip()})")
    amd_url = http_base + "/api/notify/amd"
    amd = {"CallSid": child_sid, "AccountSid": account, "AnsweredBy": "machine_end_beep", "MachineDetectionDuration": "2400"}
    code, body = post(amd_url, amd, {"X-Twilio-Signature": sign(key, amd_url, amd)})
    check("Dial-leg AMD callback merges onto the child leg", code == 200 and '"matched":true' in body, f"({code} {body.strip()})")
    row = db.execute("SELECT parent_call_sid, amd_answered_by, disp_duration_sec FROM calls WHERE call_sid=?", (child_sid,)).fetchone()
    check("child leg linked to the recorded parent",
          row is not None and row["parent_call_sid"] == main_call["call_sid"] and row["amd_answered_by"] == "machine_end_beep",
          f"(got {tuple(row) if row else None})")


def main():
    db_path, http_base, secret, artifacts = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
    db = sqlite3.connect(db_path)
    db.row_factory = sqlite3.Row

    calls = db.execute(
        "SELECT * FROM calls ORDER BY started_at").fetchall()
    check("five calls recorded", len(calls) == 5, f"(got {len(calls)})")
    if len(calls) != 5:
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
    others = [c for c in calls if c["call_sid"] != outbound_sid]
    sep_call = max(others, key=lambda c: len(legs[c["id"]]))
    # stop, main and long have one leg each; tell them apart by duration.
    stop_call, main_call, long_call = sorted(
        [c for c in others if c is not sep_call], key=lambda c: c["duration_sec"])

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

    rate, samples = load_wav(legs[long_call["id"]][0]["wav_path"])
    dur = len(samples) / rate
    check("long leg ~12s (not hung up at the 6 s RTP timeout)",
          11.0 <= dur <= 13.5, f"(got {dur:.1f}s)")

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

    check_callbacks(db, http_base, secret, main_call, stop_call, outbound)

    try:
        hotswap = open(artifacts + "/hotswap.txt").read().strip()
    except OSError:
        hotswap = "missing"
    # Informational: an install that replaces mod_siprec.so needs a
    # FreeSWITCH restart when the old file stays mapped after unload.
    print(f"  [INFO] mod_siprec hot-swap after unload + load: {hotswap}")

    print("INTEROP " + ("FAIL" if FAILURES else "PASS"))
    return 1 if FAILURES else 0


if __name__ == "__main__":
    sys.exit(main())
