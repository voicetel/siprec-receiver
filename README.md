# siprec-receiver

A standalone SIPREC Session Recording Server (SRS) — RFC 7866 / RFC 7865 —
compatible with FreeSWITCH `mod_siprec` and any other compliant Session
Recording Client (SRC). It:

- records every offered audio stream to a WAV file (G.711 PCMU/PCMA, 8 kHz),
- persists the recording and its RS-Metadata in SQLite,
- accepts AMD (answering-machine detection) call notifications keyed the
  callBroadcast way and logs the disposition against the call,
- serves a Google-OAuth-protected web UI: administrators manage recordings
  and users; authorized users review recordings.

Extracted from callBroadcast's production `internal/siprecsrv` (plus its
`internal/rtp` receiver and `internal/recording` WAV writer) and extended
with the store, notification endpoint, and UI described below.

## Quick start

```sh
cp .env.example .env   # edit ADMIN_EMAIL, SESSION_SECRET, NOTIFY_SECRET, ...
set -a; . ./.env; set +a
make run
```

Point your SRC at `sip:<host>:5070` (UDP, TCP, or TLS — see below), place a
recorded call, then open http://127.0.0.1:8080 and sign in.

For local development without a Google OAuth client, set
`DEV_AUTH_EMAIL=you@example.com` (and `ADMIN_EMAIL` to the same address):
`/auth/login` then signs you in directly. **Never set `DEV_AUTH_EMAIL` in
production** — it bypasses Google sign-in entirely and the server logs a
warning while it is set.

## Configuration

All configuration is environment variables.

| Variable | Default | Purpose |
|---|---|---|
| `SIPREC_LISTEN` | `127.0.0.1:5070` | SIP listen address for the SRS |
| `SIPREC_NETWORKS` | `udp,tcp` | SIP transports to listen on (comma list) |
| `SIPREC_LOCAL_IP` | `127.0.0.1` | IP advertised in the SDP answer (`c=IN IP4`) — must be reachable by the SRC's RTP fork |
| `SIP_TLS_LISTEN` | `127.0.0.1:5071` | TLS (SIPS) listen address, used only when cert+key are set |
| `SIP_TLS_CERT` / `SIP_TLS_KEY` | — | PEM files enabling the SIPS listener |
| `RTP_PORT_MIN` / `RTP_PORT_MAX` | `41000` / `42000` | UDP port pool for RTP receivers (even ports) |
| `RECORDING_DIR` | `./data/recordings` | Where WAV files are written |
| `DB_PATH` | `./data/siprec-receiver.db` | SQLite database path |
| `HTTP_LISTEN` | `127.0.0.1:8080` | Web UI + notify endpoint listen address |
| `PUBLIC_BASE_URL` | `http://127.0.0.1:8080` | Public base URL; OAuth redirect is `<base>/auth/callback` |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | — | Google OAuth client (console.cloud.google.com → Credentials) |
| `SESSION_SECRET` | ephemeral | HMAC secret for session cookies; set a stable random value in production |
| `ADMIN_EMAIL` | — | Bootstrap administrator email (always admin; managed via env, not the UI) |
| `NOTIFY_SECRET` | — (endpoint disabled) | Shared secret for `POST /api/notify/amd` |
| `DEV_AUTH_EMAIL` | — | **Dev only:** bypass Google and sign in as this address |

Put the HTTP listener behind a TLS-terminating reverse proxy in production
and set `PUBLIC_BASE_URL` to the public `https://` URL so session cookies
are marked `Secure`.

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

Compatibility notes (all covered by tests):

- Mixed mode (one `m=audio`, `a=label:1`) and separate-streams mode
  (two labelled streams) are both accepted; each stream becomes one WAV
  leg grouped under the call.
- Pause/resume re-INVITEs (`a=inactive` / `a=sendonly`, SDP only) keep the
  same ports and files — a pause never splits the recording (RFC 7866 §6.4).
- The SRS answers with plain `application/sdp` (never multipart), which
  mod_siprec's answer parser handles per-media.
- RS-Metadata `session_id` (the parent FreeSWITCH call UUID, base64 per
  RFC 7865 §6.9) is decoded and stored as the call's FreeSWITCH UUID.
