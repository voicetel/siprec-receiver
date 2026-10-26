# siprec-receiver — SIPREC Session Recording Server (SRS)

A standalone [RFC 7866][rfc7866] Session Recording Server with
[RFC 7865][rfc7865] metadata handling, built to receive recordings from
FreeSWITCH `mod_siprec` (and any other compliant Session
Recording Client), persist them with their metadata, collect AMD
(answering-machine detection) dispositions for the recorded calls, and
present everything in a Google-OAuth-protected web UI with administrator
and reviewer roles.

[rfc7865]: https://datatracker.ietf.org/doc/html/rfc7865
[rfc7866]: https://datatracker.ietf.org/doc/html/rfc7866
[rfc2046]: https://datatracker.ietf.org/doc/html/rfc2046
[rfc3261]: https://datatracker.ietf.org/doc/html/rfc3261
[rfc3550]: https://datatracker.ietf.org/doc/html/rfc3550
[rfc4566]: https://datatracker.ietf.org/doc/html/rfc4566

```
   original call                          recorded call's audio + metadata
  ┌──────────────┐   RTP (both ways)   ┌──────────────────────────────┐
  │   carrier /  │◄───────────────────►│          FreeSWITCH          │
  │   endpoint   │                     │        + mod_siprec (SRC)    │
  └──────────────┘                     └──────────────┬───────────────┘
                                                      │  SIPREC INVITE
                                                      │  multipart: SDP + RS-Metadata
                                                      │  + forked RTP (G.711)
                                                      ▼
  ┌───────────────────────────────────────────────────────────────────┐
  │                      siprec-receiver (SRS)                        │
  │   SIP listener (UDP/TCP/TLS) ─► RTP receivers ─► WAV per stream   │
  │                                     │                             │
  │                                     ▼                             │
  │                        SQLite: calls · legs · users               │
  │                                     ▲                             │
  │   POST /api/notify/amd ─────────────┘  (AMD disposition, secret)  │
  │   Web UI (Google OAuth) ──► review · play · download · admin      │
  └───────────────────────────────────────────────────────────────────┘
```

The SRS core (`internal/siprecsrv`, `internal/rtp`, and the
`internal/recording` WAV writer) is extracted from VoiceML's
production SIPREC receiver; the SQLite store, the AMD notification
endpoint, and the web UI are new in this repository.

## Status

