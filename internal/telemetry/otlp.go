package telemetry

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"google.golang.org/grpc/credentials"
)

// otlpTransport is how the OTLP gRPC exporter reaches its collector: plaintext
// or TLS, and why. It is resolved up front, rather than left to the exporter,
// so that contradictory settings fail at start instead of silently picking one,
// and so the start-up log can say which transport is in use.
type otlpTransport struct {
	endpoint string
	// url is set when endpoint has a scheme (http:// or https://); the
	// scheme then decides the transport, as the OTel specification says.
	url      bool
	insecure bool
	// reason names the setting that decided insecure.
	reason string
}

// The OTel environment variables the exporter reads itself. A per-signal
// METRICS_ variable takes precedence over the generic one, as in the exporter.
var (
	otlpInsecureEnv = []string{"OTEL_EXPORTER_OTLP_METRICS_INSECURE", "OTEL_EXPORTER_OTLP_INSECURE"}
	otlpTLSEnv      = []string{
		"OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE", "OTEL_EXPORTER_OTLP_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_METRICS_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE",
	}
)

// resolveOTLPTransport decides plaintext or TLS for endpoint.
//
//   - An endpoint with a scheme follows it: http:// is plaintext, https:// is
//     TLS. OTEL_EXPORTER_OTLP_INSECURE does not apply (OTel specification).
//   - A host:port endpoint follows --otlp-insecure when given (flag non-nil),
//     else OTEL_EXPORTER_OTLP_[METRICS_]INSECURE when set, else plaintext for a
//     loopback host (traffic that never leaves the host, typically a local
//     collector agent) and TLS with the system roots for anything else.
//
// The exporter itself reads OTEL_EXPORTER_OTLP_[METRICS_]CERTIFICATE,
// CLIENT_CERTIFICATE/CLIENT_KEY and HEADERS. A certificate always makes the
// exporter use TLS, so a certificate with a plaintext setting is an error rather
// than a setting that is silently ignored, as are a flag and an env var, or a
// flag and a scheme, that disagree.
func resolveOTLPTransport(endpoint string, flag *bool, getenv func(string) string) (otlpTransport, error) {
	t := otlpTransport{endpoint: endpoint}
	envName, envVal := firstSetEnv(otlpInsecureEnv, getenv)
	tlsEnv, _ := firstSetEnv(otlpTLSEnv, getenv)

	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil {
			return t, fmt.Errorf("otlp endpoint %q: %w", endpoint, err)
		}
		switch strings.ToLower(u.Scheme) {
		case "http":
			t.insecure = true
		case "https":
		default:
			return t, fmt.Errorf("otlp endpoint %q: scheme must be http or https, or give host:port", endpoint)
		}
		if u.Host == "" {
			return t, fmt.Errorf("otlp endpoint %q has no host", endpoint)
		}
		t.url = true
		t.reason = u.Scheme + ":// scheme"
		if flag != nil && *flag != t.insecure {
			return t, fmt.Errorf("--otlp-insecure=%v contradicts the %s of --otlp %q", *flag, t.reason, endpoint)
		}
	} else {
		switch {
		case flag != nil:
			t.insecure, t.reason = *flag, "--otlp-insecure"
			if envName != "" && parseOTelBool(envVal) != *flag {
				return t, fmt.Errorf("--otlp-insecure=%v contradicts %s=%s", *flag, envName, envVal)
			}
		case envName != "":
			t.insecure, t.reason = parseOTelBool(envVal), envName
		case tlsEnv != "":
			t.reason = tlsEnv
		case isLoopback(endpoint):
			t.insecure, t.reason = true, "loopback endpoint"
		default:
			t.reason = "non-loopback endpoint"
		}
	}
	if t.insecure && tlsEnv != "" {
		return t, fmt.Errorf("plaintext OTLP (%s) contradicts %s, which makes the exporter use TLS", t.reason, tlsEnv)
	}
	return t, nil
}

// options builds the exporter options for t. Options apply after the
// exporter's own environment variables, so they override a transport the
// environment implied (an http:// OTEL_EXPORTER_OTLP_ENDPOINT behind an --otlp
// host:port, say). A TLS transport with a certificate from the environment
// passes no credentials: the exporter's certificate credentials take precedence
// over everything else, and t already rejected plaintext with a certificate.
func (t otlpTransport) options(getenv func(string) string) []otlpmetricgrpc.Option {
	var opts []otlpmetricgrpc.Option
	if t.url {
		opts = append(opts, otlpmetricgrpc.WithEndpointURL(t.endpoint))
	} else {
		opts = append(opts, otlpmetricgrpc.WithEndpoint(t.endpoint))
	}
	if t.insecure {
		return append(opts, otlpmetricgrpc.WithInsecure())
	}
	if name, _ := firstSetEnv(otlpTLSEnv, getenv); name == "" {
		// TLS with the system roots, as the exporter's own default.
		opts = append(opts, otlpmetricgrpc.WithTLSCredentials(credentials.NewTLS(nil)))
	}
	return opts
}

func firstSetEnv(names []string, getenv func(string) string) (name, value string) {
	for _, n := range names {
		if v := strings.TrimSpace(getenv(n)); v != "" {
			return n, v
		}
	}
	return "", ""
}

// parseOTelBool parses a boolean the way the exporter does: only "true",
// case-insensitively, is true.
func parseOTelBool(v string) bool { return strings.EqualFold(v, "true") }

func isLoopback(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
