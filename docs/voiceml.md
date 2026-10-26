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

## Receiving AMD and final dispositions

### The key distinction: siprec-receiver receives; it does not relay

siprec-receiver is a **terminal consumer** for these callbacks. It
validates one, stores it, merges it with the recording identified by
`CallSid`, and returns JSON such as `{"id":42,"matched":true}`. It
has no “forward to my application” setting and never makes an
outbound webhook to your application.

That matters because VoiceML has one configured destination for each
callback kind on a call (and one per `<Number>` leg):

- If VoiceML should deliver the outcome **only to siprec-receiver**,
  point VoiceML directly at siprec-receiver (direct receiving below).
- If your application **also** needs the same AMD or status callback,
  do not point VoiceML only at siprec-receiver. Put your application
  or a small proxy in the VoiceML callback path and have that first
  recipient send a copy to siprec-receiver (proxying below).

Do not confuse these three callback families:

| Callback | VoiceML setting | Sent to siprec-receiver? | Meaning |
|---|---|---|---|
| AMD verdict | `AsyncAmdStatusCallback`; `<Number amdStatusCallback>` | `POST` or `GET /api/notify/amd` | `human`, `machine_*`, `fax`, etc. |
| Final call disposition | `StatusCallback`; `<Number statusCallback>` | `POST` or `GET /api/notify/disposition` | `completed`, `busy`, `no-answer`, `failed`, duration, SIP/error detail |
| SIPREC lifecycle | `<Siprec statusCallback>` / REST `StatusCallback` on the Siprec resource | **No** | `siprec-started` / `siprec-error` for the recording fork itself; send this to your application or omit it |

AMD is an answering-party classification delivered while the call is
in progress. Disposition is the call's eventual outcome. They are
separate events and normally arrive at different times.

### Option A — direct receiving (siprec-receiver only)

Use this when no other system needs VoiceML's AMD/status callbacks:

```text
VoiceML ──AMD────────► https://srs.example.com/api/notify/amd
        └─status─────► https://srs.example.com/api/notify/disposition
                              │
                              ▼
                    stored + merged with recording
```

VoiceML ignores the JSON body on these asynchronous callbacks; the
body is for logs, tests, and other machine senders. The recording and
notifications can arrive in either order. A notification that arrives
first creates a searchable AMD/disposition-only row; the recording
adopts it when the SIPREC dialog is finalized.

Authorize direct VoiceML callbacks in either of two ways:

- **Signature (recommended):** store the account's API key under
  **Integrations** in siprec-receiver and set `PUBLIC_BASE_URL` to the
  exact `https://` origin the callback URLs use. VoiceML signs every
  form/GET callback with that key (`X-Twilio-Signature`), so the URLs
  need no secret and nothing sensitive lands in access logs. The
  callback must carry its `AccountSid`; a signed callback can only
  update that account's calls. JSON bodies cannot use this signature
  scheme. `NOTIFY_SECRET` must still be set: it is the master switch
  that enables both notify endpoints.
- **Shared secret:** append `?secret=$NOTIFY_SECRET` to each callback
  URL, as below, because VoiceML's webhook sender cannot set an
  `Authorization` header. The secret can then appear in access and
  proxy logs, so prefer signatures where possible.

Either callback method works as POST or GET. With
`StatusCallbackMethod=GET`, the fields arrive as the query string:

```sh
curl -u "AC…:$AUTH" "$VOICEML_API/2010-04-01/Accounts/AC…/Calls.json" \
  --data-urlencode "To=+15551234567" \
  --data-urlencode "From=+15557654321" \
  --data-urlencode "Url=https://app.example.com/voice/recorded-call.xml" \
  --data-urlencode "MachineDetection=Enable" \
  --data-urlencode "AsyncAmd=true" \
  --data-urlencode "AsyncAmdStatusCallback=https://srs.example.com/api/notify/amd?secret=$NOTIFY_SECRET" \
  --data-urlencode "StatusCallback=https://srs.example.com/api/notify/disposition?secret=$NOTIFY_SECRET" \
  --data-urlencode "StatusCallbackEvent=completed"
```

To capture the dialed party's outcome too, point the `<Number>`'s
`statusCallback` (and `amdStatusCallback` with `machineDetection`) at
the same endpoint pair. Each dialed leg is stored as its own call,
linked to the recorded call by its `ParentCallSid`; preserve
`ParentCallSid` exactly and do not use it as the child's own
correlation key:

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

Expected receiver responses are:

| Result | Meaning |
|---|---|
| `200 {"id":…,"matched":true}` | An existing call row was updated |
| `200 {"id":…,"matched":false}` | A notification-only row was created; a later recording can adopt it |
| `400` | Required field missing (`AnsweredBy` or `CallStatus`), or no `CallSid`/`SipCallId`/`FsUuid` correlation key |
| `401` | Neither a valid shared secret nor a valid account signature |
| `403` | A signed callback tried to touch another account's call |
| `503` | `NOTIFY_SECRET` is unset, so both notify endpoints are disabled |

