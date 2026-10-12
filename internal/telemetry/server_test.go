package telemetry

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"golang.org/x/crypto/bcrypt"

	"github.com/yuuki/hostping/internal/config"
)

// Every timeout must be set (gosec G112): a zero leaves a slow client able to
// hold a connection forever. The write timeout must still leave a large scrape
// time to be written.
func TestMetricsServerTimeouts(t *testing.T) {
	s := newMetricsServer(nil)
	for name, d := range map[string]int64{
		"ReadHeaderTimeout": int64(s.ReadHeaderTimeout),
		"ReadTimeout":       int64(s.ReadTimeout),
		"WriteTimeout":      int64(s.WriteTimeout),
		"IdleTimeout":       int64(s.IdleTimeout),
	} {
		if d <= 0 {
			t.Errorf("%s is not set", name)
		}
	}
	if s.WriteTimeout < 30_000_000_000 {
		t.Errorf("WriteTimeout %v is too short for a large scrape", s.WriteTimeout)
	}
	if s.ReadHeaderTimeout > s.ReadTimeout {
		t.Errorf("ReadHeaderTimeout %v exceeds ReadTimeout %v", s.ReadHeaderTimeout, s.ReadTimeout)
	}
}

func testConfig(addr string) config.Config {
	cfg := config.Defaults()
	cfg.PrometheusAddr = addr
	cfg.OTLPEndpoint = ""
	return cfg
}

// An address that cannot be bound fails Start, so the daemon exits (and systemd
// restarts it) rather than running without its main output, and the global
// meter provider is left alone.
func TestStartFailsWhenMetricsAddrCannotBind(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback: %v", err)
	}
	defer func() { _ = busy.Close() }()

	before := otel.GetMeterProvider()
	tel, err := Start(context.Background(), testConfig(busy.Addr().String()), testBuild)
	if err == nil {
		_ = tel.Shutdown(context.Background())
		t.Fatalf("Start bound %s, which is already in use", busy.Addr())
	}
	if !strings.Contains(err.Error(), "metrics listener") {
		t.Errorf("error %q does not name the metrics listener", err)
	}
	if otel.GetMeterProvider() != before {
		t.Error("a failed Start installed its meter provider")
	}
}

// The same holds for an address that is not one, which needs no socket to test.
func TestStartFailsOnInvalidMetricsAddr(t *testing.T) {
	before := otel.GetMeterProvider()
	tel, err := Start(context.Background(), testConfig("127.0.0.1:99999"), testBuild)
	if err == nil {
		_ = tel.Shutdown(context.Background())
		t.Fatal("Start accepted port 99999")
	}
	if otel.GetMeterProvider() != before {
		t.Error("a failed Start installed its meter provider")
	}
}

func TestStartServesMetricsAndHealthz(t *testing.T) {
	before := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(before) })
	tel, err := Start(context.Background(), testConfig("127.0.0.1:0"), testBuild)
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("cannot listen on loopback: %v", err)
		}
		t.Fatal(err)
	}
	defer func() { _ = tel.Shutdown(context.Background()) }()
	if tel.Addr() == "" {
		t.Fatal("Start returned no bound address")
	}
	for path, want := range map[string]string{"/healthz": "", "/metrics": "hostping_build_info"} {
		resp, err := http.Get("http://" + tel.Addr() + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Errorf("GET %s = %d, body contains %q: %v", path, resp.StatusCode, want, strings.Contains(string(body), want))
		}
	}
}

// A web config that cannot be loaded fails Start before anything is bound.
func TestStartFailsOnBadWebConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.yml")
	if err := os.WriteFile(path, []byte("tls_server_config:\n  cert_file: /nonexistent.crt\n  key_file: /nonexistent.key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig("127.0.0.1:99999") // never reached: the config fails first
	cfg.MetricsWebConfig = path
	tel, err := Start(context.Background(), cfg, testBuild)
	if err == nil {
		_ = tel.Shutdown(context.Background())
		t.Fatal("Start accepted a web config whose certificate does not exist")
	}
	if !strings.Contains(err.Error(), "--metrics-web-config") {
		t.Errorf("error %q does not name --metrics-web-config", err)
	}
}

func TestStartMetricsBasicAuth(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("test-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "web.yml")
	if err := os.WriteFile(path, []byte("basic_auth_users:\n  scraper: "+string(hash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(before) })
	cfg := testConfig("127.0.0.1:0")
	cfg.MetricsWebConfig = path
	tel, err := Start(context.Background(), cfg, testBuild)
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("cannot listen on loopback: %v", err)
		}
		t.Fatal(err)
	}
	defer func() { _ = tel.Shutdown(context.Background()) }()

	get := func(user, pass string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+tel.Addr()+"/metrics", nil)
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("", ""); code != http.StatusUnauthorized {
		t.Errorf("no credentials: %d, want 401", code)
	}
	if code := get("scraper", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong password: %d, want 401", code)
	}
	if code := get("scraper", "test-password"); code != http.StatusOK {
		t.Errorf("valid credentials: %d, want 200", code)
	}
}

func TestAddrWithoutListener(t *testing.T) {
	var nilTel *Telemetry
	if nilTel.Addr() != "" || (&Telemetry{}).Addr() != "" {
		t.Fatal("Addr without a listener must be empty")
	}
}

// WithLiveness puts the tracker behind the served /healthz.
func TestStartServesLivenessOnHealthz(t *testing.T) {
	before := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(before) })
	clk := time.Unix(1_700_000_000, 0)
	live := NewLiveness(time.Second, func() time.Time { return clk })
	clk = clk.Add(time.Hour)
	tel, err := Start(context.Background(), testConfig("127.0.0.1:0"), testBuild, WithLiveness(live))
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("cannot listen on loopback: %v", err)
		}
		t.Fatal(err)
	}
	defer func() { _ = tel.Shutdown(context.Background()) }()
	resp, err := http.Get("http://" + tel.Addr() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "no monitor poll completed") {
		t.Fatalf("stalled loop: GET /healthz = %d %q, want 503 with the reason", resp.StatusCode, body)
	}
}
