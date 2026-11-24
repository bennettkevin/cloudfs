# Spike 08: Device auth

- **Issue:** #8
- **Status:** Decided. See [ADR-0018](../adr/0018-device-enrollment-and-tokens.md): all recommendations accepted on 2026-11-24, including `changes.device_id`. Library status (go-keyring, 99designs/keyring) re-checked 2026-11-24.
- **Question:** How does a headless client daemon prove which device it is to the CloudFS API, how does a fresh self-hosted server get its first device, how are more devices added, and how is a lost device cut off? Covers bootstrap, enrollment codes, token type, revocation, token storage on the device, authorizing API calls (including presigned URL issuance and the change signal), TLS, and the user data this adds.

Facts below were checked on 2026-11-24 unless marked *unverified*. Three kinds of evidence are used:
- **Docs:** RFCs, OWASP cheat sheets, AWS and vendor documentation, fetched on 2026-11-24.
- **Source:** reading library code from the Go module proxy (go-keyring v0.2.8, 99designs/keyring v1.2.2, coder/websocket v1.8.15).
- **Probe:** a small offline Go program in the scratchpad (Go 1.27.4, darwin/arm64, `golang.org/x/crypto` v0.59.0). No network calls, no services started.

---

## 0. Decisions this builds on

- **Single user, many devices.** Devices must authenticate, but there is no sign-up and no per-user isolation. Don't make a later `owner_id` impossible ([ADR-0002](../adr/0002-single-user-first.md)).
- **Auth is separate from encryption.** Revoking a device's login stops it syncing but doesn't take away keys it already holds ([ADR-0007](../adr/0007-end-to-end-encryption.md), design §11). Key hierarchy and new-device key onboarding are designed in phase 1.
- **Presigned URLs.** The API issues 15-minute presigned PUT/GET URLs for chunks; clients never hold store credentials ([ADR-0015](../adr/0015-direct-presigned-chunk-transfer.md)). Commits carry a `commit_id` ([ADR-0016](../adr/0016-upload-reservations-and-idempotent-commits.md)).
- **Change signal.** Devices learn about new changes through a long-poll/WebSocket "come fetch" signal, then pull with a cursor ([ADR-0003](../adr/0003-sync-model.md)).
- **Clients** are a headless CLI daemon on macOS and Linux. macOS builds already use cgo; Linux builds are pure Go ([ADR-0014](../adr/0014-local-change-detection.md)). **No web UI in v1** (design §4).
- **Deployments:** self-hosters run Compose (API, Postgres, Garage) behind their own reverse proxy. Phase 2 runs on AWS (ECS Fargate, RDS, S3, ALB). Phase 4 adds a WireGuard private network.

---

## 1. Bootstrap: the first device on a fresh server

**Jargon:**
- **Enrollment** is the act of turning a new machine into a known device: the server creates a `devices` row and the machine ends up holding a credential (a **token**) for it.
- An **enrollment code** is a short, one-time secret that lets exactly one machine enroll, once, before it expires.
- The **admin CLI** is a subcommand of the server binary (e.g. `cloudfs-server admin enroll`) that talks straight to Postgres, not through the HTTP API. Whoever can run it already controls the server.

There is no sign-up page and no web UI, so the first credential has to come from somewhere else.

| Option | How it works | Risks |
|---|---|---|
| **A. Admin CLI prints a one-time code** | Operator runs `docker compose exec api cloudfs-server admin enroll`. It stores a hashed code with a 15-minute expiry and prints it. The new device runs `cloudfs login https://cloudfs.example` and pastes the code. | Needs a shell on the server. On AWS that means ECS Exec or a one-off task (below). If run as a one-off task, the code lands in CloudWatch Logs; it's single-use and expires in 15 min, so that's acceptable. |
| **B. Server prints a code at startup when no devices exist** | Like A, but automatic: on boot with zero active devices, the server logs a fresh code. (Jenkins does something similar with its initial admin password; *unverified*.) | Code is written to logs every restart until claimed. Logs are often shipped elsewhere (CloudWatch, log aggregators) and kept longer than the code lives. Silent: the operator may not notice the window is open. |
| **C. Env-var initial secret** | `CLOUDFS_BOOTSTRAP_TOKEN` set in `.env` or Secrets Manager. Accepted only while zero active devices exist. | Long-lived secret that sits in config. Leaks via `docker inspect`, process environment, a committed `.env`, or CI logs. Must be disabled after first use, which is easy to get wrong. Re-opens if all devices are revoked. |
| **D. First-come claim** | The first client to connect becomes the owner. | A server exposed to the internet before the owner claims it can be taken over by anyone who finds it. New hosts are found quickly, e.g. via public Certificate Transparency logs as soon as a Let's Encrypt certificate is issued (*reasoned, timing unverified*). Not acceptable for a service on the public internet. |

**AWS (phase 2) for option A.** RDS sits in a private subnet, so the admin CLI must run inside the VPC:
- **ECS Exec** (`aws ecs execute-command`) opens a shell in a running task. Needs `enableExecuteCommand` on the service and SSM permissions on the task role (*from prior knowledge, unverified; check in phase 2*).
- **One-off task** (`aws ecs run-task` with a command override). Output goes to CloudWatch Logs.
- Either one is a manual step, which conflicts with design §5's "no manual console steps" only for the deploy itself, not for enrolling a device. Running it from the AWS CLI (not the console) keeps it scriptable.

