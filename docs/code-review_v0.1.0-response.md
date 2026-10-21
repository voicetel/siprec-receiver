# Response to the v0.1.0 adversarial code review

**Date:** 2026-10-21
**Reviews:** `docs/code-review_v0.1.0.md` (adversarial review of the pre-tag tree)
**Disposition of this document:** the review itself is kept unmodified as the
historical record. Every numbered item is triaged below against the current
tree on `main`. Status values:

- **Fixed** — addressed in the hardening pass after the review.
- **Already fixed** — the review predates the fix (it reviewed the uncommitted
  v0.1.0 tree; several items landed before the tag).
- **Partial** — the main risk is closed; a named remainder is documented.
- **Accepted** — deliberately not changed; rationale recorded.

Verification for the whole pass: `go vet` clean, `golangci-lint` 0 issues,
`go test -count=1 ./...` green in all packages, `govulncheck` reports **no
vulnerabilities** (toolchain pinned to go1.26.10), and
`tests/interop/run.sh` (real FreeSWITCH + mod_siprec calls) passes end to end.

---

## Must fix before v0.1.0

| # | Item | Status | Evidence / notes |
|---|---|---|---|
| 1 | Form-encoded AMD notifications lose all fields | **Already fixed** | `parseNotifyBody` parses the already-read bytes with `url.ParseQuery`; `Raw` is the raw body. Covered by form + JSON web tests. (The disposition endpoint shares the parser.) |
| 2 | Shutdown discards active recordings | **Fixed** | `siprecsrv.Server.Shutdown` finalizes every session; `main` shuts HTTP down, drains sessions into the store, waits for the SIP listeners (`sync.WaitGroup`), and only then closes the store. Test: `TestHardening_ShutdownFinalizes`. Startup orphan adoption remains as the crash backstop. |
| 3 | Sessions that never get a BYE are never cleaned up | **Fixed** | Idle reaper (default 5 min without SIP or RTP, `SIPREC_SESSION_IDLE_SEC`) finalizes abandoned sessions and drops stale pending reservations. Test: `TestHardening_ReaperFinalizesIdle`. |
| 4 | Same WAV path can be reused and silently overwrite audio | **Fixed** | Filenames are server-generated random names opened `O_EXCL` (`recording.OpenExclusive`); labels are de-duplicated within a session and live only in the database. Nothing on disk is ever truncated by a later session. |
| 5 | Second recording session for the same CallSid fails to save | **Fixed (model settled)** | Model: the first dialog owns the `CallSid` correlation key. A later dialog carrying an owned CallSid records under its own SIP Call-ID without stealing the key (no UNIQUE failure, no cross-attachment). `ApplyAMD`/`ApplyDisposition` run their lookup + write in one transaction (the store's single connection serializes writers), never stamp a SIP Call-ID onto a row whose CallSid names a different call, and never steal a CallSid another row owns. Test: `TestRecordLeg_SecondDialogSameCallSid`. |
| 6 | The 200 OK has no Contact header | **Fixed** | Every 2xx carries `Contact: <sip:srs@host:port;transport=…>` for its listener (RFC 3261 §12.1.1). Test: `TestHardening_ContactAndDialogChecks`. Verified against real sofia/mod_siprec in the interop run. |

## Security

| Item | Status | Evidence / notes |
|---|---|---|
| SIP has no access control | **Fixed** | `SIPREC_ALLOW_CIDRS` source allowlist (others get 403), max 8 streams per INVITE (488), concurrent-session cap `SIPREC_MAX_SESSIONS` (503). A disk-space pre-check was **not** added: the leg sample budget, retention sweep, and write-error surfacing bound the blast radius; capacity alerting stays an operator concern. Per-source session caps were not added — the allowlist is the per-source control. |
| RTP listens on every interface, accepts any sender | **Fixed** | Receivers bind `SIPREC_LOCAL_IP` (`rtp.NewReceiverIP`) and lock onto the first (source address, SSRC); all other packets are dropped (one log line). Ports recycle FIFO, so a released port is reused last. Tests: `TestReceiver_LocksToFirstSender`, pool tests. |
| Dev auth bypass has no safeguard | **Fixed** | Startup refuses `DEV_AUTH_EMAIL` with a non-loopback `HTTP_LISTEN` (fatal config error). |
| Notify secret can be passed in the URL | **Partial** | Kept for senders that cannot set headers (documented in README + SECURITY), but the first query-secret use now logs a one-time warning that it will appear in logs. |
| Logout is a GET | **Fixed** | Logout is POST-only, linked as a form. Test: `TestLogoutRequiresPOST`. |
| No frame/CSP headers | **Fixed** | `X-Frame-Options: DENY`, `Content-Security-Policy: frame-ancestors 'none'` (+ self-only defaults), `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin` on every response. Test: `TestSecurityHeadersPresent`. |
| `msg` query parameter reflected | **Accepted** | It is the flash-message channel for form posts and is HTML-escaped by `html/template`; crafting a link that shows your own text is not a vuln. Noted here so the decision is explicit. |
| OAuth flow doesn't use PKCE; state cookie not cleared | **Fixed** | PKCE (S256) is now used; state + verifier cookies are cleared on every callback outcome. |
| `SESSION_SECRET` minimum length | **Fixed** | A set secret under 32 bytes is a fatal config error. |
| HTTP server timeouts | **Fixed** | `IdleTimeout` (120 s) added; `ReadHeaderTimeout` already set. No global `WriteTimeout` (would cut audio downloads), as the review itself advises. |
| No tenant scoping | **Accepted, now documented** | Intended model: the allowlist is the access boundary; every reviewer sees every recording. Stated explicitly in README ("Web UI and roles"). |

## Correctness (medium)

| Item | Status | Evidence / notes |
|---|---|---|
| Concurrent INVITE/BYE can leak sessions | **Fixed** | The Call-ID is reserved (`pending`) in the session map before any allocation; a concurrent INVITE gets 491, a racing BYE marks the reservation cancelled and the builder finalizes/rolls back under the same lock. |
| Each transport has its own session map | **Fixed** | Listeners are `Clone()`s of one family sharing a single session map, port pool, and store; dialogs may cross transports. Verified by the interop run (BYE on the dialog's transport) and unit tests. |
| Codec selection only looks at the first payload type | **Fixed** | All offered PTs + rtpmaps are scanned; telephone-event-first and dynamic-PT offers now pick PCMU/PCMA correctly. Answers always use the static PT. Tests: `TestParseSDP_CodecChoiceScansAllPayloadTypes`. |
| SDP answer: port-0 streams get a recorder | **Fixed** | Port-0 offers are answered port 0 and never allocate a receiver or file. Test: `TestBuildAnswerSDP_PortZeroStream`. |
| SDP answer: inactive answered recvonly | **Fixed** | Inactive offers are answered `a=inactive` (RFC 3264 §6.1). |
| SDP answer: `o=` version never changes | **Fixed** | Answers carry a per-session increasing version (`BuildAnswerSDPWithVersion`). Test: `TestBuildAnswerSDP_VersionAndInactive`. |
| Media-level `c=` ignored | **Fixed (parsed)** | Media-level connection addresses are parsed into `MediaDescription.ConnectionIP`. Source enforcement uses first-sender locking instead (more robust than trusting SDP), so the field is informational today. |
| RTP receiver ignores timing | **Fixed** | RTP padding is stripped; sequence differences are signed (late/duplicate packets drop, gaps count as loss); timestamp gaps are filled with silence so duration and leg sync stay honest; the loop recovers from panics. Tests: `TestReceiver_StripsPadding`, `TestReceiver_FillsTimestampGapsWithSilence`. |
| Bad metadata on re-INVITE replaces good metadata | **Fixed** | Replacement happens only when the new metadata parses. |
| WAV write errors ignored | **Already fixed** | `legWriter` captures the first error, logs it immediately, stores it on the leg, and the UI badges it (⚠). Test: `TestLegWriter_CapturesFirstWriteError`, `TestRecordLeg_WriteErrorPersisted`. |
| Multipart errors treated as end of body | **Fixed** | Only `io.EOF` ends the part loop; other errors fail the INVITE with 400. Part reads use `io.ReadAll`. |
| DB writes delay the BYE response | **Fixed** | BYE is answered before finalizing. |
| `fs_uuid` lookups can match several rows | **Fixed** | Deterministic pick (newest `started_at`) in both notify paths and placeholder merging. |
| Very long Call-IDs can't be saved | **Fixed (moot)** | Filenames no longer derive from Call-IDs at all (random names). |

## Maintainability

| Item | Status | Evidence / notes |
|---|---|---|
| Stale documentation copied from the source project | **Fixed** | Package/function comments rewritten for this repo (no S3/Uploader/ops references); README, SECURITY.md, and `.env.example` are current. |
| Unused code (`decodePCMU/PCMA`, `BuildMetadataResponse`, ignored params, unread `paused`) | **Fixed** | All removed (or, for `paused`, surfaced via `SessionsSnapshot`). `golangci-lint` reports 0 issues. |
| `SessionsSnapshot().WAVFiles` returns wrong paths | **Fixed** | Legs carry their real `wavPath`; snapshot and finalize both use it. |
| Relative paths stored in the DB | **Fixed** | `RECORDING_DIR`/`DB_PATH` are resolved to absolute paths at startup. |
| Configuration isn't validated | **Fixed** | Fatal on unknown transports, `RTP_PORT_MIN > RTP_PORT_MAX`, half-configured SIP TLS, short `SESSION_SECRET`, dev bypass on a public listener; odd RTP minimum is adjusted with a log line. |
| No `OPTIONS` handler | **Fixed** | `OnOptions` answers 200 with an `Allow` header. |
| Search input isn't escaped (`LIKE` wildcards) | **Fixed** | `%`, `_`, `\` are escaped with `ESCAPE '\'`. Test: `TestListCalls_LikeEscaping`. |
| No real schema migrations | **Partial** | Idempotent DDL + `addColumnIfMissing` remains the mechanism, now stamped with `PRAGMA user_version = 1` as the baseline for any future non-idempotent migration; the pre-rename schema objects migrate automatically (test: `TestOpen_MigratesLegacyCBNames`). A migration framework for its own sake was not adopted. |
| `store` imports `siprecsrv` | **Accepted** | The dependency is one direction (`siprecsrv` never imports `store`), small, and keeps `FinalizedLeg` single-sourced. A shared types package would add a third package to every change without breaking a cycle. Noted so the choice is deliberate. |

## Adversarial review

| # | Item | Status | Evidence / notes |
|---|---|---|---|
| A1 | Overwriting a finished recording by reusing its Call-ID (high) | **Fixed** | Random `O_EXCL` filenames make truncation impossible; a second session for the same dialog ID merges as a distinct recording identity (see item 5's key model) and can no longer replace audio bytes. |
| A2 | Filename collisions from Call-ID sanitising (high) | **Fixed** | Filenames no longer derive from Call-IDs at all. Deleting one call can only remove its own random-named files. |
| A3 | Anyone can end or replace a live recording (high) | **Fixed** | In-dialog requests must match the INVITE's From-tag and source IP, else 481 with the recording untouched. Test: `TestHardening_ContactAndDialogChecks`. (Dialog IDs are still shown to signed-in reviewers, as call detail requires; SIP reachability + allowlist + dialog binding are the controls.) |
| A4 | Permanent DoS with very few packets (high) | **Fixed** | Stream cap (8), session cap (default 256), idle reaper, source allowlist. The "refuse below 10% free ports" rule is subsumed by the pool answering 500 cleanly + session caps. |
| A5 | Injecting audio / cross-call mixing via port reuse (high) | **Fixed** | Bind to the advertised IP, first-sender + SSRC lock, FIFO port reuse. |
| A6 | Filling the disk through RTP (medium) | **Fixed** | Per-leg sample budget: writes stop (and the leg is marked with a write error) beyond 1.5× the 8 kHz wall-clock rate or 4 h of audio, so one port can no longer fill the disk; retention bounds total growth. |
| A7 | Attackers choose tenant/call attribution (medium) | **Partial** | The control is `SIPREC_ALLOW_CIDRS` (+ firewall): correlation parameters are only meaningful from trusted SRCs, which is now the documented deployment requirement. HMAC-signing URI parameters was **not** implemented — it would require coordinated changes in every SRC/dialplan for a threat the allowlist already closes. |
| A8 | Notify sender steers AMD onto another recording (low) | **Fixed** | Notifications never stamp a SIP Call-ID onto a row whose CallSid names a different call, and never steal an owned CallSid (item 5). Placeholder expiry is via `RETENTION_DAYS` when enabled; with retention disabled, placeholders (which hold only notification data, no audio) persist — documented trade-off. |
| A9 | Allowlisted emails don't pin the Google account (medium) | **Fixed** | First sign-in pins the Google `sub`; later sign-ins must match it. `GOOGLE_HOSTED_DOMAIN` optionally requires a Workspace `hd`. Test: `TestSubPinning`. |
| A10 | Destructive admin actions have no record or undo (medium) | **Partial** | An `audit_log` (who/what/when/target) now records deletions, user and key management, session revocations, and record fetches, viewable at `/admin/audit`. Soft-delete with an undo window and dual-admin confirmation were **not** implemented — deletion remains permanent by design for a recording-retention tool; the audit trail is the accountability control. |
| A11 | Who called whom readable by any local user (medium) | **Fixed** | Recording and DB directories are created `0700`, DB files `0600`, WAV files `0600` under random names that contain neither Call-IDs nor numbers. |
| A12 | Fake log lines through notify fields (low) | **Fixed** | Notify log lines quote user-controlled values with `%q`. |
| A13 | SDP line injection through `a=label` (low) | **Fixed** | Labels are sanitised at SDP parse time (`[A-Za-z0-9._-]`), before they can reach the answer. Test: `TestParseSDP_LabelSanitised`. |
| A14 | Toolchain and dependencies (medium) | **Fixed** | `toolchain go1.26.10`; `govulncheck ./...` reports **no vulnerabilities** (was 13 reachable on go1.26.8; the one remaining no-fix advisory is not called by this code). The auth dependency is pinned to the tagged `cloud.google.com/go/auth v0.23.3` (via `google.golang.org/api v0.299.0`), replacing the unreleased pseudo-version. Replacing Google's `idtoken` validator with go-oidc to slim the tree was **not** done: keeping the vendor-maintained validator is worth more here than the dependency count. The SQLite driver stays cgo (`mattn/go-sqlite3`); release/Docker builds are `-trimpath` reproducible. |
| A15 | Smaller hardening gaps | **Mostly fixed** | `nosniff` ✓; CSRF tokens are bound to the session expiry ✓; sessions are revocable per user (epoch + admin **Revoke sessions**) ✓; the session cookie is `__Host-`-prefixed on HTTPS ✓; panic recovery in the RTP loop and SIP INVITE/BYE handlers ✓. Rate limiting stays at the reverse proxy (documented in SECURITY.md), and `/healthz` intentionally stays on the single listener. |

## Test coverage (updated)

The review's "no tests" rows no longer hold: `auth`, `web`, `rtp`, and
`recording` all have suites, including the exact notify-form test whose
absence let item 1 ship. SIP dialog hardening, store conflict handling, and
legacy-schema migration are covered as cited above, and the whole system is
exercised against real FreeSWITCH + mod_siprec by `tests/interop/run.sh`
(INTEROP PASS after this pass: mixed pause/resume, mid-call stop,
separate-streams tone separation, decoded FreeSWITCH UUIDs, AMD merge).
