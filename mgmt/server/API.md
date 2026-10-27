# ngmgmt API

Logs & configs server for the v2rayNG Family fork. JSON over HTTPS (self-signed certificate,
clients pin its SPKI hash). Times in responses are Moscow time ISO-8601 plus raw unix `ts`.
Time parameters (`since`, `until`) accept `90m`, `6h`, `2d`, unix seconds, RFC3339 or
`2006-01-02 15:04` (Moscow).

Use the `ngctl` CLI rather than raw HTTP where possible; `ngctl api GET /v1/admin/...`
reaches any endpoint.

## Model

- **Device**: one app install. Enrolls with the shared enrollment token, starts `pending`,
  receives nothing until an admin sets `status=approved`. Referenced in the admin API by id,
  unique id prefix, or name.
- **Profile**: a named desired-config document. A profile may inherit with `"_extends": "parent"`.
- **Overrides**: per-device document merged over the profile chain.
- **Effective config** = profile chain ⊕ overrides ⊕ temporary log-level override. Objects merge
  key by key; arrays and scalars replace. Keys beginning with `_` are comments and are stripped.
  The device applies the document whenever its version (hash) differs from what it last applied,
  so the server is the source of truth: a setting changed by hand on the phone stays until the
  effective document changes again.
- **Telemetry**: the `telemetry` section of the document tunes the device agent (never triggers
  a re-apply).

## Config document

```json
{
  "settings": {
    "pref_remote_dns": "https://dns.example.net/dns-query",
    "pref_mux_enabled": false,
    "pref_per_app_proxy_set": ["ru.sberbankmobile"],
    "pref_vpn_mtu": null
  },
  "routing": { "rulesets": [ {"remarks": "Direct RU", "outboundTag": "direct", "domain": ["geosite:category-ru"]} ] },
  "subscriptions": {
    "items": [ {"id": "cloud02", "remarks": "Cloud 02", "url": "https://panel.example.net:2096/sub/xxxx",
                "enabled": true, "auto_update": true, "update_interval_min": 120, "filter": "", "user_agent": ""} ],
    "update_on_change": true
  },
  "servers": { "group": "Managed", "links": ["vless://...", "trojan://..."] },
  "select":  { "remarks": "Bomboclat", "subscription": "cloud02" },
  "restart": "auto",
  "telemetry": { "sync_sec": 300, "probe_sec": 900, "log_upload_sec": 60 }
}
```

- `settings`: any key of the app's settings store (`pref_*`, see the app's `AppConfig.kt`).
  string → string, `true/false` → bool, array → string set, number → string (the app keeps
  numbers as strings), `null` → remove the key so the app default applies.
  Useful keys: `pref_core_loglevel` (debug|info|warning|error|none), `pref_remote_dns`,
  `pref_domestic_dns`, `pref_vpn_dns`, `pref_dns_hosts`, `pref_local_dns_enabled`,
  `pref_fake_dns_enabled`, `pref_routing_domain_strategy` (AsIs|IPIfNonMatch|IPOnDemand),
  `pref_mux_enabled`, `pref_mux_concurrency`, `pref_mux_xudp_concurrency`, `pref_mux_xudp_quic`,
  `pref_fragment_enabled`, `pref_fragment_packets`, `pref_fragment_length`,
  `pref_fragment_interval`, `pref_use_hev_tunnel_v2`, `pref_hev_tunnel_loglevel`,
  `pref_hev_tunnel_rw_timeout_v2`, `pref_vpn_mtu`, `pref_vpn_bypass_lan`, `pref_ipv6_enabled`,
  `pref_prefer_ipv6`, `pref_sniffing_enabled`, `pref_route_only_enabled`, `pref_per_app_proxy`,
  `pref_bypass_apps`, `pref_per_app_proxy_set`, `pref_speed_enabled`, `pref_is_booted`
  (start on boot), `pref_delay_test_url`, `pref_geo_files_sources`, `pref_mode`.