**Leaning: A.** It is explicit, needs no long-lived secret, and is the same mechanism used later for every other device (section 2). Whoever can run it already owns the database, so it adds no new trust. B and C are conveniences with real leak paths. D is ruled out.

---

## 2. Enrolling another device

### 2.1 Who creates the code, and which way it flows

| Flow | How it works | Good | Bad |
|---|---|---|---|
| **Push (code from a trusted side)** | Admin CLI **or an already-enrolled device** (`cloudfs devices add`) creates a code. The user types or pastes it on the new device, which redeems it for a token. | One unauthenticated endpoint (`POST /v1/enroll`). No polling, no pending state. Same code path for bootstrap and later devices. | The code travels from the trusted side to the new one, so whoever sees it can use it (within 15 min, once). |
| **Pull (approve a request), RFC 8628-shaped** | New device asks the server for a request, shows a short code, and polls. The user runs `cloudfs devices approve ABCDE-FGHIJ` on an enrolled device or the admin CLI. The new device's poll then returns its token. | The long secret (the device's polling secret) never leaves the new device. The new device can attach a public key that the approver sees and checks, which is a natural place for E2E key handoff later. | Anyone can create pending requests (spam; needs a cap). Polling. Approval phishing: an attacker starts a request on their own machine and talks the user into approving it (RFC 8628 §5.4). More states to test. |

### 2.2 Does RFC 8628 (OAuth 2.0 Device Authorization Grant) fit?

**Jargon:** RFC 8628 is the "go to example.com/device and enter this code" flow used by smart TVs and CLIs such as `gh auth login`.

- The device asks for a `device_code` and a `user_code`, shows the user a `verification_uri`, and polls the token endpoint every `interval` seconds (default 5; `slow_down` adds 5 s) until the user approves (RFC 8628 §3.2, §3.5).
- The user approves **in a browser, on another device, where they are already logged in** (§1). CloudFS has no web UI and no user login, so there is nothing for the browser to show.
- So the standard grant does **not** fit as-is. Its *shape* fits: replace "browser at `verification_uri`" with "CLI on a trusted device". That is the pull flow above.
- Its security guidance applies to any short typed code (next subsection).

### 2.3 QR codes

A QR code is just another way to carry the same code (or a `cloudfs://enroll?server=…&code=…` link). It helps when a phone is involved; v1 has no mobile client, and both ends are terminals. **Not needed in v1.** A terminal QR renderer can be added later without changing the protocol.

### 2.4 Leaving room for E2E key onboarding

Research 03 §3 already sketches the passphrase model: a new device logs in, fetches the **wrapped master key** from the server, and the user types the passphrase. That needs nothing from auth except "this device is logged in".

