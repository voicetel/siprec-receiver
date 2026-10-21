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
`internal/recording` WAV writer) is extracted from callBroadcast's
production SIPREC receiver; the SQLite store, the AMD notification
endpoint, and the web UI are new in this repository.

## Status

| Concept | Spec | Status |
|---|---|---|
| SIPREC INVITE with multipart SDP + RS-Metadata | [RFC 7866 §6.1.2][rfc7866-6.1] / [RFC 2046][rfc2046] | ✅ SDP part required; metadata part optional (recording proceeds with positional leg labels) |
| `Require: siprec` / `+sip.src` handling | [RFC 7866 §6.1][rfc7866-6.1] | ✅ Accepted (no special handling needed server-side) |
| Answer shape | [RFC 7866 §7][rfc7866-7] | ✅ Plain `application/sdp` 200 OK (never multipart), one `recvonly` `m=audio` per offered stream, `a=label` echoed |
| Mixed (single-stream) recording | [RFC 7866 §7][rfc7866-7] | ✅ One WAV leg per call (both directions already mixed by the SRC) |
| Separate RX/TX streams | [RFC 7866 §7][rfc7866-7] / [§8.5][rfc7866-8.5] | ✅ Two WAV legs per call, labelled from RS-Metadata participants |
| Pause / resume re-INVITE | [RFC 7866 §6.4][rfc7866-6.4] | ✅ `a=inactive` / `a=sendonly` re-INVITEs re-answer on the same ports and keep writing the same files — a pause never splits the recording |
| BYE teardown | [RFC 7866 §6.4][rfc7866-6.4] | ✅ Closes WAVs, releases RTP ports, finalizes the call row |
| Re-INVITE replacing media shape | [RFC 7866 §6.4][rfc7866-6.4] | ✅ Prior session is finalized, a fresh one is recorded |
| `<recording>` / `<datamode>` / `<group>` / `<session>` | [RFC 7865 App. A][rfc7865] | ✅ Parsed, persisted (raw XML + normalized JSON) |
| `<participant>` AORs (`<nameID aor>`) | [RFC 7865 App. A][rfc7865] | ✅ Caller/callee numbers derived from participants; used as leg labels |
| Metadata IDs (`session_id` = parent call UUID, base64) | [RFC 7865 §6.9][rfc7865] | ✅ Decoded back to the FreeSWITCH call UUID and stored for correlation |
| `<stream>` + `<participantstreamassoc>` label mapping | [RFC 7865 App. A][rfc7865] | ✅ Stream → sending participant resolution for leg naming |
| G.711 PCMU / PCMA, 8 kHz | [RFC 3551](https://datatracker.ietf.org/doc/html/rfc3551) | ✅ The only offered codecs; WAV is 16-bit mono PCM at 8 kHz |
| RTP loss accounting | [RFC 3550 §5.1][rfc3550] | ✅ Sequence-gap tracking in the receiver |
| Transports | [RFC 7866 §11.3][rfc7866-11.3] | ✅ UDP + TCP; ✅ SIPS/TLS when `SIP_TLS_CERT`/`SIP_TLS_KEY` are set |
| SRTP recording media | [RFC 7866 §11.2][rfc7866-11.2] | ❌ Not supported — answers are `RTP/AVP` only (matches mod_siprec, which cannot offer SDES) |
| IPv6 | [RFC 4566 §5.7][rfc4566] | ❌ SDP answers advertise IPv4 (`c=IN IP4`); run the SRS on IPv4 |

[rfc7866-6.1]: https://datatracker.ietf.org/doc/html/rfc7866#section-6.1
[rfc7866-6.4]: https://datatracker.ietf.org/doc/html/rfc7866#section-6.4
[rfc7866-7]: https://datatracker.ietf.org/doc/html/rfc7866#section-7
[rfc7866-8.5]: https://datatracker.ietf.org/doc/html/rfc7866#section-8.5
[rfc7866-11.2]: https://datatracker.ietf.org/doc/html/rfc7866#section-11.2
[rfc7866-11.3]: https://datatracker.ietf.org/doc/html/rfc7866#section-11.3

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

| Variable | Default | Purpose |
|---|---|---|
| `SIPREC_LISTEN` | `127.0.0.1:5070` | SIP listen address for the SRS |
| `SIPREC_NETWORKS` | `udp,tcp` | SIP transports to listen on (comma list) |
| `SIPREC_LOCAL_IP` | `127.0.0.1` | IP advertised in the SDP answer (`c=IN IP4`) — must be reachable by the SRC's RTP fork |
| `SIP_TLS_LISTEN` | `127.0.0.1:5071` | TLS (SIPS) listen address, used only when cert+key are set |
| `SIP_TLS_CERT` / `SIP_TLS_KEY` | — | PEM files enabling the SIPS listener |
| `RTP_PORT_MIN` / `RTP_PORT_MAX` | `41000` / `42000` | UDP port pool for RTP receivers (even ports only, per RTP convention) |
| `RECORDING_DIR` | `./data/recordings` | Where WAV files are written (`0600` permissions) |
| `DB_PATH` | `./data/siprec-receiver.db` | SQLite database path (WAL mode) |
| `HTTP_LISTEN` | `127.0.0.1:8080` | Web UI + notify endpoint listen address |
| `PUBLIC_BASE_URL` | `http://127.0.0.1:8080` | Public base URL; the OAuth redirect is `<base>/auth/callback` |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | — | Google OAuth client (see below) |
| `SESSION_SECRET` | ephemeral | HMAC secret for session cookies; set a stable random value in production or every restart signs everyone out |
| `ADMIN_EMAIL` | — | Bootstrap administrator email (always admin; managed via env, not the UI) |
| `NOTIFY_SECRET` | — (endpoint disabled) | Shared secret for `POST /api/notify/amd` |
| `RETENTION_DAYS` | `0` (disabled) | Delete calls (rows + WAV files) that started more than this many days ago; swept at startup, then daily |
| `CALLBROADCAST_BASE_URL` | — (fetches disabled) | Base URL of the callBroadcast deployment for on-request Call-record fetches (per-account keys live in the UI, see Integrations) |
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
        data="myrec sip:srs.example.com:5070;cb-callsid=CA0123456789abcdef0123456789abcdef;cb-account=AC0123456789abcdef0123456789abcdef"/>
```

| Parameter | Stored as | Purpose |
|---|---|---|
| `cb-callsid` | `call_sid` | Lets AMD notifications (keyed by CallSid) merge with this recording automatically |
| `cb-account` | `account_sid` | Tenant/account attribution, shown in the UI |

## Wire behavior

- **INVITE:** the body must contain an SDP part; an RS-Metadata part is
  used for naming/correlation when present and ignored (with positional
  leg labels) when absent or malformed. A body with no SDP is a 400.
- **200 OK:** always plain `application/sdp` — one `m=audio` per offered
  stream with an SRS-pool port, the chosen G.711 payload type,
  `a=recvonly`, and the offer's `a=label` echoed. (Some SRC answer-side
  parsers do not extract SDP from a multipart 200; plain SDP is the
  interoperable choice.)
- **Recorded files:** `<RECORDING_DIR>/<sanitised SIP Call-ID>-<label>.wav`,
  where `<label>` is the sending participant's number from RS-Metadata
  when resolvable (`…-12165285911.wav`), else the SDP label, else
  `leg<N>`. WAV files are written `0600`.
- **Pause semantics:** while a recording is paused the SRC forks no
  RTP, so paused wall-clock time is *compressed out* of the WAV — the
  file (and `duration_sec`, which counts recorded samples) contains
  only the audio that was actually sent. A 4 s + 3 s-pause + 4 s call
  yields an ≈8 s recording, not 11 s.
- **Write failures are loud, never silent:** the first WAV write error
  on a leg (the classic case is a full disk) is logged immediately as
  `WAV WRITE FAILED`, stored on the leg, and shown in the UI (a
  ⚠ badge in the list, the error text on the call page). The audio
  after the failure point is lost, but the record says so.
- **Ports:** one even UDP port per stream from the `RTP_PORT_MIN`–
  `RTP_PORT_MAX` pool, released on BYE. Pool exhaustion answers 500 and
  records nothing.
- **Dialog end:** BYE closes the WAVs and finalizes the call within a
  second; a dialog that never sends BYE keeps its files open (watch for
  SRC crashes stranding ports — the pool size bounds concurrent calls).
- **Crash recovery:** if the SRS itself dies mid-recording, the WAVs
  on disk are adopted at the next startup: their headers are repaired
  in place (a crashed file still carries the writer's placeholder
  sizes), durations are recomputed from the audio present, and each
  becomes a normal `completed` call marked `recovered-orphan` in the
  direction field, started at the file's modification time. Files that
  are not playable WAVs are logged and left alone.

## AMD call notifications

`POST /api/notify/amd` logs the AMD disposition for a call. It exists so
the answering-machine verdict produced during dialing (by callBroadcast,
a dialer, or any AMD engine) ends up on the same record as the audio.

It accepts the exact field names callBroadcast POSTs to an
`AsyncAmdStatusCallback` URL, as form-encoding (the webhook shape) or
JSON:

| Field | Notes |
|---|---|
| `AnsweredBy` | **Required.** `human`, `machine_start`, `machine_end_beep`, `machine_end_silence`, `machine_end_other`, `fax`, `unknown`. callBroadcast's extended `screened`, `screened_ivr`, `disconnected` are also stored; other values are stored verbatim (a logging endpoint must not drop dispositions) |
| `CallSid` | Correlation key: matches a recording stamped `cb-callsid` |
| `MachineDetectionDuration` | Milliseconds to verdict |
| `AccountSid`, `From`, `To`, `CallStatus`, `Direction` | Stored with the disposition; `Caller`/`Called` are accepted as `From`/`To` synonyms |
| `SipCallId` | The SIPREC recording dialog's SIP Call-ID — direct match |
| `FsUuid` | The FreeSWITCH parent call UUID — matches the decoded metadata `session_id` |

A request with none of `CallSid`, `SipCallId`, `FsUuid` cannot be
correlated and is rejected with 400.

Auth is the `NOTIFY_SECRET` shared secret, presented as
`Authorization: Bearer <secret>` (or the `X-Notify-Secret` header, or
`?secret=` for webhook senders that cannot set headers — callBroadcast's
sender is one). With no secret configured the endpoint answers 503
rather than running open.

Notifications are upserts, order-independent:

- AMD **after** the recording: the recording's row gains the disposition.
- AMD **before** the recording (or with no recording at all): an
  AMD-only row (`status=notified`) is created and shown in the UI
  immediately; when the recording later arrives carrying the same
  `CallSid` or FreeSWITCH UUID, it adopts the disposition and the
  placeholder row disappears.

```sh
# Form-encoded (the callBroadcast/Twilio webhook shape)
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
secret · 503 endpoint disabled. Bodies are capped at 64 KB; the raw
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
| `CallDuration` | Billable call seconds (kept separate from the recording's audio duration); `Duration` accepted |
| `SipResponseCode` | Final SIP response code, when the poster knows it |
| `ErrorCode` / `ErrorMessage` | Failure detail |
| `AnsweredBy` | Terminal callbacks repeat the AMD verdict; fills the AMD fields when still empty |
| `CallSid`, `AccountSid`, `From`, `To`, `Direction`, `SipCallId`, `FsUuid` | Same meaning as the AMD endpoint |

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

## Full call record from callBroadcast (on request)

A recording row can be enriched with the complete calling record from
callBroadcast — final status, billable duration, price, timestamps,
`answered_by`, everything in the Call resource. Nothing is fetched
automatically: a signed-in user clicks **Fetch full record from
callBroadcast** on the call page (`POST /recordings/{id}/fetch-record`),
the receiver GETs
`/2010-04-01/Accounts/{AccountSid}/Calls/{CallSid}.json` with the
account's API key as Basic auth, stores the pretty-printed JSON on the
call, and fills any still-empty identity/disposition fields from it
(recording-owned fields are never overwritten).

Because callBroadcast keys are per-account, an administrator stores
one key per AccountSid under **Integrations** in the UI (with
`CALLBROADCAST_BASE_URL` set in the environment). Keys are write-only
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
| Fetch/refresh the full call record from callBroadcast | ✅ | ✅ |
| Delete a recording (row + WAV files) | ✅ | ❌ (403) |
| Add/remove users, grant the admin role | ✅ | ❌ (403) |
| Store/remove per-account callBroadcast API keys | ✅ | ❌ (403) |

- **Administrator** — the `ADMIN_EMAIL` bootstrap address (always admin,
  cannot be removed in the UI), plus any added user whose role is
  `admin`.
- **User** — anyone an administrator adds under **Users** with the
  `user` role. Users review recordings; they cannot change anything.

The recordings list filters by free text (number, CallSid, SIP
Call-ID, FreeSWITCH UUID) and by AMD disposition, 50 calls per page.
Each call's page shows every stored field, one audio player + download
link per leg, and collapsible raw AMD/metadata payloads. Anonymous
requests redirect to the sign-in page; `GET /healthz` is the only
unauthenticated route besides the OAuth flow and the secret-gated
notify endpoint.

## Architecture

```
cmd/siprec-receiver/   Binary wiring: env config, SIP + HTTP listeners
internal/siprecsrv/    The SRS: INVITE/ACK/BYE, SDP offer/answer, RS-Metadata
internal/rtp/          UDP RTP receiver (G.711 decode, loss tracking), port pool
internal/recording/    Streaming WAV writer (header patched on close)
internal/store/        SQLite store: calls, legs, users; SRS Finalizer; AMD merge
internal/cbclient/     callBroadcast Call-record fetcher (Basic auth, Twilio JSON)
internal/auth/         Google OAuth, signed sessions, RequireAuth/RequireAdmin
internal/web/          UI handlers + embedded templates, notify endpoints
```

One process, two listener families:

- **SIP (per transport in `SIPREC_NETWORKS`)** — one `siprecsrv.Server`
  per transport, sharing the RTP port pool, the recording directory,
  and the store. A SIPREC dialog stays on its starting transport, so
  the per-instance session maps never need to agree.
- **HTTP** — the UI, the OAuth flow, `/healthz`, and
  `/api/notify/amd` on `HTTP_LISTEN`.

Concurrency: sipgo runs SIP transactions on its own goroutines; each
recording leg owns one RTP receive goroutine and one WAV writer
(single-writer by contract). SQLite is opened with one connection in
WAL mode, so finalize, notify, and UI writes serialize cleanly.

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

`legs` — one row per recorded stream: `label`, `codec`,
`participant_aor`, `wav_path`, `duration_sec`. Files live in
`RECORDING_DIR`; deleting a call in the UI deletes its rows and files.

`users` — the UI allowlist: `email` (primary key, normalized),
`role` (`admin` | `user`), `created_by`, `created_at`, `disabled`.

`cb_accounts` — per-account callBroadcast API keys for on-request
record fetches: `account_sid` (primary key), `api_key`, `created_by`,
timestamps. Treat the database file as credential material.

Disposition columns on `calls`: `disp_status`, `disp_duration_sec`,
`disp_sip_code`, `disp_error_code`, `disp_error_message`,
`disp_notified_at`, `disp_raw`; fetched-record columns:
`cb_record_json`, `cb_fetched_at`.

## Development

```sh
make test    # go test ./...
make vet
make build
```

What the tests prove (no FreeSWITCH needed):

- `internal/siprecsrv` — full SIPREC dialogs over real SIP/TCP with a
  sipgo client: two-leg INVITE → named WAVs → BYE → per-leg finalize;
  port-pool exhaustion answers 500 and rolls back; the mod_siprec
  lifecycle (INVITE → pause re-INVITE → resume re-INVITE → BYE)
  finalizes exactly once; mod_siprec-shaped metadata (group, base64
  session_id) parses and round-trips the FreeSWITCH UUID.
- `internal/store` — two legs merge into one call; AMD-after-recording
  updates; AMD-before-recording is adopted by `CallSid`; keyless AMD is
  rejected; user CRUD; delete returns the WAV paths.
- `internal/web` / `internal/auth` — notify endpoint auth (401/400/200)
  and Twilio-form parsing; anonymous redirect; admin vs. user
  authorization (users get 403 on delete and user-admin, 200 on review
  and audio); audio paths outside `RECORDING_DIR` are refused; session
  tampering and CSRF failures are rejected.
- `internal/store` (dispositions + fetched records) — disposition
  before/after the recording merges like AMD; a fetched callBroadcast
  record denormalizes without overwriting recording fields.
- `internal/cbclient` + the fetch flow — Twilio-shaped Call JSON
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
- Set `RETENTION_DAYS` (or your own external rotation) — recordings
  otherwise accumulate forever. The built-in sweep deletes rows and
  WAV files together at startup and then daily.
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
| INVITE answered 400 `bad SDP` | SRC sent no SDP part, or a non-SIPREC INVITE reached the listener |
| Recording has a WAV but it is silent/short | RTP never arrived: the SDP answer's `c=IN IP4` (`SIPREC_LOCAL_IP`) is not reachable from FreeSWITCH, or a firewall drops the UDP pool range |
| Recording never starts over UDP, works over TCP | Multipart body over the path MTU — switch the SRC entry to `transport=tcp` |
| Notify returns 401 | Wrong/missing secret (check Bearer value against `NOTIFY_SECRET`) |
| Notify returns 503 | `NOTIFY_SECRET` is not set — the endpoint disables itself rather than run open |
| Notify returns 400 `no correlation key` | The poster sent none of `CallSid`, `SipCallId`, `FsUuid` — add one (see correlation above) |
| AMD shows as its own row, never merges | The recording carries no `cb-callsid` and notifications carry no `SipCallId`/`FsUuid` — nothing links them; add a key on either side |
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

The SRS core derives from callBroadcast's SIPREC receiver
(`internal/siprecsrv` and friends), also by VoiceTel Communications.
