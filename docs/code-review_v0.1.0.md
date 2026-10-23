# Code review: v0.1.0

**Date:** 2026-10-21

**Scope:** The review was done against the uncommitted working tree, before the repository had any commits:

- `cmd/siprec-receiver`
- `internal/{auth,recording,rtp,siprecsrv,store,web}`
- the web templates

**How this relates to the `v0.1.0` tag:** The tag was created later and points at `cfdeb0c`. That tree is **newer** than the one reviewed here. It adds tests for `auth`, `recording`, `rtp`, and `web`, and it fixes finding 1. The findings below were **not** rechecked against the tag, apart from the ones marked "Status at tag", so treat each one as "present in the reviewed tree; confirm at the tag before acting".

**Names:** Package, parameter, and table names below are the current ones (`internal/voiceml`, the `callsid` / `accountsid` URI parameters). The reviewed tree and the tag used earlier names for the same things.

**Checks run on the reviewed tree:**

| Check | Result |
| --- | --- |
| `go build ./...` | Clean |
| `go vet ./...` | Clean |
| `go test -race -count=1 ./...` | Pass. Only `siprecsrv` and `store` had tests in the reviewed tree. |
| `golangci-lint run ./...` | 2 issues: `decodePCMU` and `decodePCMA` are unused |
| `govulncheck ./...` with Go 1.26.8 | 13 reachable standard-library vulnerabilities (see A14) |

**Later checks on the working tree** (2026-10-21, after the tag, with uncommitted changes from other work present):

| Check | Result |
| --- | --- |
| `govulncheck ./...` with Go 1.26.9 | 1 reachable vulnerability: GO-2026-6809, fixed in 1.26.10 |
| `govulncheck ./...` with Go 1.26.10 | No reachable vulnerabilities (exit 0) |
| `govulncheck ./...` with Go 1.27.3 (current `stable`) | No reachable vulnerabilities (exit 0); `go build ./...` passes |
| `gosec ./...` | 25 advisory findings: G115 integer conversion ×5, G124 cookie attributes ×3, G304 file path from a variable ×3, G301 directory mode ×2, G706 log injection ×12 |
| `go test -race -cover ./...` | Pass. Total coverage 62.6%. |

In both govulncheck runs that exited 0, it also reported one vulnerability in a required module that the code doesn't call.

## Summary

The SIPREC handling has a sensible structure and the store's merge logic is well thought out. One bug breaks a core feature: form-encoded AMD notifications lose all their fields. Several other issues lose recordings without any error reaching the user, and both the SIP and RTP ports are open to untrusted senders. The must-fix items below should be resolved before tagging v0.1.0.

---

## Must fix before v0.1.0

### 1. Form-encoded AMD notifications lose all their fields

`internal/web/notify.go`

`handleNotifyAMD` reads the whole request body with `io.ReadAll(http.MaxBytesReader(...))`. Then `parseNotifyBody` calls `r.ParseForm()`, which tries to read `r.Body` again. The body is already used up, so `r.PostForm` is always empty.

Reproduced: with the body `CallSid=CA1&AnsweredBy=human`, `PostForm` came back as `map[]`.

**Impact:** Every form-encoded webhook (the VoiceML/Twilio style, which is the main use case) gets `400 AnsweredBy is required`. The only exception is when the fields are also in the query string. JSON bodies work. The stored `Raw` value (`r.PostForm.Encode()`) is also empty.

**Fix:** Parse the bytes that were already read with `url.ParseQuery(string(body))`, and store `string(body)` as `Raw`. Add a test in `internal/web` that covers both form and JSON bodies.

**Status at tag:** Fixed. At `v0.1.0`, `parseNotifyBody` parses the body bytes with `url.ParseQuery` (confirmed with `git show v0.1.0:internal/web/notify.go`). I haven't checked whether a test covers form-encoded bodies.

### 2. Shutdown discards active recordings

`cmd/siprec-receiver/main.go`

On SIGTERM or SIGINT, only the HTTP server is shut down.

- Active SIPREC sessions are never finalized. Their WAV headers keep zero data and RIFF sizes, so many players reject the files, and no database row is written.
- Nothing waits for the SIP serve goroutines. The deferred `st.Close()` can run while a finalizer is still using the store.

**Fix:** Add `Server.Shutdown(ctx)` (or a drain step) that finalizes every session. Track the serve goroutines with a `sync.WaitGroup`, and close the store only after they have finished.

