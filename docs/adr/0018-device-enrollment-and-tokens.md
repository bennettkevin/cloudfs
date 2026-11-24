# ADR-0018: Device enrollment with one-time codes and per-device tokens

- **Status:** Accepted
- **Date:** 2026-11-24
- **Research:** [Spike 08](../research/08-device-auth.md) (#8)

## Context

CloudFS clients are headless daemons on macOS and Linux, and v1 has no web UI. A fresh self-hosted server needs a safe way to get its first device, later devices need to be added, and a lost device must be cut off. Every API call, including presigned URL issuance ([ADR-0015](0015-direct-presigned-chunk-transfer.md)), must be authorized server-side. Device auth is separate from encryption keys ([ADR-0007](0007-end-to-end-encryption.md)). The server is single-user for now but must stay ready for multi-user ([ADR-0002](0002-single-user-first.md)).

## Decision

**One mechanism for the first device and every later one: a one-time activation code.**
- Created by the server's admin CLI (`cloudfs-server enroll`) or by an already-enrolled device (`cloudfs device add`).
- Format `XXXXX-XXXXX`: 10 characters from a 20-letter alphabet, about 43 bits.
- Valid 15 minutes, single use, at most 5 active at once.
- After 10 failed attempts in total, every active code is invalidated. There is also a per-IP rate limit on the enroll endpoint. Together these make a successful guess about a 1-in-10¹² event.
- Stored only as an argon2id hash with a salt. Used and expired codes are deleted by the worker after a day.
- The flow is "push": the code goes from the trusted side to the new device, which types it in. RFC 8628 (OAuth device flow) needs a browser login, which v1 doesn't have.

**Each device then has its own long-lived token.**
- The new device generates the token itself (`cfs_` + 32 random bytes) and sends only its SHA-256 with the code. The server stores that hash; the token never leaves the device after enrollment. Retrying an enrollment with the same code and hash returns the same result.
- Sent on every request as `Authorization: Bearer`, over TLS, never in a URL.
- The server looks the hash up in Postgres on every request, with no cache, so a revocation takes effect on the device's next request.
- No JWTs, no refresh-token rotation, no client certificates (mTLS) and no request signing in v1. A nullable `devices.public_key` column is reserved so per-device keypairs or device-to-device key handoff can be added in phase 1.

**Revocation and expiry.**
- Any enrolled device or the admin CLI can list, rename and revoke devices. Revoking warns about devices the revoked one enrolled and offers to revoke them too.
- Presigned URLs already issued stay valid for their remaining ≤ 15 minutes.
- Revoking a device does not remove encryption keys it already holds. Key rotation after a revocation belongs to the phase 1 key hierarchy.
- A device unused for 180 days is revoked automatically (configurable; 0 turns it off). There is no absolute expiry.

**Authorization.** One middleware covers every route except enroll and health. It puts `Principal{UserID, DeviceID}` into the request context. A one-row `users` table and `devices.user_id` exist from day one, so multi-user stays additive. The change signal uses long-polling first; each poll re-checks the token.

**Token storage on the device:** behind a `SecretStore` interface. Default is a file with `0600` permissions, and the client refuses to start if they're looser. On macOS the Keychain is used via `zalando/go-keyring` (MIT, v0.2.8, no cgo). Linux Secret Service can come later. The phase 1 master key uses the same store.

**TLS is required.** The client refuses plain HTTP except to localhost and has no skip-verify option. The server trusts `X-Forwarded-For` only from configured proxies. The self-hosting docs show a reverse proxy example (Caddy).

**Last-seen data:** time (updated at most every 5 minutes) and the most recent IP only, no history. IP recording is on by default and can be turned off (`record_ip: false`). `changes.device_id` records which device made each change, for `cloudfs log`.

## Alternatives considered

- **Bootstrap secret in an environment variable** — long-lived, and leaks through config files, shell history and logs.
- **First device to connect claims the server** — anyone who finds a fresh server first owns it.
- **Server prints a code at startup** — puts a secret in logs by default.
- **RFC 8628 device flow / approve-from-a-trusted-device** — needs a browser login or an approval step and a polling endpoint; v1 has neither. `public_key` leaves room for an approval flow in phase 1.
- **26-character code with no attempt limits** — simpler server code, but awkward to type between machines.
- **JWT access tokens** — stateless verification doesn't help when every request hits Postgres anyway, and revocation needs a denylist.
- **Access + refresh tokens with rotation and reuse detection** — one lost response can lock out a headless daemon.
- **mTLS** — breaks when TLS ends at a reverse proxy or the AWS load balancer.
- **Per-request signatures (RFC 9421)** — more moving parts; a later upgrade via `public_key`.
- **Absolute token expiry** — forces re-enrolling unattended servers.
- **`99designs/keyring`** — last release 2022, last push 2024-05.

## Consequences

- Schema (all additive): `users`, `devices` (incl. `token_hash`, `public_key`, `enrolled_by_device_id`, `last_seen_at`, `last_seen_ip`, `revoked_at`), `enrollment_codes`, and `changes.device_id`.
- On AWS, creating the first code needs ECS Exec or a one-off task running the admin CLI. Fine for a rare action.
- A stolen enrolled laptop can create codes for new devices. Mitigations: `enrolled_by` is recorded, and revoking cascades on request.
- **New user data:** device name (also shown in conflicted-copy filenames, ADR-0005), platform, client version, token and code hashes, last-seen time and IP, which device enrolled which, which device made each change, and IPs of failed enrollments in server logs. No privacy policy exists; a hosted deployment would need these listed in one.