### Option B — proxying (your application also needs them)

Use this when the application that receives VoiceML callbacks today
must keep receiving them. VoiceML sends each callback once, to the
configured URL; the first recipient is responsible for fan-out:

```text
                         ┌──── copy (Bearer NOTIFY_SECRET) ───► siprec-receiver
VoiceML ──► app or proxy ┤
                         └──── original callback ─────────────► your application
```

There are two equivalent implementations.

#### B1. Let the application forward a copy

Keep the existing application URLs in VoiceML. In the application's
AMD and status handlers:

1. Validate and process the VoiceML callback exactly as today.
2. Send a server-side copy of **all original fields** to the matching
   siprec-receiver endpoint (`/amd` for AMD, `/disposition` for
   status), using `Authorization: Bearer $NOTIFY_SECRET`.
3. Return the application's original response to VoiceML.

Make the copy with a short timeout (for example 2 seconds), log a
non-2xx result, and do not fail the VoiceML callback solely because
siprec-receiver is unavailable — unless your recording-compliance
policy explicitly requires failing closed. If the copy must never be
lost, write it to the application's durable outbox/queue and retry
from a worker. The receiver's upserts are idempotent, and duplicate
dispositions are ordered, so a retried copy is safe.

Do not ask siprec-receiver to validate the copy using VoiceML's
original `X-Twilio-Signature` unless the proxy is transparent at the
same public origin and path. Signature validation reconstructs
`PUBLIC_BASE_URL + /api/notify/…`; a callback signed for
`https://app.example.com/callbacks/amd` will not verify as though it
had been sent to the receiver's notify URL. The proxy-to-receiver hop
therefore has its own machine credential: the Bearer secret.

#### B2. Mirror at an edge proxy

If the application cannot be changed, put a proxy in front of its
callback URLs and mirror each request to the receiver. An NGINX
sketch (one location per callback kind, so AMD and disposition are
never crossed):

```nginx
location = /callbacks/amd {
    mirror /_srs_amd;
    proxy_pass https://app.example.com;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_http_version 1.1;
}

location = /_srs_amd {
    internal;
    proxy_pass https://srs.example.com/api/notify/amd;
    proxy_set_header Authorization "Bearer NOTIFY_SECRET";
    proxy_http_version 1.1;
}

location = /callbacks/disposition {
    mirror /_srs_disposition;
    proxy_pass https://app.example.com;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_http_version 1.1;
}

location = /_srs_disposition {
    internal;
    proxy_pass https://srs.example.com/api/notify/disposition;
    proxy_set_header Authorization "Bearer NOTIFY_SECRET";
    proxy_http_version 1.1;
}
```

Then configure VoiceML with the **proxy's** public URLs
(`https://proxy.example.com/callbacks/amd` and
`https://proxy.example.com/callbacks/disposition`). The application's
response remains the response VoiceML sees; the mirrored copy's
response is ignored. Preserve the original method, query string, and
every form field — especially `CallSid`, `AccountSid`,
`ParentCallSid`, `SequenceNumber`, `Timestamp`, and `CallDuration`.
Your application must validate `X-Twilio-Signature` against the
public proxy URL it was configured with, which is why the proxy
forwards the original `Host`.

### End-to-end checks

1. Simulate one callback against the receiver (replace the IDs with a
   real test call's):

   ```sh
   curl -i https://srs.example.com/api/notify/amd \
     -H "Authorization: Bearer $NOTIFY_SECRET" \
     --data-urlencode "CallSid=CA…" \
     --data-urlencode "AccountSid=AC…" \
     --data-urlencode "AnsweredBy=machine_end_beep" \
     --data-urlencode "MachineDetectionDuration=3100"
   # → {"id":…,"matched":true|false}
   ```

2. Place a recorded VoiceML call with AMD enabled. Confirm, in any
   arrival order: the WAV is playable, the call page shows the AMD
   badge, and after hangup its outcome and call duration are set.
3. In proxy mode, stop siprec-receiver and place another test call.
   The application must still receive its callback; only the proxy's
   copy may fail. Restart the receiver and confirm normal calls merge
   again.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `siprec-error` callback / error 31940, or REST 400 / 13520 | `connectorName` does not match an **enabled** connector on this account (names are exact) |
| `siprec-started` fires but nothing appears in siprec-receiver | SIP is not reaching `SIPREC_LISTEN`: firewall, wrong host/port in the connector URI, or UDP fragmentation (switch to `transport=tcp`) |
| Recording row exists but the WAV is silent, or it is marked "no audio" | RTP is not reaching `SIPREC_LOCAL_IP` / the RTP port range |
| Outbound calls never record, inbound calls do | The media servers run a mod_siprec build older than v1.7.1 (see [Outbound calls](#4-outbound-calls)) |
| AMD/status rows never merge with recordings | The recording was made before this receiver version (no derived `call_sid`), or the webhooks point at another account's receiver |