- `routing.rulesets`: replaces the whole routing rule list (RulesetItem fields: `remarks`,
  `outboundTag` proxy|direct|block, `domain`, `ip`, `port`, `network`, `protocol`, `process`,
  `enabled`).
- `subscriptions.items`: managed subscriptions (app GUID `mgmt-<id>`). Managed subscriptions
  missing from the list are removed from the device; the user's own are never touched.
- `servers`: share links imported into one managed group (replaced on change).
- `select`: server to use, matched by remarks (optionally within one subscription id/remarks).
- `restart`: `auto` (default) restarts the running VPN when an applied change needs it; `never`.

## Device agent parameters (`telemetry`)

| key | default | meaning |
|---|---|---|
| `sync_sec` | 300 | state upload / config check while the VPN runs |
| `idle_sync_min` | 15 | sync interval while the VPN is off (WorkManager, ≥ 15) |
| `log_upload_sec` | 60 | log batch upload interval |
| `probe_sec` | 900 | background health probe interval (0 = off) |
| `probe_core_delay` | true | probe: delay test through the running core |
| `probe_direct` | true | probe: direct TCP + TLS handshake to the selected server |
| `log_spec` | `*:W GoLog:V {app}:V` | logcat filter (`{app}` = app log tag) |
| `log_buffer_lines` | 20000 | lines kept on the device while uploads fail |
| `poll_wait_sec` | 240 | long-poll hold between syncs (commands arrive immediately) |
| `dns_log` | true | core config gets `log.dnsLog`: one line per upstream DNS query (applies at the next core start) |
| `dns_log_cache` | false | also upload the core's DNS cache-hit lines |
| `probe_dns` | `ya.ru youtube.com` | hosts the background probe resolves with the phone's own resolver (`dns_system`) |
| `mgmt_urls` | server `device_urls` | addresses of this server, best first; the app tries them in order, then its baked URL, moving on only when no connection could be made (2.3.13-family.5+, device info `mgmt_caps` contains `urls`) |

Live mode (`POST .../live`) temporarily sets `log_upload_sec=3`, `sync_sec=60`.

## Commands

`POST /v1/admin/devices/{id}/commands {"type": "...", "args": {...}, "ttl_sec": 3600}`
(add `?wait=120` to block until the result). A running VPN agent long-polls, so commands reach it
within seconds; with the VPN off they wait for the next idle sync (≤ `idle_sync_min`).

Status: `queued` → `sent` (handed to the device; `deliveries` counts how often) → `running` (the
device acknowledged starting it) → `done` / `failed`, or `expired` when the TTL passes first.

Delivery is at least once for apps from 2.3.13-family.4 on (device info `"mgmt_caps": ["ack"]`):
the app acknowledges each command when it starts it (`POST /v1/ack`), and a command still `sent`
90 s after its last delivery goes out again with the next sync or poll, at most 5 deliveries in
all. That covers a response that never reached the phone (a long poll cut by a VPN restart or a
network switch) and a command the phone dropped unstarted when its agent stopped (e.g. queued
behind a `select_server`). The app remembers the ids it started for a week and skips a repeat, so
nothing runs twice. A command queued on the phone behind a long probe can get repeats; they are
skipped. Older apps get each command exactly once, and a command handed to an older app is not
resent after that app is upgraded. A resent command can run late (up to its TTL, e.g. a phone that
was offline): give a `select_server` a short `ttl_sec` if running it late would be wrong.

