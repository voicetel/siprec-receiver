# Recording VoiceML calls with siprec-receiver

VoiceML can fork any call's audio to a SIPREC
recording server with the `<Start><Siprec>` verb or the REST
`Calls/{CallSid}/Siprec` resource. This guide points that fork at
siprec-receiver, for both inbound and outbound calls, and links the
recordings to VoiceML's AMD and status webhooks.

Placeholders used below:

| Placeholder | Meaning |
|---|---|
| `$VOICEML_API` | Your VoiceML API base URL (the host your SDK or REST client already uses) |
| `AC…` / `$AUTH` | Your AccountSid and API credentials |
| `srs.example.com` | The public DNS name of your siprec-receiver host |

## How it fits together

```text
caller ──► VoiceML (FreeSWITCH + mod_siprec) ──► callee
                    │  SIPREC INVITE + RTP fork
                    ▼
            siprec-receiver  ◄── AMD / status webhooks (CallSid)
```

1. A VoiceML document (or REST call) starts SIPREC with a
   **connector name**.
2. VoiceML looks up that name among your account's enabled SIPREC
   connectors and has FreeSWITCH run `siprec <connectorName> <SRS URI>`
   on the call's channel.
3. mod_siprec sends siprec-receiver a SIPREC INVITE whose metadata
   `session_id` encodes the call's channel ID; RTP follows until the
   call ends or `<Stop><Siprec/>` runs.