A later device-to-device handoff (an enrolled device wraps the master key to the new device's public key, so no passphrase is typed) needs the new device to have a keypair the other side can trust. To keep that door open cheaply:
- Allow an **optional `public_key` field** in the enrollment request and a nullable `devices.public_key` column. Unused in v1.
- Keep enrollment codes single-purpose (auth only). Don't use the code itself as key material in v1.
- If phase 1 chooses device-to-device handoff, the pull flow (2.1) or a PAKE (a password-authenticated key exchange, as used by magic-wormhole, where a short code becomes a strong shared key) are the places to look. That's a phase 1 decision, not this spike's.

### 2.5 Code format, entropy, expiry, brute force

**Jargon:** **entropy** is how many bits of guessing a secret needs. 20 possible letters per character gives log2(20) ≈ 4.32 bits per character.

RFC 8628 §5.1 and §6.1 guidance:
- An 8-character code from a 20-letter set has about 34.5 bits. That is only safe **combined with rate limiting** so an attacker gets just a few guesses in the code's lifetime.
- Suggested alphabet: `BCDFGHJKLMNPQRSTVWXZ` (no vowels, so no accidental words; no 0/O or 1/I confusion). Case-insensitive input.
- `expires_in` short enough to limit misuse, long enough to use (§5.4).

**Proposed CloudFS code:**
- **10 characters** from that 20-letter alphabet, shown as `XXXXX-XXXXX`. 20^10 ≈ 1.0 × 10^13, about **43.2 bits** (probe output example: `STWVG-HCZZK`).
- **15-minute expiry. Single use.** At most **5 unexpired codes** at a time.
- **Global failure cap:** every failed redemption increments a counter on every unexpired code; a code with **10 failures** is dead. So each code faces at most 10 guesses in total, from anyone, anywhere. The chance an attacker guesses a given code is at most 10 / 10^13 = 1 in 10^12. This holds across several API tasks on AWS because the counter lives in Postgres.
- **Per-IP rate limit** on the enroll endpoint as well (e.g. 5 per minute, `golang.org/x/time/rate` v0.16.0, released 2026-08-19). It protects the server's CPU (section 7), not the code; the global cap protects the code.
- **Alternative:** a long code, e.g. `crypto/rand.Text()`, 26 base32 characters, at least 128 bits (Go docs). No brute-force concern, plain SHA-256 is enough, no caps needed. Fine when copy-paste works (SSH from a laptop); tedious to type between two machines with no shared clipboard. See decision 3.

**Lost redemption response.** If the network drops the reply, the code is already used and a device row exists whose token nobody has. Two ways out:
- the user creates a new code and revokes the orphan row (simple, slightly annoying), or
- **the client generates its own token** (32 bytes from `crypto/rand`) and sends only its SHA-256 in the enrollment request. A retry with the same code and the same hash returns success again. The token never appears in any HTTP response. This mirrors ADR-0016's `commit_id` idea. **Leaning: client-generated token.** The client is CloudFS's own code, so trusting it to use `crypto/rand` is reasonable.

### 2.6 Who may create codes

Any enrolled device creating codes is convenient, but a **stolen device could enroll an attacker's machine** before the owner notices, and that second device would survive revoking the stolen one.

Mitigation: record `enrolled_by_device_id`. `cloudfs devices list` shows it, and revoking a device lists the devices it enrolled and asks whether to revoke them too. Alternative: only the admin CLI creates codes (safer, but on AWS every new device needs ECS Exec). See decision 4.

---

## 3. Token types

**Jargon:**
- An **opaque token** is a random string with no meaning inside it. The server looks it up to find out who it belongs to.
- A **JWT** (JSON Web Token) is a signed blob that carries claims (device ID, expiry). The server can check the signature without a database lookup; that's what "stateless" means.
- **Access + refresh tokens:** a short-lived access token for API calls plus a long-lived refresh token used only to get new access tokens. **Rotation** means every refresh issues a new refresh token and invalidates the old one.
- **mTLS** (mutual TLS) means the client also presents a certificate during the TLS handshake.
- **Request signing:** each device holds a private key (e.g. Ed25519) and signs each request (RFC 9421 HTTP Message Signatures) or a login challenge.

| | Opaque token, hashed in Postgres | JWT | Access + refresh with rotation | mTLS client certs | Per-device keypair signing |
|---|---|---|---|---|---|
| Server check per request | Hash, one indexed lookup | Verify signature (no DB) | Verify access token; refresh occasionally | TLS handshake + map cert fingerprint to device | Verify signature + replay check |
| Immediate revocation | **Yes**: delete or mark the row | **No** without a denylist; OWASP notes a denylist undercuts statelessness and suggests ordinary sessions instead | Only after the access token expires, unless access tokens are also checked in the DB | Needs a DB check of the fingerprint (or CRLs) | Yes, delete the public key |
| What a DB leak gives an attacker | Nothing usable (SHA-256 of 256-bit secrets) | Nothing, unless the signing key is in the DB | Hashes only | Public certs only | Public keys only |
| What a stolen device file gives | The token (until revoked) | The token (until expiry) | The refresh token (until revoked) | The private key | The private key |
| Works behind a reverse proxy / ALB | Yes | Yes | Yes | **Awkward.** TLS ends at Caddy/Traefik/ALB. ALB supports mTLS "verify" or "passthrough" mode with a trust store and forwards the cert in `X-Amzn-Mtls-*` headers (AWS docs), so the app must trust a header. Each self-hoster's proxy is configured differently. | Yes (signature is in HTTP headers) |
| Moving parts | Fewest | Signing key management, `alg` pitfalls, a denylist anyway | Rotation, race on lost refresh responses, reuse detection | A private CA, issuing and renewing certs, proxy config | Canonicalization, `created`/`nonce`, clock skew, replay store |

### Why not access + refresh?

RFC 9700 (OAuth 2.0 Security BCP, January 2025) says refresh tokens for public clients "MUST be sender-constrained or use refresh token rotation" (§2.2.2). With rotation and reuse detection, if an old refresh token is presented, the server can't tell attacker from client and revokes the active one, so the legitimate client must re-authorize (§4.14).

For CloudFS:
- The benefit of short-lived access tokens is limiting a leaked access token's lifetime. Here there's **one resource server** (the API), it already does a Postgres lookup on every request, and the refresh token sits on the same disk as the access token. The split buys little.
- **Reuse detection hurts a headless daemon.** A lost refresh response (laptop sleeps mid-request, network drop) looks exactly like reuse. The daemon is locked out and needs re-enrolling, possibly on a server in a closet. That's a bad failure mode for a sync tool.

### Why not JWT?

Its one advantage, no DB lookup, doesn't apply: every CloudFS request touches Postgres anyway. Immediate revocation then needs a denylist (a DB lookup), at which point an opaque token is simpler.

### Why not mTLS or request signing in v1?

Both are stronger in one way: the secret never travels over the wire, so a TLS-terminating proxy or a logging bug can't leak it. But:
- mTLS fights the deployment model (TLS ends at the proxy in both Compose and AWS).
- Request signing has many details to get right (RFC 9421, February 2024: covered components, `created`, `expires`, `nonce`; replay is discussed in §7.2.2, *not read in full*). It's a lot for a Go learner's first auth layer.
- **Upgrade path:** the nullable `devices.public_key` column (2.4) could later back a "sign a challenge to get a token" login without changing the rest of the system.

### Recommended: one long-lived opaque token per device

- **Format:** `cfs_` + base64url of 32 random bytes = 47 characters (probe: `cfs_b0_jdPXbWa0Q-…`). 256 bits, far above OWASP's 64-bit minimum for session IDs. The `cfs_` prefix makes leaked tokens easy to grep for and to add to secret scanners (GitHub's reason for `ghp_` prefixes, GitHub blog 2021-04-05).
- **Sent as** `Authorization: Bearer <token>` over TLS only. **Never in a URL**: RFC 6750 §5.3 says bearer tokens "SHOULD NOT be passed in page URLs" because URLs get logged.
- **Stored** server-side as `SHA-256(token)` in a unique index on `devices.token_hash`. Looked up on every request.
- **No absolute expiry, optional idle expiry.** OWASP's session guidance asks for idle and absolute timeouts, but these are device credentials (like Nextcloud app passwords or Tailscale node keys), not browser sessions. Tailscale node keys default to 180 days and admins can disable expiry for servers (Tailscale docs). Proposed: revoke a device automatically if unseen for **180 days**, configurable, `0` = never. See decision 6.
- **Rotation:** `cloudfs devices rotate-token` (client makes a new token, sends its hash, server swaps) can be added later. Not needed for v1.