- The SIP listener should only accept traffic from your FreeSWITCH hosts —
  firewall `SIPREC_LISTEN` accordingly; SIPREC has no in-band auth.

### Per-call correlation URI parameters

Whoever chooses the SRS URI can stamp correlation keys as URI parameters,
e.g. mod_siprec's per-call ad-hoc form:

```xml
<action application="siprec"
        data="myrec sip:srs.example.com:5070;cb-callsid=CA0123456789abcdef0123456789abcdef;cb-account=AC0123456789abcdef0123456789abcdef"/>
```

`cb-callsid` (CallSid) and `cb-account` (AccountSid) are stored on the call
and let AMD notifications merge automatically (see below).

## AMD call notifications

`POST /api/notify/amd` logs the AMD disposition for a call. It accepts the
exact field names callBroadcast POSTs to an `AsyncAmdStatusCallback` URL —
form-encoded or JSON:

| Field | Notes |
|---|---|
| `AnsweredBy` | **Required.** `human`, `machine_start`, `machine_end_beep`, `machine_end_silence`, `machine_end_other`, `fax`, `unknown` (extended: `screened`, `screened_ivr`, `disconnected` are also stored) |
| `CallSid` | Correlation key (matches a recording stamped with `cb-callsid`) |
| `MachineDetectionDuration` | Milliseconds |
| `AccountSid`, `From`, `To`, `CallStatus`, `Direction` | Stored; `Caller`/`Called` accepted as `From`/`To` synonyms |
| `SipCallId` | The SIPREC recording dialog's SIP Call-ID — direct match |
| `FsUuid` | The FreeSWITCH parent call UUID — matches the decoded metadata `session_id` |

Auth is the `NOTIFY_SECRET` shared secret: `Authorization: Bearer <secret>`
(or `X-Notify-Secret`, or `?secret=` for webhook senders that cannot set
headers). With no secret configured the endpoint answers 503.

Notifications are upserts: a disposition that arrives before its recording
creates an AMD-only row that the recording later adopts (matched by
`CallSid` or `FsUuid`); one that arrives after updates the recording's row.
Either way the disposition is visible in the UI immediately.

```sh
curl -X POST https://srs.example.com/api/notify/amd \
  -H "Authorization: Bearer $NOTIFY_SECRET" \
  --data-urlencode "CallSid=CA0123456789abcdef0123456789abcdef" \
  --data-urlencode "AnsweredBy=machine_end_beep" \
  --data-urlencode "MachineDetectionDuration=3100" \
  --data-urlencode "SipCallId=abc123@fs.example.com"
# {"id":1,"matched":true}
```

## Web UI and roles

- Sign-in is Google OAuth (`openid email profile`); the ID token is
  verified and the email must be authorized.
- **Administrator** — the `ADMIN_EMAIL` address (always admin, cannot be
  removed in the UI), plus any user whose role is set to `admin`. Admins
  can delete recordings and add/remove/disable users under **Users**.
- **User** — anyone an administrator adds. Users can list, search, play,
  and download recordings, but cannot delete recordings or manage users.

Roles are resolved on every request, so removing or disabling a user takes
effect immediately. Recordings can be filtered by number/SID/UUID text and
by AMD disposition; each call's page shows its metadata, the raw
RS-Metadata XML and AMD payload, and per-leg audio players.

## Development

```sh
make test    # go test ./...
make vet
make build   # ./siprec-receiver
```

Layout:

```
cmd/siprec-receiver/   binary wiring (env config, listeners)
internal/siprecsrv/    SIPREC SRS: INVITE/BYE handling, SDP, RS-Metadata
internal/rtp/          RTP receiver + UDP port pool (G.711 decode)
internal/recording/    streaming WAV writer
internal/store/        SQLite: calls, legs, users (+ SRS Finalizer)
internal/auth/         Google OAuth, sessions, admin/user middleware
internal/web/          UI handlers, templates, AMD notify endpoint
```

## License

MIT + Commons Clause, as in LICENSE (this project derives from
callBroadcast, © VoiceTel Communications).
