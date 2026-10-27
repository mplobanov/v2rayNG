// ngctl is the admin CLI for ngmgmt. Every command prints JSON (or plain text for logs and
// timelines), so it works equally well for people and for agents piping into jq.
//
// Config: ~/.config/ngctl/config.json {"url": "https://<server-ip>:9443", "token": "...",
// "pin": "sha256/..."} or the NGCTL_URL / NGCTL_TOKEN / NGCTL_PIN environment variables.
package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type cfg struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	Pin   string `json:"pin"`
}

func loadCfg() cfg {
	c := cfg{}
	home, _ := os.UserHomeDir()
	if b, err := os.ReadFile(filepath.Join(home, ".config", "ngctl", "config.json")); err == nil {
		json.Unmarshal(b, &c)
	}
	if v := os.Getenv("NGCTL_URL"); v != "" {
		c.URL = v
	}
	if v := os.Getenv("NGCTL_TOKEN"); v != "" {
		c.Token = v
	}
	if v := os.Getenv("NGCTL_PIN"); v != "" {
		c.Pin = v
	}
	if c.URL == "" || c.Token == "" {
		die("no server configured: write ~/.config/ngctl/config.json {\"url\",\"token\",\"pin\"}")
	}
	c.URL = strings.TrimRight(c.URL, "/")
	return c
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "ngctl: "+f+"\n", a...)
	os.Exit(1)
}

func client(c cfg, timeout time.Duration) *http.Client {
	tc := &tls.Config{}
	if c.Pin != "" {
		// Self-signed server certificate: trust exactly the pinned public key.
		tc.InsecureSkipVerify = true
		tc.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			if got := pinOf(cs.PeerCertificates[0]); got != c.Pin {
				return fmt.Errorf("certificate pin mismatch: got %s", got)
			}
			return nil
		}
	}
	return &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: tc, Proxy: nil}}
}

func pinOf(c *x509.Certificate) string {
	h := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return "sha256/" + base64.StdEncoding.EncodeToString(h[:])
}