---

## 4. Revoking a lost device

**What revocation does:** `cloudfs devices revoke laptop` (from any enrolled device or the admin CLI) sets `revoked_at` and clears `token_hash` in one statement.

| Effect | How fast | Notes |
|---|---|---|
| API calls fail with 401 | **Next request.** No token cache, so no delay, on any number of API tasks | If a cache is ever added, keep its TTL ≤ 30 s and document it |
| No new presigned URLs | Next request (URL issuance is an API call) | |
| **Already-issued presigned URLs keep working** | **Up to 15 min** (ADR-0015) | SigV4 URLs can't be revoked one by one. Worst case: up to 256 PUT URLs that can only write the exact bytes already signed (harmless), and GET URLs for chunk ciphertext (E2E-protected). Emergency only: rotating the signer key (Garage key, or the AWS role session) kills all URLs for every device. |
| Open change-signal connections | **Long-poll:** next poll (each is a new authenticated request, keep it ≤ 60 s). **WebSocket:** the handshake is the only auth, so the server must close the socket. Simplest: re-check the device row every 60 s inside the connection loop. | The signal only says "something changed", so a few extra seconds leak only timing |
| In-progress commit | Fails at commit (401). Uploaded chunks become orphans and the 24 h GC removes them (ADR-0016) | |

**What revocation does not do** (ADR-0007, research 03 §4):
- The device still has the **master key** and every file it already synced. Anyone holding the device can read those.
- If an attacker also gets ciphertext some other way (a leaked bucket, an old URL), the device's keys can decrypt it.
- **Real cut-off needs key rotation:** a new master key, re-wrapped file keys, and (depending on the hierarchy) re-encryption. That is a **phase 1 key-hierarchy question**. This spike only requires that the device list makes it obvious when a rotation is advisable.
- No **remote wipe** in v1 (Nextcloud has one, *not checked*). A thief can keep the daemon offline anyway, so it's of limited value.

**Client behaviour on revocation:** on a 401 with error code `device_revoked`, the daemon stops syncing, keeps all local files, deletes its token, and logs a clear message. It never deletes local data because of an auth failure.

**Recovering from "all devices revoked":** the admin CLI can always enroll a new device, so this is not a lockout.

**Listing devices:** `cloudfs devices list` shows name, short ID, platform, enrolled at, enrolled by, last seen (time and IP), revoked at. The current device is marked. Names are unique among active devices and can be renamed (`cloudfs devices rename`).

**Last-seen updates** without a write on every request:
```sql
UPDATE devices SET last_seen_at = now(), last_seen_ip = $2
WHERE id = $1 AND last_seen_at < now() - interval '5 minutes';
```

**Device names already reach other devices:** conflicted copies are named `<name> (conflicted copy, <device>, <date>)` (ADR-0005). So whatever the user names a device appears in synced filenames. Worth saying in the `devices add` help text.

---

## 5. Token storage on the device

| Store | macOS | Linux desktop | Linux headless server |
|---|---|---|---|
| OS keychain | **Keychain** (login keychain) | **Secret Service** over D-Bus (GNOME Keyring, KWallet) | Usually **absent**: no D-Bus session, no keyring daemon, nobody to unlock it at boot |
| File, mode 0600 | Works | Works | **Works**; what Syncthing (key files in its config dir) and Tailscale (`tailscaled.state`, *path unverified*) do |

Notes:
- A daemon started by **launchd before login** (a LaunchDaemon, not a LaunchAgent) can't use the login keychain (*unverified*). A per-user LaunchAgent can.
- Keychain protects against other *users* and against copying the disk image, not against malware running as the same user. A 0600 file in a home directory on an encrypted disk (FileVault, LUKS) is close in practice for a headless daemon.
- The token is no more sensitive than the **E2E master key**, which also has to live on the device. Whatever phase 1 picks for the key should hold the token too. One `SecretStore` interface for both.

### Go libraries (checked 2026-11-24)