4. siprec-receiver writes the WAV and stores the call with
   `call_sid` = the VoiceML CallSid, derived from that `session_id`
   (see [Correlation](#correlation-with-voiceml-webhooks)).

## 1. Make siprec-receiver reachable

VoiceML's media servers must reach the SRS for SIP and RTP. The
defaults bind to loopback, so set at least:

```sh
SIPREC_LISTEN=0.0.0.0:5070          # SIP (UDP and TCP)
SIPREC_LOCAL_IP=<public IP>         # advertised in the SDP answer; RTP is sent here
RTP_PORT_MIN=41000
RTP_PORT_MAX=42000
```

Firewall:

- Allow SIP on `5070` (TCP recommended, see below) and the UDP range
  `RTP_PORT_MIN`–`RTP_PORT_MAX` from VoiceML's media servers only. The
  SIP listener has no authentication (SIPREC defines none). Ask VoiceTel
  for the current source IP ranges; this guide does not list them.
- Use `transport=tcp` in the connector URI. SIPREC INVITEs are
  multipart (SDP plus XML metadata) and routinely exceed the UDP MTU.

For the webhook part of this guide, the web UI (`HTTP_LISTEN`) must also
be reachable over HTTPS from VoiceML, with `NOTIFY_SECRET` set.

## 2. Create a SIPREC connector in VoiceML

In the VoiceML portal: **Recordings → SIPREC connectors → New**.

| Field | Value |
|---|---|
| Connector name | The name your documents reference, e.g. `siprec-receiver` |
| Friendly name | Anything, e.g. `Call recorder` |
| SRS SIP URI | `sip:srs.example.com:5070;transport=tcp;accountsid=AC…` |
| Enabled | On |

`accountsid` is optional but recommended: siprec-receiver stores it as
the call's `account_sid`, which the UI shows and the
[full-record fetch](../README.md#full-call-record-from-voiceml-on-request)
needs. Use your own AccountSid; the URI is static, so every call through
this connector carries the same value.

Do **not** put `callsid` on the connector URI. It would be the same
for every call. siprec-receiver derives the CallSid per call instead.

A missing or disabled connector means no recording. In a document,
the verb fails with error **31940** and sends a `siprec-error` status
callback ("Unable to find connector configuration. Invalid Connector
Name. …"). The REST start answers HTTP 400, code **13520**.

## 3. Inbound calls

Point the phone number's Voice URL at a document that starts SIPREC
**before** connecting the call, so the whole conversation is captured:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Response>
  <Start>
    <Siprec name="rec" connectorName="siprec-receiver"
            statusCallback="https://app.example.com/siprec-events"/>
  </Start>
  <Dial>+15551234567</Dial>
</Response>
```

The fork records the inbound channel, so both sides of the `<Dial>`
bridge are in the recording. To record only part of a call, stop it
from a later document:

```xml
<Response>
  <Stop>
    <Siprec name="rec"/>
  </Stop>
  <Say>Recording has stopped.</Say>
</Response>
```

## 4. Outbound calls

Outbound calls need **mod_siprec v1.7.1 or later** on VoiceML's media
servers. VoiceML gives each originated channel the CallSid (`CA` + 32
hex) as its FreeSWITCH UUID. Earlier mod_siprec builds rejected that
UUID while building the SIPREC metadata ("could not derive RFC 7865
metadata IDs"), so the outbound recording never started.

Start SIPREC from the document the call runs when answered, either
inline (`Twiml`) or by `Url`:

```sh
curl -u "AC…:$AUTH" "$VOICEML_API/2010-04-01/Accounts/AC…/Calls.json" \
  --data-urlencode "To=+15551234567" \
  --data-urlencode "From=+15557654321" \
  --data-urlencode 'Twiml=<Response><Start><Siprec name="rec" connectorName="siprec-receiver"/></Start><Say>This call is recorded.</Say><Dial>+15550001111</Dial></Response>'
```

Or start (and stop) it on a call that is already in progress, inbound or
outbound, through REST:

```sh
# Start
curl -u "AC…:$AUTH" \
  "$VOICEML_API/2010-04-01/Accounts/AC…/Calls/CA…/Siprec.json" \
  --data-urlencode "Name=rec" \
  --data-urlencode "ConnectorName=siprec-receiver" \
  --data-urlencode "StatusCallback=https://app.example.com/siprec-events"
# → {"sid":"SR…", "status":"in-progress", …}

# Stop (by the SiprecSid, or by the Name it was started with)
curl -u "AC…:$AUTH" \
  "$VOICEML_API/2010-04-01/Accounts/AC…/Calls/CA…/Siprec/SR….json" \
  --data-urlencode "Status=stopped"
```

There is no `GET` for SIPREC sessions; the start response and the
status callbacks carry the `SiprecSid`.

## Behavior to know

- **One session per call.** A second `<Start><Siprec>` (or REST start)
  on the same call replaces the first. siprec-receiver sees two SIPREC
  dialogs for the same CallSid and files both under one call: the
  second recording's WAVs are added as more legs.
- **Stopping is a hard stop.** `<Stop><Siprec/>` and REST
  `Status=stopped` both end the fork and send the SRS a BYE. The
  recording is then finalized. Starting again creates a new recording.
- **Status callbacks.** `siprec-started` fires when the fork starts;
  `siprec-error` fires when the connector cannot be resolved. No
  stopped event is sent, although VoiceML's markup reference lists
  one. Each callback carries `AccountSid`, `CallSid`, `SiprecSid`,
  `SiprecName`, `SiprecEvent`, and `Timestamp`.
- **`track` and `<Parameter>` have no effect.** VoiceML accepts and
  parses them but does not pass them to the recorder, despite the
  markup reference saying `<Parameter>` reaches the INVITE. Mixed (one
  WAV) versus separate (two WAVs) streams is a FreeSWITCH-wide
  mod_siprec setting (`separate-streams`), not a per-call choice.

## Correlation with VoiceML webhooks

VoiceML's webhooks identify a call only by `CallSid`. siprec-receiver
derives the same CallSid from every VoiceML recording, so webhooks
pointed at its notify endpoints attach to the right call in either
order:

- **Inbound:** the channel UUID is a plain UUID, and VoiceML's CallSid
  is `CA` followed by that UUID's 32 hex digits without dashes.
- **Outbound:** the channel UUID is the CallSid itself, and mod_siprec
  encodes it from the same 32 hex digits.

siprec-receiver decodes the metadata `session_id` to those 16 bytes and
stores `call_sid = "CA" + hex`. A `callsid` URI parameter, when
present, still takes precedence.

For outbound calls, the stored `fs_uuid` is the dashed form of the
CallSid's hex digits, not the literal `CA…` channel UUID FreeSWITCH
shows.

Send AMD and the final status to siprec-receiver. VoiceML's webhook
sender cannot set headers, so authorize the callbacks one of two ways:

- **Signature (recommended):** store the account's API key under
  **Integrations** in siprec-receiver and set `PUBLIC_BASE_URL` to the
  exact `https://` origin the callback URLs use. VoiceML signs every
  callback with that key (`X-Twilio-Signature`), so the URLs need no
  secret and nothing sensitive lands in access logs. A signed callback
  can only update its own account's calls.
- **`?secret=`:** append the notify secret to each callback URL, as
  below.

Either callback method works; with `StatusCallbackMethod=GET` the
fields arrive as the query string:

```sh
curl -u "AC…:$AUTH" "$VOICEML_API/2010-04-01/Accounts/AC…/Calls.json" \
  --data-urlencode "To=+15551234567" \
  --data-urlencode "From=+15557654321" \
  --data-urlencode "Url=https://app.example.com/voice/recorded-call.xml" \
  --data-urlencode "MachineDetection=Enable" \
  --data-urlencode "AsyncAmd=true" \
  --data-urlencode "AsyncAmdStatusCallback=https://srs.example.com/api/notify/amd?secret=$NOTIFY_SECRET" \
  --data-urlencode "StatusCallback=https://srs.example.com/api/notify/disposition?secret=$NOTIFY_SECRET"
```

To capture the dialed party's outcome too, point the `<Number>`'s
`statusCallback` (and `amdStatusCallback` with `machineDetection`) at
the same URLs. Each dialed leg is stored as its own call, linked to
the recorded call by its `ParentCallSid`:

```xml
<Dial>
  <Number statusCallback="https://srs.example.com/api/notify/disposition?secret=$NOTIFY_SECRET"
          machineDetection="Enable"
          amdStatusCallback="https://srs.example.com/api/notify/amd?secret=$NOTIFY_SECRET">+15550001111</Number>
</Dial>
```

For inbound calls, set the phone number's **StatusCallback** to the
disposition URL above. A callback configured for more events than
`completed` is fine: the receiver orders events by `SequenceNumber`
and `Timestamp`, ignores late or duplicate ones, and never lets a
progress event (`ringing`, `in-progress`) replace a final outcome.
The call's billable length is read from `CallDuration` (seconds);
`Duration` (whole minutes) is ignored.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `siprec-error` callback / error 31940, or REST 400 / 13520 | `connectorName` does not match an **enabled** connector on this account (names are exact) |
| `siprec-started` fires but nothing appears in siprec-receiver | SIP is not reaching `SIPREC_LISTEN`: firewall, wrong host/port in the connector URI, or UDP fragmentation (switch to `transport=tcp`) |
| Recording row exists but the WAV is silent | RTP is not reaching `SIPREC_LOCAL_IP` / the RTP port range |
| Outbound calls never record, inbound calls do | The media servers run a mod_siprec build older than v1.7.1 (see [Outbound calls](#4-outbound-calls)) |
| AMD/status rows never merge with recordings | The recording was made before this receiver version (no derived `call_sid`), or the webhooks point at another account's receiver |
