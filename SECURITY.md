# Security Policy

## Reporting a Vulnerability

This repository hosts a **self-hosted SIPREC Session Recording Server**
(Go binary `siprec-receiver`): it receives call audio over SIP/RTP,
stores recordings and metadata, and serves them over an
OAuth-protected web UI.

Please **do not** open a public issue for security vulnerabilities.

Use GitHub's private vulnerability reporting on this repository:
**Security → Report a vulnerability** (or
<https://github.com/voicetel/siprec-receiver/security/advisories/new>).

Include, where possible:

- A description of the issue and its impact
- Steps to reproduce or a proof of concept
- Affected version(s) and configuration
- Whether the issue is reachable from the SIP listener, the notify
  endpoint, or the authenticated web UI

You can expect an acknowledgement within a few business days. Please
allow reasonable time for a fix before any public disclosure.

## Scope Notes

siprec-receiver has three network surfaces, with different trust
models:

- **SIP/SIPREC listener (`SIPREC_LISTEN`)** — SIPREC (RFC 7866) has
  no in-band authentication: anyone who can reach this listener can
  offer recordings. It must be firewalled to your FreeSWITCH/SRC
  hosts only. The SDP answer advertises `SIPREC_LOCAL_IP`; the RTP
  port range (`RTP_PORT_MIN`–`RTP_PORT_MAX`) must likewise only be
  reachable from those hosts.
- **AMD notify endpoint (`POST /api/notify/amd`)** — authenticated
  with the `NOTIFY_SECRET` shared secret (Bearer header preferred;
  the `?secret=` form exists for webhook senders that cannot set
  headers — prefer headers, since URLs get logged). With no secret
  configured the endpoint disables itself (503). Rotate the secret
  if a posting system is decommissioned.
- **Web UI (`HTTP_LISTEN`)** — Google OAuth sign-in plus an
  allowlist. Only the `ADMIN_EMAIL` bootstrap administrator and
  users an administrator adds can sign in; roles are re-checked on
  every request. Serve it over HTTPS (reverse proxy) with
  `PUBLIC_BASE_URL=https://…` so session cookies are `Secure`.
  `SESSION_SECRET` must be a stable random value in production.
  **Never set `DEV_AUTH_EMAIL` in production** — it bypasses Google
  sign-in entirely.

Recordings are sensitive: WAV files are written `0600` under
`RECORDING_DIR`, are only served to authenticated users after a
path-confinement check, and deleting a call in the UI removes its
files. Back up `DB_PATH` and `RECORDING_DIR` together and protect
both like the audio they describe.

Out of scope: vulnerabilities in FreeSWITCH or `mod_siprec` itself
(report upstream), in Google's OAuth service, or in the operator's
network configuration and reverse proxy.