### 3. Sessions that never get a BYE are never cleaned up

`internal/siprecsrv/server.go`

If the recording client crashes, the network drops, or a BYE is lost, the session stays in `s.sessions` forever. It keeps its RTP ports and an open file descriptor, and its WAV header is never completed.

**Fix:** Add a reaper that finalizes a session after a period with no RTP (for example 5 minutes, configurable) and also enforces a maximum session length.

### 4. The same WAV path can be reused, which silently overwrites audio

`internal/siprecsrv/server.go` (`onInvite`, `finalizeSession`)

The WAV path is `<sanitisedCallID>-<label>.wav`, opened with `O_TRUNC`. Two things can reuse a path:

- **Re-INVITE that changes the media shape.** The old session is finalized, then the new legs open the same filenames and truncate the finished first segment. The `legs` upsert on `(call_id, wav_path)` then overwrites the old row, so nothing records that the audio was lost.
- **Two streams with the same label.** This happens if both streams resolve to the same participant userpart, or the offer repeats an `a=label`. Both writers then write to one file.

**Fix:** Add a per-session part to the filename, such as a session start timestamp or a segment counter, and de-duplicate labels within a session (`label`, `label-2`, and so on).

### 5. A second recording session for the same CallSid fails to save

`internal/store/store.go` (`RecordLeg`, `ApplyAMD`)

The `INSERT ... ON CONFLICT(sip_call_id) DO UPDATE` in `RecordLeg` also inserts `call_sid`. Suppose a row already holds that CallSid under a different SIP Call-ID, for example after re-recording following a transfer or a mod_siprec restart for the same call. The insert then violates `UNIQUE(call_sid)`, which the `ON CONFLICT` clause doesn't cover, and the whole transaction fails. The finalizer only logs the error, so the WAV stays on disk with no row and never appears in the UI.

`ApplyAMD` can fail the same way. Its update sets `sip_call_id = COALESCE(sip_call_id, ?)` on a row found by CallSid, which fails when another row already owns that SIP Call-ID. `ApplyAMD` also does its lookup and its write outside a transaction, so two concurrent notifications for a new CallSid can both try to insert, and one fails with a 500.

**Fix:** Decide what the data model should be when one CallSid has several SIPREC sessions. Either attach the later sessions' legs to the existing call row, or drop `UNIQUE` on `call_sid`. Then make both paths transactional and handle the conflict explicitly.

### 6. The 200 OK has no Contact header

`internal/siprecsrv/server.go`

RFC 3261 §12.1.1 requires a Contact header in a 2xx response to INVITE. sipgo's `sip.NewResponseFromRequest` adds a To-tag but not a Contact, and the handler doesn't add one either. Strict recording clients can't set up the dialog's remote target without it, so in-dialog BYE and re-INVITE may never arrive. This combines badly with item 3. The current mod_siprec tests don't check for it.

**Fix:** Add `Contact: <sip:srs@LocalIP:port;transport=...>` to the initial 200 OK and to re-INVITE 200 OK responses.

---

## Security

### SIP has no access control

Anyone who can reach the SIP listener can start recordings. A single INVITE with many `m=audio` lines can use up the whole RTP port pool (about 500 ports with the default 41000–42000 range), and repeated INVITEs can fill the disk.

**Fix:**

- Add a source-IP or CIDR allowlist for SIP.
- Cap the number of `m=audio` lines per INVITE (2 is enough for SIPREC).
- Cap the number of concurrent sessions.
- Consider a disk-space check before accepting a session.

### RTP listens on every interface and accepts any sender

`internal/rtp/receiver.go`

`NewReceiver` binds `0.0.0.0` even though the SDP answer advertises `SIPREC_LOCAL_IP` (default `127.0.0.1`). Packets are accepted from any source address and any SSRC, so anyone who can reach the port can inject audio into a live recording. Packets from several SSRCs are also interleaved into one file.

**Fix:** Bind to the advertised local IP. Lock each leg to the first source address and SSRC it receives, or check the source against the SDP offer's connection address.

### The dev auth bypass has no safeguard

`internal/auth/auth.go`

When `DEV_AUTH_EMAIL` is set, any visitor to `/auth/login` gets a session for that email. If it equals `ADMIN_EMAIL` (or names an admin user), every visitor is an administrator. The only protection is a startup log warning.