| Concept | Spec | Status |
|---|---|---|
| SIPREC INVITE with multipart SDP + RS-Metadata | [RFC 7866 §6.1.1][rfc7866-6.1.1] / [§9.1][rfc7866-9.1] / [RFC 2046][rfc2046] | ✅ SDP part required; metadata part optional (recording proceeds with positional leg labels) |
| `Require: siprec` / `+sip.src` handling | [RFC 7866 §6.2][rfc7866-6.2] | ✅ Accepted; the SRS's own Contact carries `+sip.srs` (a `sips:` URI on the TLS listener) |
| Answer shape | [RFC 7866 §7.2][rfc7866-7.2] | ✅ Plain `application/sdp` 200 OK (never multipart), one `recvonly` `m=audio` per offered stream, `a=label` echoed |
| Mixed (single-stream) recording | [RFC 7866 §8.3.2][rfc7866-8.3.2] | ✅ One WAV leg per call (both directions already mixed by the SRC) |
| Separate RX/TX streams | [RFC 7866 §8.3.1][rfc7866-8.3.1] | ✅ Two WAV legs per call, labelled from RS-Metadata participants |
| Pause / resume re-INVITE | [RFC 7866 §7.1.1.1][rfc7866-7.1.1.1] | ✅ The SRC pauses each stream with `a=inactive` and resumes with `a=sendonly`; both re-INVITEs (or UPDATEs) re-answer on the same ports and keep writing the same files, so a pause never splits the recording. A paused dialog's idle timeout is 24 h instead of `SIPREC_SESSION_IDLE_SEC`. No RTP arrives while paused and only gaps under 2 s are filled with silence, so a pause is cut out of the WAV rather than kept as silence |
| BYE teardown | [RFC 7866 §5.1][rfc7866-5.1] | ✅ Closes WAVs, releases RTP ports, finalizes the call row |
| Re-INVITE replacing media shape | [RFC 7866 §7.1.1.1][rfc7866-7.1.1.1] | ✅ Prior session is finalized, a fresh one is recorded |
| UPDATE / session timers | [RFC 3311](https://datatracker.ietf.org/doc/html/rfc3311) / [RFC 4028](https://datatracker.ietf.org/doc/html/rfc4028) / [RFC 7866 §9.2][rfc7866-9.2] | ✅ UPDATE refreshes, pauses/resumes (same media shape) and carries RS-Metadata; `Session-Expires` is echoed with the SRC as refresher (422 below 90 s); an SDP-less re-INVITE is re-offered the current SDP |
| `<recording>` / `<datamode>` / `<group>` / `<session>` | [RFC 7865 App. A][rfc7865] | ✅ Parsed, persisted (raw XML + normalized JSON) |
| `<participant>` AORs (`<nameID aor>`) | [RFC 7865 App. A][rfc7865] | ✅ Caller/callee numbers derived from participants; used as leg labels |
| Metadata IDs (`session_id` = parent call UUID, base64) | [RFC 7865 §6.9][rfc7865] | ✅ Decoded back to the FreeSWITCH call UUID and stored for correlation |
| `<stream>` + `<participantstreamassoc>` label mapping | [RFC 7865 App. A][rfc7865] | ✅ Stream → sending participant resolution for leg naming |
| G.711 PCMU / PCMA, 8 kHz | [RFC 3551](https://datatracker.ietf.org/doc/html/rfc3551) | ✅ The only offered codecs; WAV is 16-bit mono PCM at 8 kHz |
| RTP loss accounting | [RFC 3550 §5.1][rfc3550] | ✅ Sequence-gap tracking in the receiver |
| Transports | [RFC 7866 §12.1][rfc7866-12.1] | ✅ UDP + TCP; ✅ SIPS/TLS when `SIP_TLS_CERT`/`SIP_TLS_KEY` are set |
| SRTP recording media | [RFC 7866 §8.1.2][rfc7866-8.1.2] / [§12.2][rfc7866-12.2] | ❌ Not supported — an SRTP offer is refused with 488 + `Warning: 302` (mod_siprec cannot offer SDES) |
| IPv6 | [RFC 4566 §5.7][rfc4566] | ❌ SDP answers advertise IPv4 (`c=IN IP4`); run the SRS on IPv4 |

[rfc7866-5.1]: https://datatracker.ietf.org/doc/html/rfc7866#section-5.1
[rfc7866-6.1.1]: https://datatracker.ietf.org/doc/html/rfc7866#section-6.1.1
[rfc7866-6.2]: https://datatracker.ietf.org/doc/html/rfc7866#section-6.2
[rfc7866-7.1.1.1]: https://datatracker.ietf.org/doc/html/rfc7866#section-7.1.1.1
[rfc7866-7.2]: https://datatracker.ietf.org/doc/html/rfc7866#section-7.2
[rfc7866-8.1.2]: https://datatracker.ietf.org/doc/html/rfc7866#section-8.1.2
[rfc7866-8.3.1]: https://datatracker.ietf.org/doc/html/rfc7866#section-8.3.1
[rfc7866-8.3.2]: https://datatracker.ietf.org/doc/html/rfc7866#section-8.3.2
[rfc7866-9.1]: https://datatracker.ietf.org/doc/html/rfc7866#section-9.1
[rfc7866-9.2]: https://datatracker.ietf.org/doc/html/rfc7866#section-9.2
[rfc7866-12.1]: https://datatracker.ietf.org/doc/html/rfc7866#section-12.1
[rfc7866-12.2]: https://datatracker.ietf.org/doc/html/rfc7866#section-12.2

## Build

Pure Go (CGO only for the SQLite driver); no other build dependencies.

```sh
make build        # ./siprec-receiver
make test         # go test ./...
make vet
```

Or directly: `go build ./cmd/siprec-receiver`.

## Quick start

```sh
cp .env.example .env
# Edit at minimum:
#   ADMIN_EMAIL=you@example.com
#   SESSION_SECRET=<long random string>
#   NOTIFY_SECRET=<long random string>
#   GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET  (see "Google OAuth setup")
set -a; . ./.env; set +a
make run
```

Point your SRC at `sip:<host>:5070`, place a recorded call, then open
http://127.0.0.1:8080 and sign in.

For local development without a Google OAuth client, set
`DEV_AUTH_EMAIL=you@example.com` (and `ADMIN_EMAIL` to the same address):
`/auth/login` then signs you in directly. **Never set `DEV_AUTH_EMAIL`
in production** — it bypasses Google sign-in entirely, and the server
logs a warning on startup while it is set.

## Configuration

All configuration is environment variables (see also `.env.example`).
A value that does not parse or is out of range stops the start with
an error naming it; nothing silently falls back to a default.

| Variable | Default | Purpose |
|---|---|---|
| `SIPREC_LISTEN` | `127.0.0.1:5070` | SIP listen address for the SRS |
| `SIPREC_NETWORKS` | `udp,tcp` | SIP transports to listen on (comma list) |
| `SIPREC_LOCAL_IP` | `127.0.0.1` | IPv4 address advertised in SDP (`c=IN IP4`); must be reachable by the SRC's RTP fork |
| `SIPREC_RTP_BIND_IP` | `SIPREC_LOCAL_IP` | Local IPv4 UDP bind address. Set to a local interface or `0.0.0.0` when the advertised address is a public/NAT address not assigned locally |
| `SIPREC_ALLOW_CIDRS` | — (any source) | Comma list of CIDRs/IPs allowed to send SIPREC INVITEs; sources outside get 403. Set this (or firewall equivalently) in production |
| `SIPREC_MAX_SESSIONS` | `256` | Concurrent SIPREC sessions cap; further INVITEs get 503 |
| `SIPREC_SESSION_IDLE_SEC` | `300` | Finalize a session after this many seconds with no SIP message and no RTP (lost BYE, crashed SRC) |
| `SIP_TLS_LISTEN` | `127.0.0.1:5071` | TLS (SIPS) listen address, used only when cert+key are set |
| `SIP_TLS_CERT` / `SIP_TLS_KEY` | — | PEM files enabling the SIPS listener (must be set together) |
| `RTP_PORT_MIN` / `RTP_PORT_MAX` | `41000` / `42000` | UDP port pool for RTP receivers, within 1–65535 (even ports only, per RTP convention) |
| `RECORDING_DIR` | `./data/recordings` | Where WAV files are written (`0600` files in a `0700` directory) |
| `DB_PATH` | `./data/siprec-receiver.db` | SQLite database path (WAL mode, `0600`) |
| `HTTP_LISTEN` | `127.0.0.1:8080` | Web UI + notify endpoint listen address |
| `PUBLIC_BASE_URL` | `http://127.0.0.1:8080` | Public base URL; the OAuth redirect is `<base>/auth/callback`, and signed VoiceML callbacks are verified against `<base>` plus the request path |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | — | Google OAuth client (see below) |
| `GOOGLE_HOSTED_DOMAIN` | — | When set, sign-in requires a Google Workspace account from this domain (the `hd` claim) |
| `SESSION_SECRET` | ephemeral | HMAC secret for session cookies (at least 32 bytes when set); set a stable random value in production or every restart signs everyone out |
| `ADMIN_EMAIL` | — | Bootstrap administrator email (always admin; managed via env, not the UI) |
| `ADMIN_GOOGLE_SUB` | — | Pins the bootstrap administrator to one Google account ID (the `sub` claim), as a user's first sign-in pins any other user. Recommended unless `GOOGLE_HOSTED_DOMAIN` already restricts sign-in to your Workspace |
| `NOTIFY_SECRET` | — (endpoints disabled) | Shared secret for the notify endpoints, at least 16 bytes. Keep it to `A-Z a-z 0-9 - . _ ~` if it goes in a `?secret=` URL (a bare `+` is accepted as typed; anything else must be percent-encoded) |
| `RETENTION_DAYS` | `0` (disabled) | Initial retention default. An administrator can override it under **Settings**; once saved there, the UI value wins and applies on the maintenance timer’s next run |
| `VOICEML_BASE_URL` | — (fetches disabled) | Base URL of the VoiceML deployment for on-request Call-record fetches (per-account keys live in the UI, see Integrations). Must be `https://` (plain `http://` only to a loopback host): every fetch carries an API key |
| `DEV_AUTH_EMAIL` | — | **Dev only:** bypass Google and sign in as this address |

Put the HTTP listener behind a TLS-terminating reverse proxy in
production and set `PUBLIC_BASE_URL` to the public `https://` URL, so
session cookies are marked `Secure`.

### Google OAuth setup

1. In the [Google Cloud Console](https://console.cloud.google.com/apis/credentials),
   create an OAuth 2.0 Client ID (Web application).
2. Add the authorized redirect URI: `<PUBLIC_BASE_URL>/auth/callback`
   (e.g. `https://srs.example.com/auth/callback`).
3. Set `GOOGLE_CLIENT_ID` and `GOOGLE_CLIENT_SECRET` from that client.

Sign-in requests the `openid email profile` scopes. The ID token is
verified against Google on every login, the email must be verified,
and it must then be authorized (see [Web UI and roles](#web-ui-and-roles))
— a Google account alone never grants access.

> **Using VoiceML?** See
> [Recording VoiceML calls with siprec-receiver](docs/voiceml.md)
> for the connector setup, inbound/outbound examples, and the
> specific difference between **receiving** AMD/final-disposition
> callbacks in siprec-receiver and **proxying** them when your own
> application must receive them too.

## SRC integration (mod_siprec)

In FreeSWITCH `autoload_configs/siprec.conf.xml`:

```xml
<recording-server name="default">
  <settings>
    <param name="host" value="127.0.0.1"/>
    <param name="port" value="5070"/>
    <param name="transport" value="udp"/>
  </settings>
</recording-server>
```

Then in the dialplan: `<action application="siprec" data="default"/>`.

Notes:

- mod_siprec's default (mixed) mode offers one stream; its opt-in
  `separate-streams` mode offers two labelled streams. Both are
  recorded — one WAV leg per stream, grouped under one call in the UI.
- Multipart SIPREC bodies routinely exceed the UDP MTU. Real sofia
  stacks handle this (fragmentation / TCP per [RFC 3261][rfc3261]
  §18.1.1); if recordings fail to start over UDP, switch the
  recording-server entry to `tcp` — the SRS listens on both.
- The SIP listener has no in-band authentication (SIPREC has none).
  Firewall `SIPREC_LISTEN` so only your FreeSWITCH hosts can reach it.

### Per-call correlation URI parameters

Whoever chooses the SRS URI can stamp correlation keys as URI
parameters, e.g. mod_siprec's per-call ad-hoc form:

```xml
<action application="siprec"
        data="myrec sip:srs.example.com:5070;vm-callsid=CA0123456789abcdef0123456789abcdef;vm-account=AC0123456789abcdef0123456789abcdef"/>
```

| Parameter | Stored as | Purpose |
|---|---|---|
| `vm-callsid` | `call_sid` | Lets AMD notifications (keyed by CallSid) merge with this recording automatically |
| `vm-account` | `account_sid` | Tenant/account attribution, shown in the UI |

The aliases `callsid` and `accountsid` are also accepted; the `vm-*`
parameters take precedence. With no explicit CallSid, the receiver
derives `CA` plus the 32 hex identity digits from mod_siprec's metadata
`session_id`. This matches VoiceML inbound calls and outbound calls on
a mod_siprec build supporting CallSid channel identifiers. A static
VoiceML connector should carry only the account parameter, never a
fixed CallSid. Ordinary CallSid-only AMD/status webhooks then correlate
without needing the recording dialog's SIP Call-ID.

## Wire behavior

- **INVITE:** the body must contain an SDP part; an RS-Metadata part is
  used for naming/correlation when present and ignored (with positional
  leg labels) when absent or malformed. A body with no SDP is a 400.
  The SIP Call-ID is treated as an opaque dialog key — any ordinary SIP
  format is accepted — but it must be present, at most 512 bytes, and
  printable ASCII without spaces or controls; a missing or malformed
  Call-ID gets 400 and never creates a session. The same validation
  applies to BYE. Sources outside `SIPREC_ALLOW_CIDRS` get 403; more
  than 8 offered streams gets 488; at the session cap, 503.
- **Unrecordable media:** an offer with any live stream that is not
  plain RTP (`RTP/SAVP`, DTLS-SRTP: the SRS holds no keys and would
  record ciphertext) or that carries neither PCMU nor PCMA is refused
  with 488 and a `Warning` (302 or 305) before anything is allocated.
  A re-INVITE or UPDATE refused this way leaves the recording as it
  was. A declined (port 0) stream is never a reason to refuse.
- **200 OK:** always plain `application/sdp` — one `m=audio` per offered
  stream with an SRS-pool port, the chosen G.711 payload type (the first
  offered PCMU/PCMA, dynamic payload types included; the answer always
  uses the static PT), `a=recvonly` (`a=inactive` for an inactive
  offer), plus a session-level `a=recvonly` (FreeSWITCH clears the
  recording leg's RTP timeout only for that; without it a profile's
  `rtp-timeout-sec` hangs a long recording up, as the SRS sends no RTP
  back), the offer's `a=label` echoed, a `Contact` for the dialog,
  and an `o=` version that increases on every re-INVITE answer.
  Offered port-0 streams are answered port 0 and never recorded.
  (Some SRC answer-side parsers do not extract SDP from a multipart
  200; plain SDP is the interoperable choice.)
- **Dialog integrity:** sessions are shared across all transports (a
  dialog may start on UDP and end on TCP). In-dialog requests (BYE,
  re-INVITE) must match the INVITE's From-tag and source IP, or they
  get 481 and the recording is untouched — knowing a Call-ID is not
  enough to stop or replace someone else's recording. A BYE, or a
  To-tagged re-INVITE, for a dialog the SRS does not have (it already
  ended, or was lost in a restart) also gets 481 (RFC 3261 §12.2.2)
  instead of opening an empty recording.
- **Recorded files:** `<RECORDING_DIR>/<random>.wav` — server-generated
  names opened with `O_EXCL`, so a reused or colliding Call-ID can
  never overwrite an existing recording, and filenames leak neither
  Call-IDs nor phone numbers. Each leg's display label (the sending
  participant's number from RS-Metadata when resolvable, else the SDP
  label, else `leg<N>`) lives in the database. WAV files are written
  `0600` in a `0700` directory.
- **RTP discipline:** receivers bind `SIPREC_RTP_BIND_IP` (defaulting to `SIPREC_LOCAL_IP`)
  and lock onto the first (source, SSRC) they see — anything else is
  dropped, so stray packets cannot inject audio into a recording.
  Sequence gaps are filled with silence from RTP timestamps (duration
  stays honest, legs stay in sync), late packets are dropped, padding
  is stripped, and ports are reused first-in-first-out so a previous
  call's teardown tail cannot land in the next call. A leg's sample
  budget (rate vs. wall clock, 4 h hard cap) stops packet floods from
  filling the disk through one port.
- **Pause semantics:** while a recording is paused the SRC forks no
  RTP, so paused wall-clock time is *compressed out* of the WAV — the
  file (and `duration_sec`, which counts recorded samples) contains
  only the audio that was actually sent. A 4 s + 3 s-pause + 4 s call
  yields an ≈8 s recording, not 11 s.
  Explicitly paused dialogs have a separate 24-hour inactivity allowance
  (or `SIPREC_SESSION_IDLE_SEC`, if longer), so a normal five-minute
  media-idle timeout does not end a long pause. BYE and shutdown still
  finalize them immediately; abandoned paused dialogs eventually expire.
- **Write failures are loud, never silent:** the first WAV write error
  on a leg (the classic case is a full disk) is logged immediately as
  `WAV WRITE FAILED`, stored on the leg, and shown in the UI (a
  ⚠ badge in the list, the error text on the call page). The audio
  after the failure point is lost, but the record says so.
- **Ports:** one even UDP port per stream from the `RTP_PORT_MIN`–
  `RTP_PORT_MAX` pool, released when the session ends. Pool exhaustion
  answers 500 and records nothing.
- **Dialog end:** BYE is answered first, then closes the WAVs and
  finalizes the call. A dialog that never sends BYE is finalized by
  the idle reaper after `SIPREC_SESSION_IDLE_SEC` without SIP or RTP
  activity, and a graceful shutdown finalizes every active session
  before exiting — recordings are not stranded by restarts. In both
  cases the SRS then sends its own BYE to the SRC's Contact (bounded
  to 2 s, not retried), so the SRC stops forking audio at a port the
  pool will reuse; mod_siprec v1.7.1+ stops its fork on that BYE.
- **Legs that heard nothing:** a leg that never received an RTP packet
  while another leg of the dialog did is a declared stream the SRC
  never used (mod_siprec sends one mixed stream when the call's audio
  is multichannel): it gets no leg row and its empty WAV is deleted.
  When no leg heard anything, the legs are kept and marked "no audio"
  on the call page and in the list, so the call stays on record with
  the reason.
- **Crash recovery:** if the SRS itself dies mid-recording, the WAVs
  on disk are adopted at the next startup: their headers are repaired
  in place (a crashed file still carries the writer's placeholder
  sizes), durations are recomputed from the audio present, and each
  becomes a normal `completed` call marked `recovered-orphan` in the
  direction field, started at the file's modification time. Files that
  are not WAVs this recorder writes (8 kHz mono 16-bit PCM with the data
  chunk at byte 36) are logged and left alone, byte for byte.

## AMD call notifications

`POST /api/notify/amd` logs the AMD disposition for a call. It exists so
the answering-machine verdict produced during dialing (by VoiceML,
a dialer, or any AMD engine) ends up on the same record as the audio.

It accepts the exact field names VoiceML sends to an
`AsyncAmdStatusCallback` URL, as a form-encoded (the webhook shape) or
JSON `POST`, or as the query string of a `GET` (a callback configured
with `StatusCallbackMethod=GET`; the query, minus `secret`, is kept as
the raw payload):

| Field | Notes |
|---|---|
| `AnsweredBy` | **Required.** `human`, `machine_start`, `machine_end_beep`, `machine_end_silence`, `machine_end_other`, `fax`, `unknown`. VoiceML's extended `screened`, `screened_ivr`, `disconnected` are also stored; other values are stored verbatim (a logging endpoint must not drop dispositions) |
| `CallSid` | Correlation key: matches a recording stamped `vm-callsid` |
| `MachineDetectionDuration` | Milliseconds to verdict |
| `AccountSid`, `From`, `To`, `CallStatus`, `Direction` | Stored with the disposition; `Caller`/`Called` are accepted as `From`/`To` synonyms |
| `SipCallId` | The SIPREC recording dialog's SIP Call-ID — direct match |
| `FsUuid` | The FreeSWITCH parent call UUID — matches the decoded metadata `session_id` |
| `ParentCallSid` | Not a correlation key. Set on a `<Dial>` child leg's callbacks: the leg is stored as its own call and linked to the parent (see below) |

A request with none of `CallSid`, `SipCallId`, `FsUuid` cannot be
correlated and is rejected with 400.

Auth is the `NOTIFY_SECRET` shared secret, presented as
`Authorization: Bearer <secret>` (or the `X-Notify-Secret` header, or
`?secret=` for webhook senders that cannot set headers — VoiceML's
sender is one). With no secret configured the endpoint answers 503
rather than running open.

A VoiceML callback can authorize itself instead with the
`X-Twilio-Signature` it already carries, which keeps the secret out of
callback URLs and access logs. The signature is checked with the API
key stored for the payload's `AccountSid` under **Integrations**,
over `PUBLIC_BASE_URL` plus the request path and query (so
`PUBLIC_BASE_URL` must be the exact scheme and host the callback URL
names). A request authorized by an account's signature may only touch
that account's calls: one naming a call stored under another account
is refused with 403 and changes nothing. JSON bodies are not signed
this way; send those with the secret.

Notifications are upserts, order-independent:

- AMD **after** the recording: the recording's row gains the disposition.
- AMD **before** the recording (or with no recording at all): an
  AMD-only row (`status=notified`) is created and shown in the UI
  immediately; when the recording later arrives carrying the same
  `CallSid` or FreeSWITCH UUID, it adopts the disposition and the
  placeholder row disappears.
- One call, one row: every row an event's keys name (`CallSid`, a
  recording dialog's SIP Call-ID, `FsUuid`) is folded into one, so
  notifications keyed differently never leave a call split. A row
  carrying a *different* `CallSid` is another call and is left alone.
  A notification's `SipCallId` is only a lookup key; it is never
  written onto a recording's row.
- A restarted recording (a second SIPREC dialog for the same `CallSid`
  or FreeSWITCH UUID) attaches to the same call: its legs join the
  first dialog's, and the call's duration sums the dialogs.
- A `<Dial>` child leg is its own call. Its callbacks carry its own
  `CallSid` plus `ParentCallSid`, the call SIPREC recorded. The leg's
  row is linked to the parent: the parent's page lists its dialed legs
  with their outcome and AMD verdict, the list badges the leg, and
  searching for the parent's CallSid finds both. The Dial-leg AMD
  callback names no parent; its row is linked once any of the leg's
  status callbacks arrives, or when its VoiceML record is fetched.

```sh
# Form-encoded (the VoiceML/Twilio webhook shape)
curl -X POST https://srs.example.com/api/notify/amd \
  -H "Authorization: Bearer $NOTIFY_SECRET" \
  --data-urlencode "CallSid=CA0123456789abcdef0123456789abcdef" \
  --data-urlencode "AnsweredBy=machine_end_beep" \
  --data-urlencode "MachineDetectionDuration=3100" \
  --data-urlencode "SipCallId=abc123@fs.example.com"
# {"id":1,"matched":true}

# JSON
curl -X POST https://srs.example.com/api/notify/amd \
  -H "Authorization: Bearer $NOTIFY_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"CallSid":"CA0123456789abcdef0123456789abcdef",
       "AnsweredBy":"human","MachineDetectionDuration":1250,
       "FsUuid":"123e4567-e89b-12d3-a456-426614174000"}'
```

`matched` tells the poster whether a recording row already existed.
Responses: 200 logged · 400 malformed/no key/no `AnsweredBy` · 401 bad
secret and no valid signature · 403 signed for another account's call ·
503 endpoint disabled. Bodies are capped at 64 KB; the raw
payload is kept on the call record for audit and shown in the UI.

## Call disposition notifications

`POST /api/notify/disposition` is the final-outcome companion: it logs
what ultimately happened to the call — the content of the terminal
(`completed`) StatusCallback. Same auth, body shapes, correlation
keys, upsert semantics, and response codes as the AMD endpoint, except
`CallStatus` is required instead of `AnsweredBy`:

| Field | Notes |
|---|---|
| `CallStatus` | **Required.** `completed`, `busy`, `no-answer`, `failed`, `canceled`, … (stored verbatim) |
| `SequenceNumber` / `Timestamp` | Optional event order. Duplicate/older events are ignored; terminal results cannot be replaced by nonterminal events, and on a same-second `Timestamp` tie a terminal result beats a nonterminal one. Without comparable ordering, the first terminal result is retained. A value that does not parse is logged and ignored, never a 400 |
| `CallDuration` | Billable call seconds (kept separate from the recording's audio duration). `Duration` is not read: Twilio-shaped senders put the call's length in whole minutes there |
| `SipResponseCode` | Final SIP response code, when the poster knows it |
| `ErrorCode` / `ErrorMessage` | Failure detail |
| `AnsweredBy` | Terminal callbacks repeat the AMD verdict; fills the AMD fields when still empty |
| `CallSid`, `AccountSid`, `From`, `To`, `Direction`, `SipCallId`, `FsUuid`, `ParentCallSid` | Same meaning as the AMD endpoint |

```sh
curl -X POST https://srs.example.com/api/notify/disposition \
  -H "Authorization: Bearer $NOTIFY_SECRET" \
  --data-urlencode "CallSid=CA0123456789abcdef0123456789abcdef" \
  --data-urlencode "CallStatus=completed" \
  --data-urlencode "CallDuration=62" \
  --data-urlencode "SipResponseCode=200"
```

The list view shows the outcome next to the AMD disposition; the call
page shows duration, SIP code, error, and the raw payload.

## Full call record from VoiceML (on request)

A recording row can be enriched with the complete calling record from
VoiceML — final status, billable duration, price, timestamps,
`answered_by`, everything in the Call resource. Nothing is fetched
automatically: a signed-in user clicks **Fetch full record from
VoiceML** on the call page (`POST /recordings/{id}/fetch-record`),
the receiver GETs
`/2010-04-01/Accounts/{AccountSid}/Calls/{CallSid}.json` with the
account's API key as Basic auth, stores the pretty-printed JSON on the
call, and fills any still-empty identity/disposition fields from it
(recording-owned fields are never overwritten).

The fetch refuses an AccountSid or CallSid that is not `AC`/`CA` plus
32 hex digits (both come from notifications and SIP URI parameters and
end up in the request path) and follows no redirects, so the key
never travels anywhere but `VOICEML_BASE_URL`.

Because VoiceML keys are per-account, an administrator stores
one key per AccountSid under **Integrations** in the UI (with
`VOICEML_BASE_URL` set in the environment). Keys are write-only
— never rendered back — and are used solely as the fetch's Basic-auth
password. Fetch failures surface as a page message with the HTTP
status; upstream error bodies are never proxied into the page (they
can echo account SIDs).

## Web UI and roles

Sign-in is Google OAuth (see setup above). Authorization is an
allowlist, resolved on **every** request, so removing or disabling a
user takes effect on their next page load — no session revocation list
is needed.

| Capability | Administrator | User |
|---|---|---|
| List/search/filter recordings | ✅ | ✅ |
| Open a call: metadata, raw RS-Metadata + AMD payload | ✅ | ✅ |
| Play audio in the browser, download WAVs | ✅ | ✅ |
| Fetch/refresh the full call record from VoiceML | ✅ | ✅ |
| Delete a recording (row + WAV files) | ✅ | ❌ (403) |
| Add/remove users, grant the admin role | ✅ | ❌ (403) |
| Revoke a user's sessions (sign them out everywhere) | ✅ | ❌ (403) |
| Disable or re-enable a user (a disabled user is refused until enabled; changing their role does not enable them) | ✅ | ❌ (403) |
| Store/remove per-account VoiceML API keys | ✅ | ❌ (403) |
| Configure retention and purge expired calls | ✅ | ❌ (403) |
| View the audit log | ✅ | ❌ (403) |

- **Administrator** — the `ADMIN_EMAIL` bootstrap address (always admin,
  cannot be removed in the UI), plus any added user whose role is
  `admin`.
- **User** — anyone an administrator adds under **Users** with the
  `user` role. Users review recordings; they cannot change anything.

Identity is pinned, not just allowlisted: a user's first sign-in
records their Google account ID (`sub`), and later sign-ins must
present the same one — an allowlisted email address, which anyone
can register a Google account for, stops being sufficient on its
own. The bootstrap administrator has no row to pin to: set
`ADMIN_GOOGLE_SUB` to pin it. With `GOOGLE_HOSTED_DOMAIN` set, sign-in
additionally requires a Workspace account from that domain. **Revoke
sessions** bumps a per-user epoch that invalidates every outstanding
cookie at once (for the bootstrap administrator too; a removed and
re-added user starts at a fresh random epoch). The bootstrap
administrator is the break-glass account: if the database cannot be
read, its sessions are not epoch-checked.
Every administrative action (deletions, user and key changes,
session revocations, record fetches, retention changes, and manual
purges) is written to the **Audit log** (who, what, when) and shown
to administrators.

There is deliberately **no per-tenant scoping**: every signed-in
reviewer sees every recording. The allowlist is the access model —
add exactly the people who may review all calls.

The recordings list is a call search: filter by free text (number,
CallSid, SIP Call-ID, FreeSWITCH UUID, disposition), caller number,
called number, start-date range, final disposition, and AMD
disposition. Results are newest first, 50 per page, and every row
retains its **time, From, To, disposition, and AMD disposition**, so
a search can be scanned without opening each call. Each call's page
shows every stored field, one audio player + download
link per leg, and collapsible raw AMD/metadata payloads. Anonymous
requests redirect to the sign-in page; the only unauthenticated
routes are `GET /healthz`, the OpenAPI spec and docs (below), the
OAuth flow, and the secret-gated notify endpoints. Sign-out is a
POST form (a cross-site GET cannot
log a user out), and all responses carry `X-Frame-Options: DENY`, a
frame-ancestors CSP, `nosniff`, and `Cache-Control: no-store` (pages
and recordings hold call audio and numbers; only the public OpenAPI
spec may be cached).

## API documentation (OpenAPI + ReDoc)

The HTTP surface is specified as OpenAPI 3.1 in
`internal/web/openapi.yaml` and served by the binary itself:

- `GET /openapi.yaml` — the spec (no auth).
- `GET /api/docs` — a [ReDoc](https://github.com/Redocly/redoc)
  rendering of it (no auth; the ReDoc bundle loads from the jsDelivr
  CDN under a page-scoped CSP, the spec is same-origin).

The spec covers the notify endpoints in wire detail (VoiceML/Twilio
field names, correlation keys, upsert semantics), the health and
auth flows, and every UI/admin operation with its forms and
redirects. A served-spec test asserts every implemented route is
documented.

Vetting: the spec scores **100/100 with zero violations** under
vacuum's recommended ruleset, with two rules disabled in the
committed `vacuum-ruleset.yaml` because they contradict the wire
formats being documented (PascalCase webhook field names;
browser-form `/delete` action paths). Check it with:

```sh
make spec     # vacuum lint -r vacuum-ruleset.yaml internal/web/openapi.yaml
```

(Install vacuum with
`go install github.com/daveshanley/vacuum@latest`.)

## Architecture

```
cmd/siprec-receiver/   Binary wiring: env config, SIP + HTTP listeners
internal/siprecsrv/    The SRS: INVITE/ACK/BYE, SDP offer/answer, RS-Metadata
internal/rtp/          UDP RTP receiver (G.711 decode, loss tracking), port pool
internal/recording/    Streaming WAV writer (header patched on close)
internal/store/        SQLite store: calls, legs, users; SRS Finalizer; AMD merge
internal/voiceml/     VoiceML Call-record fetcher (Basic auth, Twilio JSON)
internal/auth/         Google OAuth, signed sessions, RequireAuth/RequireAdmin
internal/web/          UI handlers + embedded templates, notify endpoints
```

One process, two listener families:

- **SIP (per transport in `SIPREC_NETWORKS`)** — one listener per
  transport, all clones of one server family sharing a single session
  map, the RTP port pool, the recording directory, and the store —
  so a dialog that starts on UDP can send its BYE on TCP.
- **HTTP** — the UI, the OAuth flow, `/healthz`, and the notify
  endpoints on `HTTP_LISTEN`.

Concurrency: sipgo runs SIP transactions on its own goroutines; each
recording leg owns one RTP receive goroutine and one WAV writer
(single-writer by contract; write errors are captured on the leg and
surfaced at finalize). A background reaper finalizes idle sessions.
SQLite is opened with one connection in WAL mode, so finalize,
notify, and UI writes serialize cleanly.

### Data model (SQLite)

`calls` — one row per call, born from a recording, an AMD notification,
or the merge of both:

- Identity/correlation: `sip_call_id` (recording dialog), `call_sid`,
  `fs_uuid`, `account_sid`, `from_uri`, `from_number`, `to_number`,
  `direction`
- Recording: `started_at`, `ended_at`, `duration_sec`,
  `status` (`completed` | `notified`), `metadata_json`, `metadata_xml`
- AMD: `amd_answered_by`, `amd_duration_ms`, `amd_call_status`,
  `amd_notified_at`, `amd_raw`

`legs` — one row per recorded stream: `sip_call_id` (the SIPREC
dialog that recorded it), `label`, `codec`,
`participant_aor`, `wav_path`, `duration_sec`, `write_error`. Files
live in `RECORDING_DIR` under random names; deleting a call in the
UI deletes its rows and files.

`users` — the UI allowlist: `email` (primary key, normalized),
`role` (`admin` | `user`), `created_by`, `created_at`, `disabled`,
`google_sub` (pinned Google account ID), `session_epoch`
(revocation counter).

`audit_log` — administrative actions: `actor`, `action`, `target`,
`detail`, `created_at`.

`settings` — administrator-managed application settings:
`key`, `value`, `updated_at`. It stores the retention period and the
maintenance timer's last-purge result.

`voiceml_accounts` — per-account VoiceML API keys for on-request
record fetches: `account_sid` (primary key), `api_key`, `created_by`,
timestamps. Treat the database file as credential material.

Disposition columns on `calls`: `disp_status`, `disp_duration_sec`,
`disp_sip_code`, `disp_error_code`, `disp_error_message`,
`disp_notified_at`, `disp_raw`; fetched-record columns:
`voiceml_record_json`, `voiceml_fetched_at`.

## Development

```sh
make test    # go test ./...
make vet
make build
```

What the tests prove (no FreeSWITCH needed):

- `internal/siprecsrv` — full SIPREC dialogs over real SIP/TCP with a
  sipgo client: two-leg INVITE → WAVs → BYE → per-leg finalize;
  port-pool exhaustion answers 500 and rolls back; the mod_siprec
  lifecycle (INVITE → pause re-INVITE → resume re-INVITE → BYE)
  finalizes exactly once; mod_siprec-shaped metadata (group, base64
  session_id) parses and round-trips the FreeSWITCH UUID. Hardening:
  the 200 OK carries a Contact; a foreign-dialog BYE gets 481 and
  changes nothing; allowlist outsiders get 403; over-cap stream
  counts get 488; shutdown and the idle reaper finalize sessions;
  codec choice scans every offered payload type (telephone-event or
  dynamic PTs first); port-0 streams are answered port 0; re-INVITE
  answers bump the SDP `o=` version and answer inactive with
  inactive.
- `internal/rtp` — G.711 decode through the real receiver over UDP;
  RTP padding is stripped, not decoded; the receiver locks to the
  first (source, SSRC) and drops the rest; timestamp gaps become
  silence; ports recycle FIFO.
- `internal/store` — two legs merge into one call; AMD-after-recording
  updates; AMD-before-recording is adopted by `CallSid`; keyless AMD is
  rejected; a second dialog for the same `CallSid` joins the call;
  rows split across keys fold into one; search wildcards match
  literally; user CRUD; delete returns the WAV paths; pre-rename
  databases migrate (table/column renames) with data intact.
- `internal/web` / `internal/auth` — notify endpoint auth (401/400/200)
  and Twilio-form parsing; anonymous redirect; admin vs. user
  authorization (users get 403 on delete and user-admin, 200 on review
  and audio); audio paths outside `RECORDING_DIR` are refused; session
  tampering and CSRF failures are rejected; Google `sub` pinning
  (a different Google account for an allowlisted email is refused);
  epoch revocation kills outstanding sessions; hardening headers on
  every response; logout is POST-only; admin actions land in the
  audit log.
- `internal/store` (dispositions + fetched records) — disposition
  before/after the recording merges like AMD; a fetched VoiceML
  record denormalizes without overwriting recording fields.
- `internal/voiceml` + the fetch flow — Twilio-shaped Call JSON
  (string `duration`), per-account Basic auth, upstream errors reduced
  to a status (never echoing account SIDs), API keys never rendered
  back into HTML.

### Live interop test (real FreeSWITCH + mod_siprec)

`tests/interop/run.sh` places real sofia calls through FreeSWITCH with
`mod_siprec` recording to a freshly built siprec-receiver, then asserts
on the resulting DB rows and WAV files: the mixed pause/resume call
(≈8 s of tone, pause compressed out), the mid-call `siprec_stop` call
(≈2 s), separate-streams (two legs, 440 Hz vs 1000 Hz dominant), the
decoded FreeSWITCH UUIDs, and an AMD notification merging onto the
recording. It needs the `mod-siprec-load` image (build once with
`tests/load/run.sh` in the mod_siprec repo, ~30–45 min) and docker;
set `DOCKER="sudo docker"` if your user is not in the docker group.

## Deployment notes

- Run one instance per FreeSWITCH node (or centrally, if
  `SIPREC_LOCAL_IP` is reachable from every FreeSWITCH): there is no
  cross-instance state, and recordings land on the local disk + SQLite.
  Two processes never share a `RECORDING_DIR` or `DB_PATH`: each start
  takes an exclusive lock (`RECORDING_DIR/.siprec-receiver.lock` and
  `DB_PATH.lock`), and a second process (an overlapping blue/green
  start, say) exits at startup instead of adopting WAVs the first is
  still writing.
- Configure recording retention under **Admin → Settings** (or set
  the initial `RETENTION_DAYS` default) — recordings otherwise
  accumulate forever. The application maintenance timer runs at
  startup and every 15 minutes: it purges calls/recordings past the
  retention period, checkpoints the SQLite WAL, runs
  `PRAGMA optimize`, and vacuums when free pages exceed 20%. The
  settings page shows the last purge and database size and offers a
  manual purge. Audit-log entries are not retention-purged. A call
  (deleted by an administrator or purged) goes together with its WAV
  files: if a file cannot be removed the call is kept, whole, and the
  delete answers an error (the purge retries it on its next run). A
  file left behind without its row would otherwise come back as a
  recovered orphan at the next start.
- A `Dockerfile` is included (`docker build -t siprec-receiver .`);
  mount a volume at `/data` for the database + recordings. Container
  networking caveat: the SDP answer advertises `SIPREC_LOCAL_IP`, so
  the SRC must be able to route RTP to it — `--network host` is the
  simple shape on a FreeSWITCH node.
- Terminate TLS in front of `HTTP_LISTEN` (any reverse proxy) and set
  `PUBLIC_BASE_URL=https://…`. Keep the SIP listener firewalled to your
  FreeSWITCH hosts.
- Back up `DB_PATH` and `RECORDING_DIR` together — the database rows
  point at the WAV files by path.
- A minimal systemd unit:

```ini
[Unit]
Description=SIPREC recording server
After=network-online.target

[Service]
EnvironmentFile=/etc/siprec-receiver/env
ExecStart=/usr/local/bin/siprec-receiver
Restart=on-failure
User=siprec

[Install]
WantedBy=multi-user.target
```

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| INVITE answered 500 `no rtp ports available` | Port pool exhausted — too many concurrent recordings or BYEs not arriving; widen `RTP_PORT_MIN/MAX` |
| INVITE answered 403 | Source IP is outside `SIPREC_ALLOW_CIDRS` — add the FreeSWITCH host's CIDR |
| INVITE answered 503 `session limit` | `SIPREC_MAX_SESSIONS` concurrent sessions reached (or a flood — check the source) |
| INVITE answered 488 | More than 8 `m=audio` streams offered, or the `Warning` header names the problem: SRTP offered (302) or no PCMU/PCMA (305) |
| INVITE answered 400 `bad SDP` | SRC sent no SDP part, or a non-SIPREC INVITE reached the listener |
| BYE/re-INVITE answered 481 | In-dialog request from a different From-tag or source IP than the INVITE — the dialog check rejected it; the recording is untouched |
| Recording ends early; log says `reaping idle session` | No BYE arrived and no RTP flowed for `SIPREC_SESSION_IDLE_SEC` — the reaper finalized it (check the SRC/network for the lost BYE) |
| Recording has a WAV but it is silent/short, or is marked "no audio" | RTP never arrived: the SDP answer's `c=IN IP4` (`SIPREC_LOCAL_IP`) is not reachable from FreeSWITCH, or a firewall drops the UDP pool range |
| Recording never starts over UDP, works over TCP | Multipart body over the path MTU — switch the SRC entry to `transport=tcp` |
| Notify returns 401 | Wrong/missing secret (check Bearer value against `NOTIFY_SECRET`) |
| Notify returns 503 | `NOTIFY_SECRET` is not set — the endpoint disables itself rather than run open |
| Notify returns 400 `no correlation key` | The poster sent none of `CallSid`, `SipCallId`, `FsUuid` — add one (see correlation above) |
| AMD shows as its own row, never merges | The recording carries no `vm-callsid` and notifications carry no `SipCallId`/`FsUuid` — nothing links them; add a key on either side |
| Sign-in ends at “not authorized” | The Google email is neither `ADMIN_EMAIL` nor in the Users list — an administrator must add it |
| Everyone is signed out after restart | `SESSION_SECRET` unset (ephemeral) — set a stable value |
| Call shows ⚠ write error; audio stops mid-call | Disk full (or `RECORDING_DIR` unwritable) during recording — the first failed write's error is on the call page; free space and the next call records cleanly |
| A call from before a restart appears as `recovered-orphan` | Expected: the previous process died mid-recording and startup recovery adopted the WAV (see Wire behavior) |

## Contributing

Issues and PRs welcome. Two things to keep in mind:

1. **Wire compatibility is the product.** Changes to the SIP/SDP/
   metadata surface must keep the `internal/siprecsrv` dialog tests —
   including the mod_siprec pause/resume lifecycle — green, and should
   be checked against the RFC text, not against another implementation.
2. Run `make test` and `make vet` before submitting. New behavior in
   `store`, `auth`, and `web` needs tests in the same style as the
   existing ones (real SQLite in a temp dir, real HTTP via `httptest`).

## 🙌 Contributors

We welcome contributions! Thanks to these people:

- [Michael Mavroudis](https://github.com/mavroudis) - Lead Developer & Architect

## 💖 Sponsors

Proudly supported by:

| Sponsor | Contribution |
|---------|--------------|
| [VoiceTel Communications](http://www.voicetel.com) | Primary development and testing infrastructure |

## 📄 License

This project is licensed under the MIT License - see the [LICENSE](LICENSE)
file for details.

The SRS core derives from VoiceML's SIPREC receiver
(`internal/siprecsrv` and friends), also by VoiceTel Communications.