| type | args | does |
|---|---|---|
| `sync` | – | sync now |
| `dump_state` | – | sync now with full settings + server list |
| `upload_logs` | `{"dump": true}` | upload the whole on-device logcat buffer |
| `probe` | see below | run measurements now, return them |
| `restart_vpn` / `stop_vpn` / `start_vpn` | – | control the service (`start_vpn` needs prior VPN consent) |
| `update_subscriptions` | `{"id": "mgmt-cloud02"}` (optional) | download subscriptions now |
| `update_geo` | – | re-download geoip.dat / geosite.dat from `pref_geo_files_sources` |
| `select_server` | `{"remarks": "...", "subscription": "..."}` | switch server (restarts a running VPN) |
| `exec` | `{"argv": ["ping","-c","4","1.1.1.1"], "timeout_sec": 20}` | allow-listed: ping, ping6, getprop, ip, cat/ls under /proc |
| `reenroll` | – | forget the device token and enroll again |

`probe` args (all optional):

```json
{"servers": "selected", "real_ping": true, "tcp": true, "tls": true,
 "urls_proxy": ["https://www.gstatic.com/generate_204", "https://www.youtube.com/generate_204"],
 "urls_direct": ["https://ya.ru"], "dns": ["youtube.com"],
 "download_url": "https://speed.cloudflare.com/__down?bytes=10000000", "download_via": "proxy",
 "timeout_ms": 8000}
```

