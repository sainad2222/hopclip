# Security

## Reporting a vulnerability

Please **do not open a public issue**. Report privately through
[GitHub security advisories](https://github.com/sainad2222/hopclip/security/advisories/new).
You should get a response within a few days. Fixes ship in a new release and are credited in the advisory unless you prefer otherwise.

Only the latest release receives security fixes.

## Security model

| Area | Protection |
| --- | --- |
| Transport | HTTPS terminates at Cloudflare or Caddy. The app port binds to `127.0.0.1` only. HSTS is sent. |
| Passwords | argon2id (19 MiB, t=2, p=1) with a random salt. Unknown usernames cost the same time as wrong passwords. |
| Brute force | Failed logins are limited per client IP and per username (`LOGIN_MAX_ATTEMPTS` per `LOGIN_WINDOW`). |
| Sessions | 256-bit random token in an `HttpOnly; Secure; SameSite=Strict` cookie with the `__Host-` prefix. Only its SHA-256 is stored. Sliding expiry. Changing a password signs out every other device. |
| Revocation | Signing a device out also closes its live event stream. Streams re-check their session every 25 s, so sign-outs from the CLI apply too. |
| CSRF | Per-session `X-CSRF-Token` on every state-changing request, `application/json` required for JSON bodies, and `Sec-Fetch-Site: cross-site`/`same-site` requests rejected. |
| Isolation | Every query is scoped to the signed-in user. Other users' IDs return `404`. |
| Uploaded files | Always served as attachments with `nosniff` and a `sandbox` CSP. Inline preview only for PNG, JPEG, GIF, WebP and BMP whose bytes were sniffed as images. SVG and HTML never render. Filenames are sanitised. |
| Web UI | `script-src 'self'`, no inline script or style, `frame-ancestors 'none'`. User content is only ever inserted with `textContent`. |
| Container | Distroless, non-root (UID 65532), read-only root filesystem, all capabilities dropped, `no-new-privileges`. |

## Out of scope / known limitations

- **Data at rest is not encrypted.** Clips and files are stored as-is on the server's disk. Use disk or volume encryption if you need it.
- **Anyone can lock a username out** for `LOGIN_WINDOW` by failing its password `LOGIN_MAX_ATTEMPTS` times. Devices already signed in are unaffected.
- **The admin sees nothing extra:** admins manage accounts but cannot read other users' clips or files through the app. Whoever runs the server can, of course, read the database.
- Set `CLIENT_IP_HEADER` correctly for your proxy (see [deployment](docs/deployment.md#why-client_ip_header-matters)); otherwise rate limiting treats all clients as one.
