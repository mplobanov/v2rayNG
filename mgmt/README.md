# v2rayNG Family: logs & configs server

This fork (`com.v2ray.ang.family`) adds two things to v2rayNG:

1. **Russian defaults** for fresh installs (see "Defaults" below).
2. **Remote management**: every install reports to `ngmgmt`, a small server that hands out the
   desired configuration and collects state, events, measurements and logs. The administrator (or
   an agent) drives it with the `ngctl` CLI.

```
 phone (v2rayNG Family)                                server <ip>:9443 (ngmgmt, SQLite)
 ├─ VPN daemon process ── MgmtAgent ── sync / long-poll ──▶  /v1/sync  /v1/poll  /v1/logs  /v1/results
 │    logcat stream (xray GoLog + app), probes, net events      │
 └─ WorkManager (:bg) ─── MgmtSyncWorker, every 15 min ─────▶   │ profiles + per-device overrides
      (while the VPN is off)                                     ▼
                                                       Mac / agent: ngctl ─▶ /v1/admin/*
```

The app's own traffic is excluded from its VPN, so the phone reaches the server over its physical
network even when the tunnel is broken; that is also why "direct" probes measure the ISP path.
The channel is HTTPS to a bare IP with a 30-year self-signed certificate whose SPKI hash is pinned in
the app and in `ngctl` (no DNS, no CA, no renewals).

## Files

| path | what |
|---|---|
| `mgmt/server/` | `ngmgmt` server (Go, pure-Go SQLite) and `cmd/ngctl` CLI |
| `mgmt/server/API.md` | API, config-document schema, commands, measurement kinds (also `ngctl help-api`) |
| `mgmt/deploy/` | `deploy.sh` (build + install/upgrade on Moscow), systemd unit, backup script |
| `mgmt/profiles/family.example.json` | example shared profile (the live one stays in `~/.config/ngctl/profiles/`) |
| `mgmt/build-apk.sh` | builds the signed arm64 release APK |
| `V2rayNG/app/src/main/java/com/v2ray/ang/handler/mgmt/` | the app side |
| `V2rayNG/app/src/main/java/com/v2ray/ang/ui/mgmt/` | "Remote management" screen (drawer) |

Secrets and infrastructure details live only on the admin Mac in `~/.config/ngctl/` (mode 700):
`config.json` (server URL, admin token, pin), `enroll_token`, `pin`, `deploy.env` (SSH target),
`profiles/` (live profiles), the release key `v2rayng-family.jks` and its
`signing.properties`. **Back up the release key**: without it the family's installed apps cannot be
updated in place. The server keeps only SHA-256 hashes of the tokens.

## Server operations

- Unit `ngmgmt.service` (user `ngmgmt`, MemoryMax 250M, CPUWeight 20, Nice 5), binary
  `/opt/ngmgmt/ngmgmt`, config `/etc/ngmgmt/config.json`, cert `/etc/ngmgmt/tls.{crt,key}`,
  data `/var/lib/ngmgmt/ngmgmt.db`.
- Daily backup `ngmgmt-backup.timer` 01:45 → `/var/backups/ngmgmt/` (7 kept).
- Retention: logs 14 days / 3M rows, states 30 days, events and measurements 120 days.
- Upgrade: `mgmt/deploy/deploy.sh` (no flags). First install was `deploy.sh --init`.
- Logs: `journalctl -u ngmgmt`. Health: `curl -k https://<server-ip>:9443/healthz`.

## Adding a family member

1. Build the APK (`mgmt/build-apk.sh`), send it, install it.
2. Open the app once: it enrolls itself and shows "Waiting for the administrator…" under
   *Remote management*.
3. `ngctl devices` → find the new pending device, then:

   ```
   ngctl approve <id-prefix> --name mama --profile family
   ngctl overrides mama put '{"subscriptions":{"items":[{"id":"mama","remarks":"VPN","url":"https://<panel>:2096/sub/<subId>"}]},"select":{"remarks":"Bomboclat"}}'
   ```