| Library | Licence | Latest | Maintenance | macOS backend | cgo? | Linux backend | Notes |
|---|---|---|---|---|---|---|---|
| `github.com/zalando/go-keyring` | MIT | v0.2.8 (2026-03-23) | Active: last commit 2026-11-17, 36 open issues | Runs `/usr/bin/security -i` and writes the command to its **stdin** (so the secret isn't in the process list) | **No** (source: `os/exec`) | Secret Service via `godbus/dbus/v5`; expects the `login` collection to exist (README) | macOS limit about 3,000 bytes per item (`ErrSetDataTooBig`); a 47-char token is fine. The item's access list trusts `/usr/bin/security`, so other processes of the same user can likely read it via that tool without a prompt (*reasoned, unverified*). |
| `github.com/99designs/keyring` | MIT | v1.2.2 (2022-12-19) | **Stale:** last push 2024-05-07, 43 open issues | Native Keychain API via `99designs/go-keychain` | **Yes** (`//go:build darwin && cgo`) | Secret Service, KWallet, `pass`, keyctl, encrypted file | Many backends, but unmaintained |
| `github.com/byteness/keyring` | MIT | v1.16.1 (2026-11-17) | Active; README: "a maintained fork of 99designs/keyring … maintained on best effort basis" (27 stars) | Native Keychain (cgo, *assumed from parent, not checked*) | Likely yes on macOS | Same as 99designs plus oo7, passage, 1Password | Small project; more surface than CloudFS needs |

cgo on macOS is already accepted (ADR-0014), so cgo isn't the deciding factor. Maintenance and size are.

**Leaning:**
- `SecretStore` interface with two backends: **file** (default on Linux, fallback everywhere) and **macOS Keychain** via `zalando/go-keyring`.
- Skip Secret Service in v1. It's absent on headless Linux, and on desktops a 0600 file in an encrypted home is comparable for this threat model. Add it later behind the same interface if wanted.
- File rules: `$XDG_CONFIG_HOME/cloudfs/` (default `~/.config/cloudfs/`) with dir `0700` and file `0600`; write via temp file + rename; **refuse to start if the file is group- or world-readable** (as `ssh` does with keys). Also store the server URL and device ID next to it (not secret).

---

## 6. Authorizing API calls

### Every endpoint

- One HTTP middleware wraps every route except `POST /v1/enroll` and a health check. It:
  1. reads `Authorization: Bearer cfs_…` (rejects anything else, including tokens in the query string),
  2. computes SHA-256, looks up `devices WHERE token_hash = $1 AND revoked_at IS NULL`,
  3. puts a `Principal{UserID, DeviceID}` into the request `context.Context`,
  4. updates last seen (throttled, section 4).
- Failures return 401 with `WWW-Authenticate: Bearer` and a JSON error code (`invalid_token` or `device_revoked`).

### Ready for multi-user later (ADR-0002)

- Add a `users` table now with **one row**, created at bootstrap, and `devices.user_id NOT NULL` referencing it. Additive, cheap, and the principal already carries a `UserID`.
- Handlers and the data layer take the `Principal` (not just a device ID) as an argument, even though v1 never checks ownership. Adding `owner_id` to `nodes` later then has one obvious place to enforce it.
- Don't put user IDs into object keys (ADR-0002, ADR-0015 already derives keys from chunk IDs only).
- Per the repo's security practice ("verify authorization server-side"): in v1 the check is "valid, non-revoked device". Multi-user would add "and this principal owns the resource". The client never supplies its own device ID or user ID; both come from the token.

### Presigned URL issuance

- The upload-URL and download-URL endpoints are ordinary authenticated endpoints. A valid device gets URLs; a revoked one doesn't.
- The server already derives object keys from chunk IDs (ADR-0015), so a device can't ask for an arbitrary key.
- Optionally record `device_id` on reservation rows or on `changes` rows for audit and debugging ("which device committed this?"). Not required. Flagged in section 8.

### Change signal (long-poll or WebSocket)

- **Same bearer token in the `Authorization` header of the handshake.** Browsers can't set headers on WebSocket handshakes, which is why web apps put tokens in URLs; CloudFS's Go client can. `coder/websocket` v1.8.15 (2026-06-15) has `DialOptions.HTTPHeader` for exactly this (source). `gorilla/websocket` v1.5.3 (2024-06-14) also works.
- Re-check the device every 60 s while connected (section 4), or prefer **long-poll** for v1: each poll is a normal authenticated request, so revocation needs no extra code. That's a reason to start with long-poll (a spike 01/protocol detail, not decided here).

### TLS

- **API and Garage both HTTPS** (design §11). The client refuses to send a token over `http://` unless the host is `localhost` (for development).
- **Self-hosters:** terminate TLS at a reverse proxy. Caddy gets and renews Let's Encrypt certificates automatically, given DNS pointing at the host and ports 80/443 open (Caddy docs). Traefik and nginx + certbot also work. The self-hosting docs should show one complete Caddy example with both hostnames (`cloudfs.example`, `s3.cloudfs.example`).
- **Real client IP:** behind a proxy, the API sees the proxy's IP. Trust `X-Forwarded-For` only from configured proxy addresses (`trusted_proxies` setting), or last-seen IP and per-IP rate limits are wrong or spoofable.
- **Client trust:** Go's default certificate verification, plus an optional `ca_file` for self-hosters with a private CA. **No "skip verification" flag.**
- **Phase 2:** ALB with an ACM certificate; ALB sets `X-Forwarded-For`.
- **Phase 4 (WireGuard):** keep TLS and device tokens. The tunnel restricts who can reach the API; it doesn't replace device auth. Revoking a device should also remove its WireGuard peer (another per-device item for that phase).

---

## 7. Go implementation notes

| Need | Use | Why |
|---|---|---|
| Random token | `crypto/rand.Read` into 32 bytes, then `base64.RawURLEncoding` | `rand.Read` "never returns an error, and always fills b entirely" (Go docs, Go 1.24+ behaviour). `crypto/rand.Text()` is an alternative: base32, ≥ 128 bits |
| Enrollment code characters | `crypto/rand.Int(rand.Reader, big.NewInt(20))` per character | Unbiased choice from the 20-letter alphabet. **Never `math/rand`.** |
| Hash a token (256-bit) | `crypto/sha256` | High-entropy secrets can't be brute-forced, so a fast hash is enough and allows an indexed lookup. ~55 ns per hash (probe) |
| Hash an enrollment code (43-bit) | `golang.org/x/crypto/argon2.IDKey` with OWASP's minimum: 19 MiB, t=2, p=1, 16-byte salt | 43 bits is brute-forceable offline from a leaked DB if a fast hash is used (≈10^13 SHA-256s is minutes to hours on a GPU, *estimate*). Argon2id at ~26 ms per guess (probe) makes that infeasible. With ≤ 5 active codes, the server just tries each one |
| Compare secrets in memory | `crypto/subtle.ConstantTimeCompare` | Time depends only on length (Go docs). Use it for argon2 outputs |
| DB lookup by hash | Plain `WHERE token_hash = $1` | The attacker doesn't control the hash, so index timing reveals nothing useful (*reasoned*) |
| Rate limiting | `golang.org/x/time/rate` (v0.16.0) per IP, in memory | Plus the Postgres failure counter for the cross-task cap (2.5) |
| WebSocket client header | `coder/websocket` `DialOptions.HTTPHeader` | Token stays out of the URL |

Everything is standard library except `x/crypto` (argon2), `x/time` (rate), the WebSocket library and the keyring. **No auth framework, OAuth server or JWT library needed.**

Logging rules: never log tokens, codes, or `Authorization` headers. Log `device_id`, enroll/revoke events, and failed redemptions with IP.

### Proposed schema (all additive)

```sql
CREATE TABLE users (
  id         uuid PRIMARY KEY,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE devices (
  id                    uuid PRIMARY KEY,
  user_id               uuid NOT NULL REFERENCES users(id),
  name                  text NOT NULL,
  platform              text,          -- e.g. "darwin/arm64"
  client_version        text,
  token_hash            bytea UNIQUE,  -- SHA-256; NULL once revoked
  public_key            bytea,         -- reserved for phase 1, unused in v1
  enrolled_by_device_id uuid REFERENCES devices(id),  -- NULL = admin CLI
  created_at            timestamptz NOT NULL DEFAULT now(),
  last_seen_at          timestamptz,
  last_seen_ip          inet,
  revoked_at            timestamptz
);
CREATE UNIQUE INDEX devices_active_name ON devices (user_id, lower(name)) WHERE revoked_at IS NULL;

CREATE TABLE enrollment_codes (
  id                    uuid PRIMARY KEY,
  code_hash             bytea NOT NULL,  -- argon2id output
  salt                  bytea NOT NULL,
  created_by_device_id  uuid REFERENCES devices(id),  -- NULL = admin CLI
  created_at            timestamptz NOT NULL DEFAULT now(),
  expires_at            timestamptz NOT NULL,
  failed_attempts       int NOT NULL DEFAULT 0,
  used_at               timestamptz,
  used_by_device_id     uuid REFERENCES devices(id)
);
```

Used and expired codes can be deleted by the existing worker after a day.

### Enrollment flow (recommended push variant)

```
admin CLI / enrolled device              API (Postgres)                     new device
  create code ──────────────────────────► insert enrollment_codes (argon2id, 15 min)
  ◄── "STWVG-HCZZK" (shown once)
  user types/pastes code ───────────────────────────────────────────────────► cloudfs login https://cloudfs.example
                                                                               generate token T (32 random bytes)
                                         ◄── POST /v1/enroll {code, name, platform, version, sha256(T), public_key?}
                                         rate limit; try each active code (≤5) with argon2id
                                         fail: failed_attempts += 1 on all active codes → 401
                                         ok: one tx: mark code used, insert device(token_hash = sha256(T))
                                         retry with same code + same hash → same success
                                         ──► {device_id, user_id}
                                                                               store T in SecretStore; start syncing
```

---

## 8. New user data stored or transmitted (flag for the plan)

| New data | Where | Notes |
|---|---|---|
| **Device name** (user-chosen) | Postgres `devices.name`; shown on every device; appears in conflicted-copy filenames (ADR-0005) | May contain a real name ("Alex's MacBook"). Help text should say names are visible on all devices and in filenames |
| **Platform and client version** | Postgres | For support and the device list. Low sensitivity |
| **Token hash** (SHA-256) | Postgres | Not reversible. The token itself only on the device |
| **Enrollment code hash** (argon2id) and salt | Postgres, ≤ 15 min useful life, deleted after a day | |
| **Last-seen time** | Postgres, updated at most every 5 min | Reveals when each device is online |
| **Last-seen IP address** | Postgres (latest only, no history) | **Personal data** in many jurisdictions. Useful to spot a stolen device. Proposed: on by default, `record_ip: false` turns it off. See decision 7 |
| **IPs of failed enrollments and auth events** | Server logs (and CloudWatch Logs in phase 2) | Log retention is a phase 3 setting |
| **Enrolled-by relationship** | Postgres | |
| **Device public key** (reserved) | Postgres, nullable | Not collected in v1 |
| **Token on the device** | macOS Keychain or a 0600 file | Local to the user's machine |
| Optional **`device_id` on `changes`** | Postgres | Only if chosen ("which device made this change"). Reveals per-device activity history |

**Privacy policy / ToS:** the repo has none (checked: no privacy or terms file in `cloudfs/`; the README tells self-hosters who serve others to provide their own). Nothing to update now. If a hosted deployment ever exists, device names, last-seen IP/time and log IPs belong in its policy, next to the object-store IP note from ADR-0015.

---

## 9. How comparable tools do it

| Tool | Device identity | Adding a device | Revoking | Takeaway for CloudFS |
|---|---|---|---|---|
| **Syncthing** | Each device makes an Ed25519 key and self-signed cert; the **device ID is the SHA-256 of the cert** (base32 with check digits) | Add the other device's ID to config on both sides | Remove the ID from config: after the TLS handshake, connections from IDs not in config are dropped | Mutual TLS works well peer-to-peer, without proxies. CloudFS has a central server behind proxies, so a bearer token is a better fit. Syncthing's docs note long IDs are hard to verify by hand and suggest a one-time PIN, the same idea as an enrollment code |
| **Nextcloud desktop** | Per-device **app password** | **Login Flow v2:** client POSTs `/index.php/login/v2`, opens the browser to log in, polls for up to 20 minutes, receives server + login name + app password once | Revoke each device in the web UI's Security settings; client calls `DELETE /ocs/v2.php/core/apppassword` when removing an account | Closest match: long-lived per-device opaque credential, revocable individually. The flow needs a browser and a web login, which CloudFS lacks; the enrollment code takes the browser's place |
| **Seafile** | Per-login API token (`POST /api2/auth-token/`), sent as `Authorization: Token …` (or `Bearer` from v11) | Username + password (+ OTP header with 2FA) | Each token appears under "linked devices"; disconnecting a device expires its token | Same per-device token idea. *From search summaries and a forum thread, not checked against Seafile docs or source* |
| **restic rest-server** | None per device: HTTP Basic auth against a `.htpasswd` file (bcrypt recommended, SHA "insecure by today's standards") | Admin adds a user line | Remove the line | Simplest possible model; no device notion. Shows why per-device credentials are worth having: revoking one machine means changing the shared password everywhere |
| **Tailscale** | Per-node **node key** | Browser login, or an **auth key**: one-off, reusable, ephemeral, pre-approved; 1–90 day expiry | Delete the machine. "Revoking a key does not deauthorize nodes using the key." Node keys expire (default 180 days); expiry can be disabled per device | Enrollment secrets and device credentials are separate things with separate lifetimes, exactly as proposed here. A one-off auth key is CloudFS's enrollment code |

---

## Recommendation (accepted 2026-11-24)

1. **Bootstrap and enrollment use one mechanism:** a 10-character one-time code (`XXXXX-XXXXX`, ~43 bits), created by the server's admin CLI or by an enrolled device, valid 15 minutes, single use, at most 5 active, dead after 10 failed attempts in total, plus a per-IP rate limit. Stored as argon2id. No env-var bootstrap, no first-come claim.
2. **Push flow in v1** (code goes from trusted side to new device). Keep an optional `public_key` field and column so an RFC 8628-shaped approve flow or device-to-device key handoff can be added in phase 1 without breaking anything.
3. **One long-lived opaque token per device:** `cfs_` + 32 random bytes, **generated by the client**, only its SHA-256 sent at enrollment and stored in Postgres. Sent as `Authorization: Bearer`, never in a URL. No JWT, no refresh tokens, no mTLS, no request signing in v1.
4. **Revocation is immediate** for API calls and URL issuance (DB lookup every request, no cache). Already-issued presigned URLs live out their ≤ 15 minutes. Long-poll re-auths per poll; WebSockets re-check every 60 s. Revoking warns about devices the revoked one enrolled. Key rotation after a revocation is left to the phase 1 key hierarchy.
5. **Idle expiry 180 days** (configurable, 0 = off); no absolute expiry.
6. **Device storage:** `SecretStore` interface; 0600 file by default (refuse to start if permissions are loose); macOS Keychain via `zalando/go-keyring` (MIT, pure Go). Same store for the phase 1 master key.
7. **Authorization middleware** on every route except enroll and health; `Principal{UserID, DeviceID}` in context; a one-row `users` table and `devices.user_id` now, so multi-user stays additive.
8. **TLS required**, client refuses plain HTTP except localhost, no skip-verify option, `trusted_proxies` for real client IPs, Caddy example in self-hosting docs.
9. **Device list** with name, platform, enrolled-by, last seen (time, IP); rename and revoke from any device or the admin CLI.

---

## Decisions for the user

1. **Bootstrap mechanism.**
   - Options: (A) admin CLI prints a one-time code; (B) server logs a code at startup while no devices exist; (C) env-var initial secret; (D) first-come claim.
   - **Recommended: A.** No long-lived secret, nothing in logs by default, and the same path as every later device. Whoever can run it already controls the database. On AWS it costs an ECS Exec or one-off task per bootstrap, which is fine for a rare action.
2. **Enrollment flow.**
   - Options: push (code from admin CLI or enrolled device); pull (new device shows a code, a trusted device approves, RFC 8628-shaped); both.
   - **Recommended: push in v1.** One unauthenticated endpoint, no polling or pending-request spam, no approval-phishing case. Reserve `public_key` so pull can be added if phase 1 wants device-to-device key handoff.
3. **Code length.**
   - Options: 10 characters from a 20-letter alphabet (~43 bits) with caps and argon2id; 26-character `rand.Text()` (≥ 128 bits) with SHA-256 and no caps.
   - **Recommended: 10 characters.** Easy to type between machines with no shared clipboard. The caps are a few dozen lines and make guessing a 1-in-10^12 event. Pick the long code if you'd rather never type it and keep the code simpler.
4. **Who can create enrollment codes.**
   - Options: admin CLI only; admin CLI and any enrolled device.
   - **Recommended: both**, with `enrolled_by` recorded and a cascade prompt on revoke. Admin-only is safer against a stolen laptop enrolling a hidden device, but makes adding a device on AWS a chore.
5. **Token type.**
   - Options: opaque token hashed in Postgres; JWT; access + refresh with rotation; mTLS; per-device keypair signing.
   - **Recommended: opaque token.** Simplest, immediate revocation, nothing useful in a DB leak, works behind any proxy. The others add moving parts that only pay off with stateless services, many resource servers, or no TLS-terminating proxy. Keypair signing stays a later upgrade via `public_key`.
6. **Token lifetime.**
   - Options: never expires; idle expiry (180 days, configurable); absolute expiry with forced re-enroll.
   - **Recommended: idle expiry, 180 days.** Cleans up forgotten devices without surprising an active headless server. Matches Tailscale's default length. Absolute expiry would force re-enrolling servers in closets.
7. **Recording last-seen IP.**
   - Options: don't store; store latest only (on by default, can be turned off); store a history.
   - **Recommended: latest only, configurable.** Helps spot a stolen device; no history kept. It's new personal data (section 8).
8. **Token storage backends in v1.**
   - Options: file only; file + macOS Keychain; file + Keychain + Linux Secret Service.
   - **Recommended: file + macOS Keychain** (`zalando/go-keyring`). Secret Service is absent on headless Linux, the main Linux target. Add it later behind the interface if wanted.
9. **`users` table now.**
   - Options: one-row `users` table and `devices.user_id` now; nothing until multi-user.
   - **Recommended: now.** Two small additive pieces, and the auth principal is multi-user-shaped from day one (ADR-0002).
10. **Change-signal transport, from an auth point of view.**
    - Options: long-poll; WebSocket with periodic re-check.
    - **Recommended: long-poll first.** Revocation needs no extra code. This only affects auth; the protocol decision belongs with the sync protocol work.
11. **Schema additions** (all additive, safe to run any time): `users`, `devices`, `enrollment_codes`. Optional `changes.device_id`: recommended **yes** only if "which device made this change" is wanted in `cloudfs log`; it's new per-device activity data.

---

## References

Fetched or read directly (2026-11-24):
- RFC 8628, OAuth 2.0 Device Authorization Grant (§1, §3.2, §3.5, §5.1, §5.4, §6.1): https://www.rfc-editor.org/rfc/rfc8628.html
- RFC 9700, Best Current Practice for OAuth 2.0 Security, January 2025 (§2.2.1, §2.2.2, §4.10.1, §4.14): https://www.rfc-editor.org/rfc/rfc9700.html
- RFC 6750, OAuth 2.0 Bearer Token Usage (§2.3, §5.3): https://www.rfc-editor.org/rfc/rfc6750.html
- RFC 9421, HTTP Message Signatures, February 2024 (abstract and §2.3 parameters; §7.2.2 replay not read): https://www.rfc-editor.org/rfc/rfc9421.html
- OWASP Session Management Cheat Sheet (64-bit minimum, CSPRNG, idle and absolute timeouts): https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html
- OWASP JSON Web Token Cheat Sheet (revocation, denylists): https://cheatsheetseries.owasp.org/cheatsheets/JSON_Web_Token_Cheat_Sheet.html
- OWASP Password Storage Cheat Sheet (Argon2id 19 MiB, t=2, p=1; pepper): https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html
- AWS, mutual TLS on Application Load Balancer (verify/passthrough, `X-Amzn-Mtls-*` headers): https://docs.aws.amazon.com/elasticloadbalancing/latest/application/mutual-authentication.html
- Caddy automatic HTTPS: https://caddyserver.com/docs/automatic-https
- GitHub blog, "Behind GitHub's new authentication token formats" (2021-04-05): https://github.blog/engineering/platform-security/behind-githubs-new-authentication-token-formats/
- Syncthing, device IDs: https://docs.syncthing.net/dev/device-ids.html
- Nextcloud Login Flow (v2): https://docs.nextcloud.com/server/latest/developer_manual/client_apis/LoginFlow/index.html
- Nextcloud, managing connected browsers and devices: https://docs.nextcloud.com/server/latest/user_manual/en/session_management.html
- restic rest-server README: https://github.com/restic/rest-server
- Tailscale auth keys: https://tailscale.com/kb/1085/auth-keys
- Tailscale key expiry: https://tailscale.com/kb/1028/key-expiry
- Go standard library docs, Go 1.27.4 (`go doc`): `crypto/rand.Text`, `crypto/rand.Read`, `crypto/subtle.ConstantTimeCompare`
- Go module proxy and GitHub API (versions, release dates, archive status, 2026-11-24): `github.com/zalando/go-keyring` v0.2.8, `github.com/99designs/keyring` v1.2.2, `github.com/byteness/keyring` v1.16.1, `golang.org/x/crypto` v0.59.0, `golang.org/x/time` v0.16.0, `github.com/coder/websocket` v1.8.15, `github.com/gorilla/websocket` v1.5.3
- Source read: go-keyring v0.2.8 `keyring_darwin.go`, `keyring_unix.go`, `keyring.go`, README; 99designs/keyring v1.2.2 `keychain.go`, `go.mod`; byteness/keyring README; coder/websocket `DialOptions`

Search summaries only (not fetched in full):
- Seafile auth-token and linked devices (rclone forum thread, Seafile forum, seafile-api.readme.io listing): https://forum.rclone.org/t/always-same-auth-token-config-seafile-with-2fa/26683, https://seafile-api.readme.io/reference/authentication

From prior knowledge, not checked: ECS Exec requirements; Jenkins initial admin password; launchd LaunchDaemon vs login keychain; Tailscale state file path; Nextcloud remote wipe; RFC 8705 (mTLS-bound tokens) and RFC 9449 (DPoP) beyond their mention in RFC 9700.

Local probe (throwaway, not in the repo): `tokprobe/` (token and code generation, SHA-256 and argon2id timing) and `keyprobe/` (keyring module sources).