**Fix:** Refuse to start with `DEV_AUTH_EMAIL` set unless `HTTP_LISTEN` is a loopback address and `PUBLIC_BASE_URL` is not `https://`.

### The notify secret can be passed in the URL

`internal/web/notify.go`

Accepting the secret as `?secret=` means it gets written to reverse-proxy and access logs. This is documented as a compromise for webhook senders that can't set headers. Note the risk in the README, and log a one-time warning when the query form is used.

### Lower-severity web hardening

- **Logout is a GET** (`GET /auth/logout`), so another site can log a user out.
- **No `X-Frame-Options` or `Content-Security-Policy: frame-ancestors 'none'` headers.** The delete buttons could be clickjacked.
- **The `msg` query parameter is shown on the page.** It is escaped, so it isn't an XSS risk, but anyone can craft a link that displays their own message.
- **The OAuth flow doesn't use PKCE** (`oauth2.GenerateVerifier`), and the state cookie isn't cleared after the callback.
- **`SESSION_SECRET` has no minimum length.** Enforce at least 32 bytes.
- **The HTTP server only sets `ReadHeaderTimeout`.** Add `IdleTimeout`. A `ReadTimeout` is also fine. A `WriteTimeout` would cut off long audio downloads, so use `http.ResponseController` per request instead.

### No tenant scoping

Every signed-in user can see every recording, even though `AccountSid` is stored. If that's intended for v0.1.0, say so in the docs. If not, add per-user account scoping.

---

## Correctness (medium)

### Concurrent requests can leak sessions

`onInvite` checks `s.sessions[callID]`, unlocks, builds the legs, and later writes `s.sessions[callID] = sess` without checking again.

- **Two concurrent INVITEs for the same Call-ID:** the second overwrites the first. The first session's ports, receivers, and open files are never released.
- **A BYE that arrives while the INVITE is still being set up:** the BYE is answered as "unknown", the session is stored afterwards, and it never ends (see item 3).

**Fix:** Reserve the Call-ID in the map (for example with a `pending` state) before allocating anything, and have the BYE handler deal with pending sessions.

### Each transport has its own session map

`main.go` creates one `siprecsrv.Server` per transport, each with its own map. The comment says a dialog stays on the transport it started on, but that isn't guaranteed. RFC 3261 §18.1.1 requires requests over 1300 bytes to move from UDP to TCP, and SIPREC INVITEs that carry metadata often exceed that, while the BYE can come back over UDP. A BYE on another transport is answered as "unknown" and the session leaks.

**Fix:** Use one `Server` (one session map) with several listeners, or share the map between instances.

### Codec selection only looks at the first payload type

`internal/siprecsrv/sdp.go`

`ParseSDP` records only `parts[3]`, the first payload type on the `m=` line. Offers such as `m=audio 4000 RTP/AVP 101 0` or `RTP/AVP 96 8` are rejected with a 500, even though PCMU or PCMA is on offer.

**Fix:** Parse every payload type on the `m=` line and choose the first PCMU or PCMA one.

### SDP answer doesn't follow RFC 3264

- **Rejected streams still get a recorder.** Offered streams with port 0 are allocated a port; they should be answered with port 0 and skipped.
- **Wrong direction for inactive streams.** An `a=inactive` offer is answered `a=recvonly`; RFC 3264 §6.1 requires `inactive`.
- **The `o=` version never changes.** It is always `0 0`. RFC 3264 §8 says that when SDP is reissued in the same session, the version MUST increase unless the SDP is identical. That section is written in terms of offers, and how it applies to answers isn't checked here. The current re-INVITE answers are byte-identical (always `recvonly`, same ports), so this isn't a violation today. It becomes one as soon as an answer's content changes, for example after the `inactive` fix above. Track the version per session and increment it whenever the answer changes.
- **Media-level `c=` lines are ignored.** This doesn't matter now because the connection IP isn't used, but it will once source checking is added.

### The RTP receiver ignores timing

`internal/rtp/receiver.go`

Packets are appended to the WAV in the order they arrive, with no use of the RTP timestamp.

- Lost packets and silence-suppression gaps make the audio shorter, so reported durations are too low and the two legs drift out of sync.
- Packets that arrive late or out of order are written out of order.
- The padding bit is ignored, so padding bytes are decoded as audio samples.
- The loss counter wraps when a packet arrives out of order (`uint16` subtraction), adding about 65,535 to `PacketsLost`.