4. The phone picks it up within about a minute: a pending phone checks back every minute for
   30 minutes after the app was opened (and every minute while its VPN runs); later it waits for
   the next idle sync (≤ 15 min). It downloads the subscription and selects the server. Tap the
   button once to grant the VPN permission (Android 17 also asks for "nearby devices": Allow);
   "start on boot" is on in the `family` profile. For a fully hands-off setup turn on Android's
   *Always-on VPN* for the app.
5. Optional: *Remote management → Allow network diagnostics* (location "while using the app" +
   phone). Android shows the Wi-Fi name only while the app is open, so reports carry the name
   recorded at the last app open for the same network (`"ssid_source": "cached"`).

Updating: install the new APK over the old one (same release key). Start-on-boot also fires on an
app update, so the VPN restarts by itself within seconds.

## Debugging recipes

```
ngctl devices                                   # who is online, which network/ISP, VPN on, config in sync
ngctl timeline mama --since 6h                  # state changes, events, probes, warnings in one list
ngctl stats --since 7d --by kind,target,net     # success rate / p50 / p90 per server per access network
ngctl probe mama '{"servers":"group"}'          # direct TCP+TLS, real ping, proxy/direct HTTP, now
ngctl loglevel mama info 30                     # xray access + DNS logs for 30 min (core restarts)
ngctl live mama 20                              # logs every 3 s, sync every minute
ngctl logs mama --since 15m --tag GoLog -f      # follow the core log
ngctl cmd mama exec '{"argv":["ping","-c","5","77.88.8.8"]}' --wait 60
ngctl profile patch family '{"settings":{"pref_mux_enabled":true}}'   # everyone, at next sync
```

DNS: `ngctl dns [mama] --since 24h` shows each core resolver per access network (success rate,
p50/p90, how often it lost the parallel race, filtered answers), the phone's own resolver (uncached
probes of `ya.ru`, `youtube.com` every 15 min — what bypassed apps use), the top errors and failing
domains. The data comes from xray's `dnsLog` (one line per upstream query, every query, at any log
level), turned into `dns` measurements by the server, so `ngctl stats --kind dns --by target,hour`
and `ngctl measurements --kind dns` work too; failed lookups also appear in the timeline.

Core (xray) lines carry xray's own level (`[Error]` → E, `[Warning]` → W), so `--level W` and the
timeline show core errors; the cancelled loser of a parallel DoQ+DoH lookup and the per-core start
banner are stored as D/I.

Commands go `queued` → `sent` → `running` (the phone started it) → `done`/`failed`. Apps from
2.3.13-family.4 on acknowledge each command when they start it, and the server re-sends one still
`sent` after 90 s (up to 5 deliveries), so a command caught in a VPN restart arrives late instead
of never; the phone skips ids it already started. `ngctl cmd`'s output shows `deliveries`. A resent
command can run late (up to its TTL; default 1 h). A probe's
`real_ping` that hangs (a stalled REALITY handshake) is cut off at 2 × `timeout_ms` (≥ 15 s) and
recorded as `error: "timeout"`.

`tcp_connect` / `tls_handshake` failing while `http_direct` to ya.ru works means the path to that
server port is blocked (DPI); `core_delay` failing while the direct probes pass points at the server
or the exit behind it. Measurements carry `net` like `cellular/MTS-AS` or `wifi/MGTS-PPPOE`.

## Defaults (fresh install)

- Domestic DNS `77.88.8.8` (Yandex) instead of AliDNS; local DNS split on (Russian domains resolve
  via Yandex directly, the rest via the remote resolver through the tunnel).
- Routing preset "Russia whitelist" (+ `.ru .su .рф`), domain strategy `IPIfNonMatch` so
  `geoip:ru` also catches Russian-hosted sites on foreign TLDs.
- xray-core TUN instead of hev-socks5-tunnel (hev added ~215 ms per DNS lookup in field tests).
- Per-app bypass for ~50 Russian banking/government/marketplace/carrier apps that refuse to work
  over a VPN (`AppConfig.RU_BYPASS_APPS`).
- Geo files source `runetfreedom/russia-v2ray-rules-dat`; Russia preset listed first.
- Update check looks at this fork's GitHub releases.