func call(c cfg, method, path string, body any, timeout time.Duration) []byte {
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case []byte:
			rd = bytes.NewReader(b)
		case string:
			rd = strings.NewReader(b)
		default:
			j, _ := json.Marshal(b)
			rd = bytes.NewReader(j)
		}
	}
	req, err := http.NewRequest(method, c.URL+path, rd)
	if err != nil {
		die("%v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client(c, timeout).Do(req)
	if err != nil {
		die("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		die("%s %s: HTTP %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out
}

func printJSON(b []byte) {
	var v any
	if json.Unmarshal(b, &v) != nil {
		os.Stdout.Write(b)
		return
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

// readDoc reads a JSON argument: inline JSON, @file, or "-" for stdin.
func readDoc(arg string) []byte {
	var b []byte
	var err error
	switch {
	case arg == "-":
		b, err = io.ReadAll(os.Stdin)
	case strings.HasPrefix(arg, "@"):
		b, err = os.ReadFile(arg[1:])
	default:
		b = []byte(arg)
	}
	if err != nil {
		die("%v", err)
	}
	if !json.Valid(b) {
		die("not valid JSON: %.200s", string(b))
	}
	return b
}

func esc(s string) string { return url.PathEscape(s) }

const help = `ngctl — admin CLI for the v2rayNG Family logs & configs server

devices                                   list devices (status, network, VPN, server, config sync)
device <dev>                              one device in detail
approve <dev> [--name N] [--profile P]    approve an enrolled device
set <dev> [--name N] [--profile P] [--status S] [--note T]
delete <dev> [--purge]
config <dev>                              effective config + agent params
overrides <dev> [get|put DOC|patch DOC]   per-device document (DOC: JSON, @file or -)
state <dev>                               latest full state
states <dev> [--since 6h]                 state history
profiles                                  list profiles
profile get|put|patch|delete <name> [DOC]
logs [<dev>] [--since 1h] [--until T] [--level W] [--tag GoLog,..] [--q text]... [--exclude text]
     [--limit 500] [--json] [-f]          (-f follows new lines)
events [<dev>] [--since 24h] [--type T]
measurements [<dev>] [--since 24h] [--kind K] [--target T] [--net N]
stats [--device D] [--since 24h] [--by kind,target,net] [--kind K]
timeline <dev> [--since 6h] [--until T] [--log-level W|I|none] [--json]
cmd <dev> <type> [ARGS_JSON] [--wait 120] [--ttl 3600]
probe <dev> [ARGS_JSON] [--wait 180]      shortcut for cmd <dev> probe
live <dev> [minutes]                      live mode (default 30, 0 = off)
loglevel <dev> <debug|info|warning|error|none|clear> [minutes]
commands [<dev>]                          recent commands
command <id> [--wait 60]
audit
help-api                                  server API reference (markdown)
api <METHOD> <path> [DOC]                 raw call

<dev> is a device id, unique id prefix, or name. Times: 90m, 6h, 2d, unix, RFC3339,
'2006-01-02 15:04' (Moscow).
`

// flags parses --name value / --flag pairs after positional args.
type flags struct {
	pos  []string
	kv   map[string][]string
	bool map[string]bool
}

var boolFlags = map[string]bool{"json": true, "purge": true, "f": true}

func parse(args []string) flags {
	f := flags{kv: map[string][]string{}, bool: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && len(a) > 1 && a != "-" {
			k := strings.TrimLeft(a, "-")
			if eq := strings.IndexByte(k, '='); eq >= 0 {
				f.kv[k[:eq]] = append(f.kv[k[:eq]], k[eq+1:])
				continue
			}
			if boolFlags[k] {
				f.bool[k] = true
				continue
			}
			if i+1 < len(args) {
				f.kv[k] = append(f.kv[k], args[i+1])
				i++
				continue
			}
			die("flag %s needs a value", a)
		}
		f.pos = append(f.pos, a)
	}
	return f
}

func (f flags) get(k string) string {
	if v := f.kv[k]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func (f flags) arg(i int, what string) string {
	if i >= len(f.pos) {
		die("missing %s", what)
	}
	return f.pos[i]
}

func query(f flags, keys ...string) string {
	v := url.Values{}
	for _, k := range keys {
		for _, x := range f.kv[strings.ReplaceAll(k, "_", "-")] {
			v.Add(k, x)
		}
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

func main() {
	flag.Usage = func() { fmt.Print(help) }
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(help)
		return
	}
	c := loadCfg()
	cmd := os.Args[1]
	f := parse(os.Args[2:])
	short := 30 * time.Second
	switch cmd {
	case "devices":
		printJSON(call(c, "GET", "/v1/admin/devices", nil, short))
	case "device":
		printJSON(call(c, "GET", "/v1/admin/devices/"+esc(f.arg(0, "device")), nil, short))
	case "approve", "set":
		body := map[string]any{}
		if cmd == "approve" {
			body["status"] = "approved"
		}
		for _, k := range []string{"name", "profile", "status", "note"} {
			if v, ok := f.kv[k]; ok {
				body[k] = v[len(v)-1]
			}
		}
		printJSON(call(c, "PATCH", "/v1/admin/devices/"+esc(f.arg(0, "device")), body, short))
	case "delete":
		q := ""
		if f.bool["purge"] {
			q = "?purge=1"
		}
		printJSON(call(c, "DELETE", "/v1/admin/devices/"+esc(f.arg(0, "device"))+q, nil, short))
	case "config":
		printJSON(call(c, "GET", "/v1/admin/devices/"+esc(f.arg(0, "device"))+"/config", nil, short))
	case "overrides":
		p := "/v1/admin/devices/" + esc(f.arg(0, "device")) + "/overrides"
		switch op := f.get("op"); {
		case len(f.pos) < 2 || f.pos[1] == "get" || op == "get":
			printJSON(call(c, "GET", p, nil, short))
		case f.pos[1] == "put":
			printJSON(call(c, "PUT", p, readDoc(f.arg(2, "document")), short))
		case f.pos[1] == "patch":
			printJSON(call(c, "PATCH", p, readDoc(f.arg(2, "document")), short))
		default:
			die("overrides <dev> get|put DOC|patch DOC")
		}
	case "state":
		printJSON(call(c, "GET", "/v1/admin/devices/"+esc(f.arg(0, "device"))+"/state", nil, short))
	case "states":
		printJSON(call(c, "GET", "/v1/admin/devices/"+esc(f.arg(0, "device"))+"/states"+query(f, "since", "until", "limit"), nil, short))
	case "profiles":
		printJSON(call(c, "GET", "/v1/admin/profiles", nil, short))
	case "profile":
		op, name := f.arg(0, "get|put|patch|delete"), f.arg(1, "profile name")
		p := "/v1/admin/profiles/" + esc(name)
		switch op {
		case "get":
			printJSON(call(c, "GET", p, nil, short))
		case "put":
			printJSON(call(c, "PUT", p, readDoc(f.arg(2, "document")), short))
		case "patch":
			printJSON(call(c, "PATCH", p, readDoc(f.arg(2, "document")), short))
		case "delete":
			printJSON(call(c, "DELETE", p, nil, short))
		default:
			die("profile get|put|patch|delete <name> [DOC]")
		}
	case "logs":
		if len(f.pos) > 0 {
			f.kv["device"] = []string{f.pos[0]}
		}
		qs := query(f, "device", "since", "until", "level", "tag", "q", "exclude", "limit", "order")
		if f.bool["f"] {
			follow(c, f)
			return
		}
		if f.bool["json"] {
			printJSON(call(c, "GET", "/v1/admin/logs"+qs, nil, 2*time.Minute))
			return
		}
		sep := "?"
		if qs != "" {
			sep = "&"
		}
		os.Stdout.Write(call(c, "GET", "/v1/admin/logs"+qs+sep+"format=text", nil, 2*time.Minute))
	case "events":
		if len(f.pos) > 0 {
			f.kv["device"] = []string{f.pos[0]}
		}
		printJSON(call(c, "GET", "/v1/admin/events"+query(f, "device", "since", "until", "type", "limit"), nil, short))
	case "measurements":
		if len(f.pos) > 0 {
			f.kv["device"] = []string{f.pos[0]}
		}
		printJSON(call(c, "GET", "/v1/admin/measurements"+query(f, "device", "since", "until", "kind", "target", "net", "limit"), nil, short))
	case "stats":
		if len(f.pos) > 0 {
			f.kv["device"] = []string{f.pos[0]}
		}
		printJSON(call(c, "GET", "/v1/admin/stats"+query(f, "device", "since", "until", "kind", "target", "net", "by"), nil, time.Minute))
	case "timeline":
		f.kv["device"] = []string{f.arg(0, "device")}
		qs := query(f, "device", "since", "until", "log_level", "log_limit")
		if f.bool["json"] {
			printJSON(call(c, "GET", "/v1/admin/timeline"+qs, nil, time.Minute))
			return
		}
		os.Stdout.Write(call(c, "GET", "/v1/admin/timeline"+qs+"&format=text", nil, time.Minute))
	case "cmd", "probe":
		dev := f.arg(0, "device")
		typ := "probe"
		argIdx := 1
		if cmd == "cmd" {
			typ = f.arg(1, "command type")
			argIdx = 2
		}
		body := map[string]any{"type": typ}
		if argIdx < len(f.pos) {
			body["args"] = json.RawMessage(readDoc(f.pos[argIdx]))
		}
		if t := f.get("ttl"); t != "" {
			var n int64
			fmt.Sscan(t, &n)
			body["ttl_sec"] = n
		}
		wait := f.get("wait")
		if wait == "" && typ == "probe" {
			wait = "180"
		}
		q := ""
		timeout := short
		if wait != "" {
			q = "?wait=" + wait
			var n int
			fmt.Sscan(wait, &n)
			timeout = time.Duration(n+30) * time.Second
		}
		printJSON(call(c, "POST", "/v1/admin/devices/"+esc(dev)+"/commands"+q, body, timeout))
	case "live":
		min := 30.0
		if len(f.pos) > 1 {
			fmt.Sscan(f.pos[1], &min)
		}
		printJSON(call(c, "POST", "/v1/admin/devices/"+esc(f.arg(0, "device"))+"/live", map[string]any{"minutes": min}, short))
	case "loglevel":
		lvl := f.arg(1, "level")
		if lvl == "clear" {
			lvl = ""
		}
		min := 60.0
		if len(f.pos) > 2 {
			fmt.Sscan(f.pos[2], &min)
		}
		printJSON(call(c, "POST", "/v1/admin/devices/"+esc(f.arg(0, "device"))+"/loglevel", map[string]any{"level": lvl, "minutes": min}, short))
	case "commands":
		q := ""
		if len(f.pos) > 0 {
			q = "?device=" + url.QueryEscape(f.pos[0])
		}
		printJSON(call(c, "GET", "/v1/admin/commands"+q, nil, short))
	case "command":
		q := ""
		timeout := short
		if w := f.get("wait"); w != "" {
			q = "?wait=" + w
			timeout = 11 * time.Minute
		}
		printJSON(call(c, "GET", "/v1/admin/commands/"+esc(f.arg(0, "command id"))+q, nil, timeout))
	case "audit":
		printJSON(call(c, "GET", "/v1/admin/audit", nil, short))
	case "help-api":
		os.Stdout.Write(call(c, "GET", "/v1/admin/help", nil, short))
	case "api":
		method, path := strings.ToUpper(f.arg(0, "method")), f.arg(1, "path")
		var body any
		if len(f.pos) > 2 {
			body = readDoc(f.pos[2])
		}
		printJSON(call(c, method, path, body, 10*time.Minute))
	default:
		die("unknown command %q (ngctl help)", cmd)
	}
}

// follow polls for new log lines every 3 s, like tail -f.
func follow(c cfg, f flags) {
	since := f.get("since")
	if since == "" {
		since = "2m"
	}
	f.kv["since"] = []string{since}
	seen := map[string]bool{}
	for {
		var lines []struct {
			Device string  `json:"device"`
			Ts     float64 `json:"ts"`
			Time   string  `json:"time"`
			Lvl    string  `json:"lvl"`
			Tag    string  `json:"tag"`
			Msg    string  `json:"msg"`
		}
		f.kv["order"] = nil
		json.Unmarshal(call(c, "GET", "/v1/admin/logs"+query(f, "device", "since", "level", "tag", "q", "exclude", "limit"), nil, time.Minute), &lines)
		last := 0.0
		next := map[string]bool{}
		for _, l := range lines {
			key := fmt.Sprintf("%.6f|%s|%s", l.Ts, l.Tag, l.Msg)
			next[key] = true
			if !seen[key] {
				fmt.Printf("%s %-10s %s %s: %s\n", l.Time[5:23], l.Device, l.Lvl, l.Tag, l.Msg)
			}
			if l.Ts > last {
				last = l.Ts
			}
		}
		seen = next
		if last > 0 {
			f.kv["since"] = []string{fmt.Sprintf("%.6f", last-1)}
		}
		time.Sleep(3 * time.Second)
	}
}