`servers`: `"selected"`, `"group"` (all servers in the selected server's subscription), `"all"`,
or a list of remarks.

`real_ping` and `core_delay` (libv2ray delay tests) get a hard wall-clock limit of
2 × `timeout_ms`, at least 15 s; the background probe's `core_delay` gets 15 s. A test past it is
recorded with `ok=false`, `data.error="timeout"` and the probe goes on. libv2ray cannot cancel the
call, so its thread is left running until the native call returns; while 4 such calls are stuck,
further delay tests are skipped (`data.error="skipped: 4 earlier delay tests still stuck"`).

## Measurement kinds

| kind | target | meaning |
|---|---|---|
| `core_delay` | server remarks | HTTP delay test through the running core (what the app's "test" shows) |
| `real_ping` | server remarks | delay test through a temporary core for that server (VPN may be off); `error: "timeout"` past the hard limit (see `probe` above) |
| `tcp_connect` | `remarks host:port` | direct TCP connect to the server, bypassing the VPN (DPI / port blocks) |
| `tls_handshake` | `remarks host:port sni` | direct TLS ClientHello→ServerHello with the profile's SNI |
| `http_proxy` | url | GET through the local SOCKS inbound (data: status, ttfb_ms, bytes) |
| `http_direct` | url | GET bypassing the VPN |
| `dns_system` | host | the phone's own resolver for its physical network (router/carrier DNS or Private DNS), outside the VPN, cache bypassed on Android 10+ (data: rcode, answers) — what per-app-bypassed apps get |
| `dns` | resolver (`quic+local://host`, `DOH//host:port`, `UDP:ip:53`) | one upstream query of the core's DNS module, parsed server-side from xray's dnsLog line (data: domain, ips, blocked, result `no record`, error). ok = the resolver answered (NXDOMAIN / no record counts as an answer) |
| `dns_canceled` | resolver | the same, cancelled because the other resolver of a parallel query answered first |
| `download` | url | throughput (data: mbps, bytes) |

Each measurement carries `net` = `<transport>/<ISP AS name>` derived from the device state and its
public IP, so results can be compared per access network (`cellular/MTS-AS` vs `wifi/MGTS`).

## State document (selected fields)

`GET .../state` returns the merged snapshot: `vpn` (running, mode, hev_tun, loglevel,
core_uptime_s, selected server, traffic_bytes), `network.default` (transports, validated, metered,
bandwidth, dns, private DNS, `wifi` {ssid, ssid_source, rssi_dbm, freq_mhz, link/tx/rx_mbps,
standard} or `cell` {operator, mccmnc, data_network_type, signal_dbm, signal_level}),
`network.cellular_standby` (the mobile network while on Wi-Fi, incl. data_enabled), `power`,
`mgmt`, `ip`, `ip_info`, `net_label`, and the heavy sections `settings`, `routing`,
`subscriptions`, `servers`. Android reveals the Wi-Fi name only while the app is open;
`ssid_source: "cached"` marks the name recorded then for the same network.

## Log levels

Logcat priority is kept, except for core lines (`tag=GoLog`, which libv2ray always writes as I):
they take xray's own `[Error]`/`[Warning]`/`[Debug]` level. Routine core noise is demoted: DNS
`context canceled` (the losing half of a parallel DoQ+DoH lookup) → D, the start banner and
deprecation notices → I. Access lines (`accepted … [tun >> proxy]`) stay I.

## Admin endpoints (Bearer admin token)

| method & path | purpose |
|---|---|
| `GET /v1/admin/devices` | all devices with last seen, network, VPN state, server, config sync |
| `GET /v1/admin/devices/{id}` | one device incl. info, overrides, last apply result |
| `PATCH /v1/admin/devices/{id}` | `{"name","profile","status":"approved|pending|disabled","note"}` |
| `DELETE /v1/admin/devices/{id}?purge=1` | forget a device (and its data) |
| `GET /v1/admin/devices/{id}/config` | effective config, version, applied version, agent params |
| `GET/PUT/PATCH /v1/admin/devices/{id}/overrides` | per-device document (PATCH = RFC 7386 merge patch: `null` deletes) |
| `GET /v1/admin/devices/{id}/state` | latest full state snapshot |
| `GET /v1/admin/devices/{id}/states?since=` | state history (partial snapshots as uploaded) |
| `POST /v1/admin/devices/{id}/commands` | queue a command |
| `POST /v1/admin/devices/{id}/live` | `{"minutes": 30}` live mode (0 = off) |
| `POST /v1/admin/devices/{id}/loglevel` | `{"level": "debug", "minutes": 60}` temporary core log level (`level: ""` clears) |
| `GET /v1/admin/commands?device=` / `GET /v1/admin/commands/{id}?wait=60` | command status/results |
| `GET /v1/admin/profiles`, `GET/PUT/PATCH/DELETE /v1/admin/profiles/{name}` | profiles |
| `GET /v1/admin/logs` | `device, since, until, level (V/D/I/W/E), tag (comma list), q (substring, repeatable), exclude, limit, order=asc, format=text` |
| `GET /v1/admin/events` | `device, since, until, type, limit` |
| `GET /v1/admin/measurements` | `device, since, until, kind, target, net, limit` |
| `GET /v1/admin/stats` | aggregates: `by=kind,target,net` (also `device`, `hour`, `day`), same filters; ok_rate, p50/p90 |
| `GET /v1/admin/dns` | DNS health: per resolver and network n / ok_rate / p50 / p90 / lost races / blocked, phone-resolver probes, top errors and failing domains (`device, since, until, format=text`) |
| `GET /v1/admin/timeline` | merged state changes, events, measurements, commands and logs ≥ `log_level` (default W, `none` to skip); `format=text` |
| `GET /v1/admin/audit` | config change history |
| `GET /v1/admin/ipinfo/{ip}` | cached ISP lookup |

Event types: `enrolled`, `config_applied`, `vpn_start`, `vpn_stop`, `vpn_start_failed`,
`core_reload`, `net_change`, `crash`, `logs_dropped`, `agent_start`.

## Device endpoints (Bearer device token)

- `POST /v1/enroll {"enroll_token","device_id","name","device":{...}}` → `{"device_token","status"}`
- `POST /v1/sync` → `{"status","agent":{...},"config_version","config":{"version","doc"}?,"commands":[...]}`
  body: `{"name","device","applied_version","apply_result","state","events","measurements","results"}`
- `POST /v1/logs {"lines":[{"ts","pid","lvl","tag","msg"}],"dropped":0,"source":"stream"}` (gzip ok)
- `GET /v1/poll?wait=240&v=<applied_version>` → returns early with commands / `config_changed`
- `POST /v1/results {"results":[{"id","ok","result","error","measurements"}]}`
- `POST /v1/ack {"ids":[13,15]}`: the device started these commands (`sent` → `running`); see "Commands"