**Fix:** Use the RTP timestamp to insert silence for gaps, drop packets that arrive too late, strip padding, and use a signed sequence difference.

### Smaller correctness items

- **Bad metadata on re-INVITE replaces good metadata.** If the metadata fails to parse, `rec` becomes an empty `&Recording{}`, and it still replaces `prior.metadata` because `metadata != ""`.
- **WAV write errors are ignored.** The RTP callback discards errors from `wr.Append`, so a full disk silently drops audio. Log the first error for each leg.
- **Multipart errors are treated as the end of the body.** `extractSDPAndMetadata` stops on any `NextPart` error, so a malformed body is accepted as long as an SDP part came first. Only stop on `io.EOF`. The hand-written read loop can be replaced with `io.ReadAll(part)`.
- **Database writes delay the BYE response.** `onBye` finalizes (writing to the database) before sending the 200 OK, so a slow database causes BYE retransmissions. Reply first, or finalize asynchronously and track that with the shutdown `WaitGroup`.
- **`fs_uuid` lookups can match several rows.** `ApplyAMD` and `mergePlaceholder` don't define which one is picked.
- **Very long Call-IDs can't be saved.** An unbounded Call-ID can make the filename exceed `NAME_MAX`, which causes a `500 wav open failed`. Truncate or hash long Call-IDs.

---

## Maintainability

- **Stale documentation copied from the upstream VoiceML codebase.** Package and function comments describe behavior this repo doesn't have:
  - S3 uploads and `internal/recording.Uploader`
  - a command directory that doesn't exist here (since removed)
  - OpenSIPS config paths in another repository (since removed)
  - Prometheus metrics, including an RTP port bind-failure counter (the counter reference has since been removed)
  - issue "#94"
  - the "AMD engine" in `recording`

  `main.go` also refers to a `README.md` that doesn't exist yet.
- **Unused code:**
  - `decodePCMU` and `decodePCMA` (flagged by the linter)
  - `BuildMetadataResponse`
  - the ignored parameters of `buildSIPRECResponseBody`
  - the `paused` field, which is set but never read
- **`SessionsSnapshot().WAVFiles` returns the wrong paths.** It joins `RecordingDir` with only the label, not `<callID>-<label>.wav`. Store the real path on `leg` and use it in both the snapshot and `finalizeSession`.
- **Relative paths are stored in the database.** `RECORDING_DIR` defaults to `./data/recordings` and WAV paths are saved relative to it. Starting the process from a different working directory orphans every file, and `safeWAVPath` rejects them. Resolve the directory with `filepath.Abs` at startup.
- **Configuration isn't validated:**
  - `RTP_PORT_MIN > RTP_PORT_MAX` produces an empty pool without any warning.
  - An odd `RTP_PORT_MIN` produces odd RTP ports.
  - Unknown `SIPREC_NETWORKS` values only fail later in sipgo.
  - Setting only one of `SIP_TLS_CERT` and `SIP_TLS_KEY` disables TLS silently.
- **No `OPTIONS` handler.** Keepalive pings from recording clients get `405 Method Not Allowed`. Answer them with 200.
- **Search input isn't escaped.** `ListCalls` uses the search text directly in `LIKE`, so `%` and `_` act as wildcards. Escape them and add `ESCAPE '\'`.
- **No real schema migrations.** The schema uses only `CREATE TABLE IF NOT EXISTS`. Add migrations based on `PRAGMA user_version` before the first schema change after release.
- **Package dependency direction.** `store` imports `siprecsrv` for `FinalizedLeg` and `Recording`. That works, but it ties the storage layer to the SIP package. Consider a small shared types package.

## Test coverage

| Package | Tests in reviewed tree | Coverage on 2026-10-21 working tree (after tag) |
| --- | --- | --- |
| `siprecsrv` | Yes (SDP, metadata, server, mod_siprec interop) | 77.2% |
| `store` | Yes | 75.5% |
| `auth` | None | 34.4% |
| `web` | None | 55.0% |
| `rtp` | None | 60.1% |
| `recording` | None | 72.5% |
| `voiceml` | Package did not exist | 71.1% |
| `cmd/siprec-receiver` | None | 0.0% |

