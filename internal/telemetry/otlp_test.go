package telemetry

import (
	"strings"
	"testing"
)

func TestResolveOTLPTransport(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name     string
		endpoint string
		flag     *bool
		env      map[string]string
		insecure bool
		url      bool
		reason   string
		err      string
	}{
		// host:port without any setting: loopback stays plaintext (the old
		// behaviour), anything else is TLS.
		{name: "localhost", endpoint: "localhost:4317", insecure: true, reason: "loopback endpoint"},
		{name: "ipv4 loopback", endpoint: "127.0.0.1:4317", insecure: true, reason: "loopback endpoint"},
		{name: "ipv6 loopback", endpoint: "[::1]:4317", insecure: true, reason: "loopback endpoint"},
		{name: "remote", endpoint: "10.0.0.20:4317", reason: "non-loopback endpoint"},
		{name: "remote name", endpoint: "collector.example:4317", reason: "non-loopback endpoint"},

		// --otlp-insecure overrides the address-based default either way.
		{name: "flag plaintext remote", endpoint: "10.0.0.20:4317", flag: &yes, insecure: true, reason: "--otlp-insecure"},
		{name: "flag tls loopback", endpoint: "localhost:4317", flag: &no, reason: "--otlp-insecure"},

		// The standard env vars, per-signal first, as the exporter reads them.
		{name: "env plaintext remote", endpoint: "10.0.0.20:4317",
			env: map[string]string{"OTEL_EXPORTER_OTLP_INSECURE": "true"}, insecure: true, reason: "OTEL_EXPORTER_OTLP_INSECURE"},
		{name: "env tls loopback", endpoint: "localhost:4317",
			env: map[string]string{"OTEL_EXPORTER_OTLP_INSECURE": "false"}, reason: "OTEL_EXPORTER_OTLP_INSECURE"},
		{name: "env case-insensitive", endpoint: "10.0.0.20:4317",
			env: map[string]string{"OTEL_EXPORTER_OTLP_INSECURE": "TRUE"}, insecure: true, reason: "OTEL_EXPORTER_OTLP_INSECURE"},
		{name: "metrics env wins", endpoint: "10.0.0.20:4317",
			env:      map[string]string{"OTEL_EXPORTER_OTLP_INSECURE": "false", "OTEL_EXPORTER_OTLP_METRICS_INSECURE": "true"},
			insecure: true, reason: "OTEL_EXPORTER_OTLP_METRICS_INSECURE"},
		{name: "flag agrees with env", endpoint: "10.0.0.20:4317", flag: &yes,
			env: map[string]string{"OTEL_EXPORTER_OTLP_INSECURE": "true"}, insecure: true, reason: "--otlp-insecure"},
		{name: "flag contradicts env", endpoint: "10.0.0.20:4317", flag: &no,
			env: map[string]string{"OTEL_EXPORTER_OTLP_INSECURE": "true"}, err: "contradicts OTEL_EXPORTER_OTLP_INSECURE"},

		// A CA certificate means TLS, even to loopback.
		{name: "certificate loopback", endpoint: "localhost:4317",
			env: map[string]string{"OTEL_EXPORTER_OTLP_CERTIFICATE": "/etc/ssl/ca.crt"}, reason: "OTEL_EXPORTER_OTLP_CERTIFICATE"},
		{name: "certificate with plaintext flag", endpoint: "10.0.0.20:4317", flag: &yes,
			env: map[string]string{"OTEL_EXPORTER_OTLP_CERTIFICATE": "/etc/ssl/ca.crt"}, err: "contradicts OTEL_EXPORTER_OTLP_CERTIFICATE"},
		{name: "client certificate with plaintext env", endpoint: "10.0.0.20:4317",
			env: map[string]string{"OTEL_EXPORTER_OTLP_INSECURE": "true", "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE": "/etc/ssl/c.crt"},
			err: "contradicts OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE"},

		// A scheme decides by itself; the INSECURE env var does not apply.
		{name: "http url", endpoint: "http://10.0.0.20:4317", url: true, insecure: true, reason: "http:// scheme"},
		{name: "https url", endpoint: "https://localhost:4317", url: true, reason: "https:// scheme"},
		{name: "https url ignores env", endpoint: "https://10.0.0.20:4317", url: true, reason: "https:// scheme",
			env: map[string]string{"OTEL_EXPORTER_OTLP_INSECURE": "true"}},
		{name: "flag contradicts scheme", endpoint: "https://10.0.0.20:4317", flag: &yes, err: "contradicts the https:// scheme"},
		{name: "flag agrees with scheme", endpoint: "http://10.0.0.20:4317", flag: &yes, url: true, insecure: true, reason: "http:// scheme"},
		{name: "http url with certificate", endpoint: "http://10.0.0.20:4317",
			env: map[string]string{"OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE": "/etc/ssl/ca.crt"}, err: "contradicts OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE"},
		{name: "unknown scheme", endpoint: "grpc://10.0.0.20:4317", err: "scheme must be http or https"},
		{name: "url without host", endpoint: "https://", err: "has no host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			got, err := resolveOTLPTransport(tc.endpoint, tc.flag, getenv)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want one containing %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.endpoint != tc.endpoint || got.url != tc.url || got.insecure != tc.insecure || got.reason != tc.reason {
				t.Fatalf("got %+v, want url=%v insecure=%v reason=%q", got, tc.url, tc.insecure, tc.reason)
			}
			// One endpoint option, plus WithInsecure for plaintext, or explicit
			// system-root TLS credentials unless the env supplies a certificate.
			opts := got.options(getenv)
			want := 2
			if !tc.insecure && tc.env["OTEL_EXPORTER_OTLP_CERTIFICATE"] != "" {
				want = 1
			}
			if len(opts) != want {
				t.Fatalf("%d exporter options, want %d", len(opts), want)
			}
		})
	}
}
