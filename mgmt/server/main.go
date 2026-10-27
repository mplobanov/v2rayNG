// ngmgmt is the logs & configs server for the v2rayNG Family fork.
//
// Devices enroll with a shared enrollment token, wait for an admin to approve them, then
// periodically sync: they upload state, events, measurements and logcat lines, and receive
// their desired configuration and queued commands. Admins (people or agents) use the
// /v1/admin API, usually through the ngctl CLI. See API.md.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Listen string `json:"listen"`
	DB     string `json:"db"`
	TLS    struct {
		Cert string `json:"cert"`
		Key  string `json:"key"`
	} `json:"tls"`
	// SHA-256 hex of the tokens; the plain tokens live only on the clients.
	AdminTokenSHA256  []string `json:"admin_token_sha256"`
	EnrollTokenSHA256 []string `json:"enroll_token_sha256"`
	// Devices enrolling before this unix time are approved without an admin step.
	AutoApproveUntil int64 `json:"auto_approve_until"`
	DefaultProfile   string `json:"default_profile"`
	Retention        struct {
		LogsDays         int `json:"logs_days"`
		LogsMaxRows      int `json:"logs_max_rows"`
		StatesDays       int `json:"states_days"`
		EventsDays       int `json:"events_days"`
		MeasurementsDays int `json:"measurements_days"`
	} `json:"retention"`
	IPInfoURL string `json:"ipinfo_url"`
	// Addresses devices use to reach this server, best first (e.g. a DNS name, then the bare
	// IP). Sent to every device as agent parameter "mgmt_urls"; the app falls back along the
	// list when an address is unreachable, so a new name or IP needs no new APK. A profile's
	// telemetry section may override it.
	DeviceURLs []string `json:"device_urls"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = ":9443"
	}
	if c.DB == "" {
		c.DB = "/var/lib/ngmgmt/ngmgmt.db"
	}
	if c.DefaultProfile == "" {
		c.DefaultProfile = "default"
	}
	r := &c.Retention
	if r.LogsDays == 0 {
		r.LogsDays = 14
	}
	if r.LogsMaxRows == 0 {
		r.LogsMaxRows = 3_000_000
	}
	if r.StatesDays == 0 {
		r.StatesDays = 30
	}
	if r.EventsDays == 0 {
		r.EventsDays = 120
	}
	if r.MeasurementsDays == 0 {
		r.MeasurementsDays = 120
	}
	if c.IPInfoURL == "" {
		c.IPInfoURL = "http://ip-api.com/json/%s?fields=status,message,country,countryCode,regionName,city,isp,org,as,asname,mobile,proxy,hosting"
	}
	return c, nil
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  ngmgmt serve  -config /etc/ngmgmt/config.json
  ngmgmt gen-cert -cert tls.crt -key tls.key -ip <public-ip> [-dns name,...]
  ngmgmt pin    -cert tls.crt          print the SPKI pin of a certificate
  ngmgmt token                          print a new random token and its sha256
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		cfgPath := fs.String("config", "/etc/ngmgmt/config.json", "config file")
		fs.Parse(os.Args[2:])
		if err := serve(*cfgPath); err != nil {
			log.Fatal(err)
		}
	case "gen-cert":
		fs := flag.NewFlagSet("gen-cert", flag.ExitOnError)
		cert := fs.String("cert", "tls.crt", "")
		key := fs.String("key", "tls.key", "")
		ips := fs.String("ip", "", "comma separated IP SANs")
		dns := fs.String("dns", "", "comma separated DNS SANs")
		fs.Parse(os.Args[2:])
		if err := genCert(*cert, *key, *ips, *dns); err != nil {
			log.Fatal(err)
		}
	case "pin":
		fs := flag.NewFlagSet("pin", flag.ExitOnError)
		cert := fs.String("cert", "tls.crt", "")
		fs.Parse(os.Args[2:])
		b, err := os.ReadFile(*cert)
		if err != nil {
			log.Fatal(err)
		}
		p, _ := pem.Decode(b)
		if p == nil {
			log.Fatal("no PEM block")
		}
		c, err := x509.ParseCertificate(p.Bytes)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(spkiPin(c))
	case "token":
		t := randomToken()
		fmt.Printf("token  %s\nsha256 %s\n", t, sha256Hex(t))
	default:
		usage()
	}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// spkiPin returns the OkHttp-style pin "sha256/<base64(SHA-256(SubjectPublicKeyInfo))>".
func spkiPin(c *x509.Certificate) string {
	h := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return "sha256/" + base64.StdEncoding.EncodeToString(h[:])
}

func genCert(certPath, keyPath, ips, dns string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ngmgmt"},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().AddDate(30, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, s := range strings.Split(ips, ",") {
		if ip := net.ParseIP(strings.TrimSpace(s)); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
	}
	for _, s := range strings.Split(dns, ",") {
		if s = strings.TrimSpace(s); s != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, s)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		return err
	}
	c, _ := x509.ParseCertificate(der)
	fmt.Println(spkiPin(c))
	return nil
}

func serve(cfgPath string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if len(cfg.AdminTokenSHA256) == 0 || len(cfg.EnrollTokenSHA256) == 0 {
		return errors.New("config needs admin_token_sha256 and enroll_token_sha256")
	}
	st, err := OpenStore(cfg.DB)
	if err != nil {
		return err
	}
	srv := NewServer(cfg, st)
	cert, err := tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       120 * time.Second,
		WriteTimeout:      330 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		},
		ErrorLog: log.New(&tlsNoiseFilter{}, "", 0),
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go srv.maintenanceLoop(ctx)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Printf("ngmgmt listening on %s (pin %s)", cfg.Listen, spkiPinFromTLS(cert))
	err = hs.ListenAndServeTLS("", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func spkiPinFromTLS(c tls.Certificate) string {
	if len(c.Certificate) == 0 {
		return ""
	}
	x, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return ""
	}
	return spkiPin(x)
}

// tlsNoiseFilter drops the handshake errors that internet scanners produce all day.
type tlsNoiseFilter struct{}

func (tlsNoiseFilter) Write(p []byte) (int, error) {
	s := string(p)
	if strings.Contains(s, "TLS handshake error") || strings.Contains(s, "http: TLS") {
		return len(p), nil
	}
	log.Print(strings.TrimRight(s, "\n"))
	return len(p), nil
}