The coverage column comes from `go test -race -cover ./...` on a working tree that had uncommitted changes from other work in progress, so it describes neither the tag nor any single commit exactly. I haven't checked whether the tests added at the tag cover the cases listed below.

Recommended additions:

1. A `web` notify test with form and JSON bodies; this would have caught item 1.
2. `auth` session-cookie tests: tampered signature, expired session, and a removed or disabled user.
3. An `rtp` parse and decode test with padding, CSRC, extension headers, and sequence wrap.
4. A `recording` round-trip test that checks the header sizes after `Close`.
5. `siprecsrv` tests for a re-INVITE that changes the media shape (file overwrite), a BYE racing the INVITE, and shutdown finalization.

## Adversarial review

This section looks at the service the way an attacker would. For each input it asks who can reach it, what they control, and what they can damage. Items already covered above are only referenced here. In a recording system, the integrity and confidentiality of recordings are the main things to protect, often for compliance reasons.

### Who can reach what

| Interface | Default bind | Authentication | Attacker controls |
| --- | --- | --- | --- |
| SIP (UDP/TCP/TLS) | `127.0.0.1:5070/5071` | None | Call-ID, Request-URI params (`accountsid`, `callsid`), From, SDP, RS-Metadata |
| RTP | `0.0.0.0:41000-42000` | None | Audio content, packet rate, sequence numbers and timestamps |
| `POST /api/notify/amd` | `127.0.0.1:8080` | Shared secret | Every AMD field, including the correlation keys |
| Web UI | `127.0.0.1:8080` | Google OAuth + allowlist | Search, filters, form posts (after sign-in) |

SIP is on loopback by default, but RTP is not. Even a correctly firewalled SIP port still leaves about 500 UDP ports open to anyone who can reach the host.

### A1. Overwriting a finished recording by reusing its Call-ID (high)

`finalizeSession` removes the session from memory after the BYE. Nothing stops a new INVITE from using the same Call-ID later.

1. The attacker sends `INVITE` with `Call-ID: <victim's Call-ID>`.
2. `onInvite` finds no live session and calls `recording.Open(<same path>)` with `O_TRUNC`, which empties the victim's WAV.
3. After the attacker's BYE, `RecordLeg` runs `ON CONFLICT(sip_call_id) DO UPDATE` on the **victim's call row**. That replaces `from_uri`, `from_number`, `to_number`, `metadata_json`, `metadata_xml`, `ended_at`, and the leg's `participant_aor`/`duration_sec`.

The victim's audio is replaced with audio the attacker chose, under the victim's call record. Anyone with SIP access can do this. Call-IDs are shown on every recording's detail page, so any signed-in reviewer with SIP access can see the targets.

**Fix:**

- Open WAV files with `O_CREATE|O_EXCL`, never `O_TRUNC`.
- Refuse (or give a new row) an INVITE whose Call-ID already has a completed row.
- Consider storing a SHA-256 hash of each finalized WAV in `legs` so any later change can be detected.

### A2. Filename collisions from Call-ID sanitizing (high)

`sanitiseCallID` maps several characters to the same output: `/`, `\`, and `:` all become `_`, and `@` becomes `_at_`, which can also appear literally. So the Call-IDs `abc@host`, `abc_at_host`, `abc/at/host`… (and so on) share one file name, while staying separate rows in the database (`sip_call_id` differs).

- An attacker who knows a live call's Call-ID can start a session with a colliding Call-ID. Both writers then append to the same file at once, or the second `O_TRUNC` wipes the first.
- Two database rows now point at the same `wav_path`. When an admin deletes the attacker's "junk" call, `DeleteCall` runs `os.Remove` on the **victim's** file.

**Fix:** Name files from something the server generates, such as the call's primary key or a random UUID, and keep the Call-ID only in the database. If the Call-ID has to stay in the filename, use a sanitizing function that maps each input to a unique output (for example hex or base32 encoding) and add `O_EXCL`.

### A3. Anyone can end or replace a live recording (high)

`onBye` and the re-INVITE path look sessions up **by Call-ID only**. They don't check the From-tag or To-tag, the source address, or the transport. Anyone who learns a live Call-ID can:

- send a BYE to stop the recording early. Over UDP the source address can be spoofed, because the 200 OK goes to the `Via` address and the attacker doesn't need to receive it.
- send a re-INVITE with a different number of `m=` lines. That finalizes the live session and starts a new one on the same file paths (see A1).
- send a re-INVITE with all streams `a=inactive`. That marks the session paused, and if the real recording client keeps sending RTP on a different SSRC, see A5.

**Fix:** Store the dialog ID (Call-ID, From-tag, and the To-tag the server issued) and the source transport and address on the session. Reject in-dialog requests that don't match with `481 Call/Transaction Does Not Exist`.

### A4. Permanent denial of service with very few packets (high)

The port pool is about 500 ports, sessions never expire (item 3), and nothing limits the number of streams (security section). Together this means:

- **One INVITE with about 500 `m=audio` lines** (well under the 64 KB SIP message limit) takes every RTP port. The attacker never sends a BYE, so no real call can be recorded until the process restarts.
- About 250 normal two-stream INVITEs do the same.
- Each session also holds a goroutine, a socket, and an open file, which pushes towards the process file-descriptor limit.

**Fix:** In addition to the earlier fixes (stream cap, session cap, reaper), refuse INVITEs while fewer than about 10% of ports are free, and limit sessions per source IP.

### A5. Injecting audio into recordings and mixing calls through port reuse (high)

Because RTP binds `0.0.0.0` and accepts any sender:

- **Injection.** An attacker sweeps 41000–42000/udp with valid RTP headers, payload type 0, and any SSRC. Their audio lands in whichever recordings are live, mixed in with the real packets, and nothing is logged.
- **Ports are reused last-in, first-out.** `PortPool.Acquire` takes from the end of `free` and `Release` adds to the end. The port a finished call just released is therefore the **first** one handed out again. If the recording client of the call that just ended keeps sending RTP briefly (common during teardown), its audio is recorded into the **next caller's** file. That leaks one customer's audio into another customer's recording, with no attacker needed.

**Fix:**

- Bind to the advertised IP.
- Lock each leg to the first source address and SSRC it receives, and drop everything else.
- Hand out ports first-in, first-out, and keep a released port out of use for a few seconds before reuse.

### A6. Filling the disk through RTP (medium)

Each leg writes every valid packet straight to disk. It doesn't check that the arrival rate is anywhere near the 8,000 samples per second that G.711 produces, or that the RTP timestamps advance. An attacker sending 1,400-byte packets at line rate to one open port writes about 2 bytes of WAV for every byte received. That fills the disk, and once it's full the writes fail silently (`_, _ = wr.Append`). A 4 GB WAV also overflows the `uint32` size fields (at the normal rate that takes about 74 hours of audio, but a flood gets there in minutes), which corrupts the header.

**Fix:**

- Drop samples beyond the expected rate, based on elapsed time or the RTP timestamp.
- Cap the size of each leg.
- Reject new sessions when free disk space is low.

### A7. Attackers choose which tenant and call a recording belongs to (medium)

`accountsid` and `callsid` come from the Request-URI, and `FsUUID` comes from the RS-Metadata. All of these are controlled by the sender, and none is authenticated.

- An attacker can set `callsid` to a victim's CallSid. `RecordLeg` then claims the victim's AMD placeholder (the `UPDATE calls SET sip_call_id = ? WHERE call_sid = ?` step), attaching the victim's AMD result to the attacker's audio, or the other way round.
- If tenant scoping is added later and relies on `accountsid`, any SIP sender can file recordings under any tenant.

**Fix:** Only accept correlation parameters from authenticated recording clients (source allowlist, or mTLS on the SIPS listener). If the parameters must come through the URI, sign them with an HMAC using a secret shared with the dialer.

### A8. A notify sender can steer which recording gets an AMD result (low; needs the secret)

The upsert in `ApplyAMD` sets `sip_call_id = COALESCE(sip_call_id, ?)` and `call_sid = COALESCE(call_sid, ?)`. Someone with the notify secret can post `CallSid=X&SipCallId=Y` before recording Y finishes. Recording Y then merges into row X. Placeholder rows are also never cleaned up and nothing limits how many a sender can create.

**Fix:** Expire placeholders after a set time, and don't let a notification set `sip_call_id` on a row that has a different `call_sid`.

### A9. Allowlisted emails don't pin the Google account (medium)

`HandleCallback` trusts `email` plus `email_verified`. A Google account can be created for any existing email address, for example `alice@company.com`, by verifying a code sent to it ([Google Account Help 27441](https://support.google.com/accounts/answer/27441): "You don't need to have a Gmail address to create a Google Account"). After Alice leaves the company, she keeps the Google account and still passes the allowlist until someone removes her row. If the company uses Google Workspace, the address can also belong to an unmanaged consumer account that the company doesn't control.

**Fix:**

- If all users are in one Workspace domain, check the `hd` claim against a configured `GOOGLE_HOSTED_DOMAIN`.
- Record the `sub` claim on first sign-in and require it to match afterwards, so the allowlist entry is tied to one Google account rather than an email address.

### A10. Destructive admin actions have no record or undo (medium)

Any admin listed in the database can:

- delete any recording permanently (database row and WAV);
- add new admins, or demote and remove other database admins.

The only record of who did what is a `log.Printf` line. If one admin account is compromised, every recording can be wiped and the only evidence is the process log.

**Fix:**

- Add an `audit_log` table (who, what, when, from which IP) and show it to admins.
- Soft-delete recordings with a retention window before removing the files.
- Consider requiring a second admin to confirm bulk or permanent deletion, or a retention-lock setting.

### A11. Who called whom is readable by any local user (medium)

- The SQLite database is created with the process umask: `0644` here, because `umask` is `0002`. It holds every caller and callee number, the metadata, the AMD bodies, and the user list.
- The recordings directory and the database directory are created `0755`. The WAV files themselves are `0600`, but their names contain the Call-ID and the participant phone number (`<callID>-12165285911.wav`), so any local user can list the directory and see who called whom.
- The logs contain emails, From URIs, and Call-IDs. Treat them as personal data, or make the log level configurable.

**Fix:** Create the directories `0700`, `chmod 0600` the database files (including `-wal` and `-shm`) after opening, and use opaque filenames (A2).

### A12. Fake log lines through notify fields (low)

`[notify]` log lines include `CallSid`, `SipCallId`, and `AnsweredBy` exactly as received. JSON strings can contain `\n`, so a notify sender can forge log lines, for example a fake `[web] admin … deleted call …`. This matters once the logs serve as the audit trail. Escape values with `%q`, or switch to structured logging (`log/slog`).

### A13. SDP line injection through `a=label` (low)

`ParseSDP` splits on `\n` and removes only a **trailing** `\r`, so a label can contain a bare `\r` in the middle. `BuildAnswerSDP` writes the label back out (`a=label:%s`), so the SDP answer then contains extra `\r`-separated lines that the attacker chose. The only one affected is the sender's own answer, but some SDP parsers treat a bare CR as a line break. Strip control characters from labels, or allow only `[A-Za-z0-9._-]`.

### A14. Toolchain and dependencies (medium)

`govulncheck ./...` with the installed Go 1.26.8 reports **13 standard-library vulnerabilities that the code can actually reach**. Go 1.26.9 fixes 12 of them. The 13th, **GO-2026-6809** (compromised intermediate certificates in `crypto/x509`), is fixed only in **Go 1.26.10**. Scans with 1.26.10 and with 1.27.3 (the current `stable`) find no reachable vulnerabilities. These matter for this service in particular:

- **GO-2026-6609** – `net/http` doesn't limit the size of parsed `Range` headers. `handleAudio` uses `http.ServeContent`, which parses `Range`.
- **GO-2026-6608** – a memory-limit bypass when parsing MIME headers in `mime/multipart`, which `extractSDPAndMetadata` uses on unauthenticated SIP bodies.
- **GO-2026-6599 / GO-2026-6600** – `html/template` gets the escaping context wrong in some cases. All web pages are rendered with it.
- The HTTP/2 server issues (6603, 6611, 6612, 6617) apply if the service is ever exposed over TLS without a reverse proxy in front.

Also:

- **Toolchain version.** Set `toolchain go1.26.10` (or later) in `go.mod`. `.github/workflows/security-scan.yml` now runs `govulncheck` in CI on `go-version: stable` and fails on any reachable vulnerability.
- **Unreleased Google auth dependency.** `cloud.google.com/go/auth` is pinned to an **unreleased commit** (`v0.24.1-0.20261001053825-dbc26066f70a`), which comes in through `google.golang.org/api`. That's code the authentication path depends on but that has no release. Pin `google.golang.org/api` to a version that uses a tagged `cloud.google.com/go/auth`, or explain why this commit is needed.
- **Large dependency for one call.** `google.golang.org/api` brings in gRPC, OpenTelemetry, and s2a just for `idtoken.Validate`. Checking the ID token with `github.com/coreos/go-oidc` (or the JWKS directly) would remove most of those dependencies.
- **cgo build.** `mattn/go-sqlite3` requires cgo. That's fine, but release builds should be reproducible (`-trimpath`, pinned C toolchain), or switch to `modernc.org/sqlite`.

### A15. Smaller hardening gaps

- **No `X-Content-Type-Options: nosniff`** on any response, including `/audio` and the HTML pages.
- **The CSRF token never changes.** It is `HMAC(SESSION_SECRET, "csrf:"+email)` and is identical for every session of a user until the secret rotates. A token that leaks once (an HTML page in a cache, a screenshot, a proxy log) stays valid. Include a per-session nonce, or the session expiry, in the HMAC input.
- **Sessions can't be revoked.** Signing out deletes only the cookie in the browser. A stolen cookie works for up to 12 hours unless the user is removed. Add a per-user `session_epoch` to the `users` table and put it in the cookie.
- **The session cookie has no `__Host-` prefix.** A sibling subdomain can't forge a signed cookie, but it can set an extra one that confuses which cookie is used (a cookie-tossing attack, which can also be used to force a logout). Name the cookie `__Host-sr_session` when served over https.
- **`/healthz` and every other route share one listener with the UI.** Put a reverse proxy in front with rate limits on `/auth/*` and `/api/notify/amd`. The service itself has no rate limiting.
- **A single crafted packet can't crash the receiver** (bounds checks hold), but there is no `recover()` in the RTP loop or the SIP handlers. If one ever panics, the whole process exits and every active recording is lost without being finalized (item 2).

## Recommended order of work

1. Upgrade to Go 1.26.10 or later (A14). This is a one-line change that clears all 13 reachable vulnerabilities; 1.26.9 still leaves GO-2026-6809.
2. Fix notify form parsing (item 1) and add the web test.
3. Protect recording integrity: name files from server-generated IDs and open them with `O_EXCL`, and refuse Call-IDs that already exist (items 4, A1, A2).
4. Finalize sessions on shutdown and add the session reaper (items 2 and 3).
5. Check the dialog on BYE and re-INVITE (A3). Add the Contact header and an OPTIONS handler, and use one shared session map across transports.
6. Lock down media: bind RTP to the local IP, lock each leg to one source and SSRC, reuse ports first-in, first-out with a delay, and limit the write rate (A5, A6).
7. Add SIP source allowlisting and caps on streams and sessions (security section, A4). Authenticate the correlation parameters (A7).
8. Settle the CallSid uniqueness model (item 5, A8).
9. Tighten file and database permissions (A11).
10. Guard the dev bypass, add `hd`/`sub` pinning (A9), and add an audit log and soft delete (A10).
11. Remove the stale documentation and dead code, and add the README.

## Sources for external claims

- **RFC 3261 §12.1.1** (UAS behavior): "The UAS MUST add a Contact header field to the response." <https://www.rfc-editor.org/rfc/rfc3261.txt>
- **RFC 3261 §18.1.1** (Sending Requests): requests "larger than 1300 bytes and the path MTU is unknown … MUST be sent using an RFC 2914 congestion controlled transport protocol, such as TCP." Same document.
- **RFC 3264 §6.1** (Unicast Streams): "If an offered media stream is listed as inactive, it MUST be marked as inactive in the answer." <https://www.rfc-editor.org/rfc/rfc3264.txt>
- **RFC 3264 §8** (Modifying the Session): "the version in the origin field MUST increment by one from the previous SDP. If the version in the origin line does not increment, the SDP MUST be identical." Same document.
- **Google accounts with non-Gmail addresses:** <https://support.google.com/accounts/answer/27441>
- **sipgo behavior** (response headers, 64 KB `ParseMaxMessageLength`, 405 for unhandled methods): sipgo v1.6.0 source in the module cache (`sip/response.go`, `sip/parser.go`, `server.go`).
- **Go vulnerability IDs and fixed versions:** output of `govulncheck` (latest), cross-checked at <https://pkg.go.dev/vuln/>.
- **RFC 7865/7866 behavior** cited in source comments (for example pause and resume in §6.4) was **not** checked during this review.
