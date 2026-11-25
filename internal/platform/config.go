package platform

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port      string
	Partners  []Partner
	Tenants   []string
	Telemetry Telemetry
	// Resilience e Cache: decisões de docs/sad.md (seção 4); cada default tem a defesa lá.
	Resilience Resilience
	Cache      Cache
}

type Partner struct {
	Name    string
	BaseURL string
}

type Telemetry struct {
	ServiceName string
	Endpoint    string
	Enabled     bool
}

type Resilience struct {
	PartnerTimeout     time.Duration // teto de espera por parceira; o estouro conta como falha no breaker
	BreakerFailures    uint32        // falhas consecutivas que abrem o circuito
	BreakerOpenFor     time.Duration // quanto o circuito fica aberto antes de passar a meio aberto
	BreakerHalfOpenMax uint32        // sondas admitidas no meio aberto; todas com sucesso fecham o circuito
}

type Cache struct {
	Addr     string        // vazio desliga o cache
	TTL      time.Duration // vida "fresca" da cotação
	StaleTTL time.Duration // por quanto tempo além do TTL a cotação ainda serve de fallback
}

const defaultEndpoints = "partner-slow=http://localhost:9001," +
	"partner-flaky=http://localhost:9002," +
	"partner-degrading=http://localhost:9003"

const defaultTenants = "corretora-a,corretora-b"

const defaultCollector = "http://localhost:4317"

const defaultServiceName = "quotation-api"

// maxCacheSeconds é o teto comercial do cenário: as seguradoras honram o prêmio por 24 horas.
const maxCacheSeconds = 24 * 60 * 60

func LoadConfig(env func(string) string) (Config, error) {
	cfg := Config{Port: text(env, "PORT", "8080")}

	var err error
	if cfg.Partners, err = parsePartners(text(env, "PARTNER_ENDPOINTS", defaultEndpoints)); err != nil {
		return Config{}, err
	}
	if cfg.Tenants, err = parseTenants(text(env, "TENANTS", defaultTenants)); err != nil {
		return Config{}, err
	}
	if cfg.Telemetry, err = parseTelemetry(env); err != nil {
		return Config{}, err
	}
	if cfg.Resilience, err = parseResilience(env); err != nil {
		return Config{}, err
	}
	if cfg.Cache, err = parseCache(env); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseResilience(env func(string) string) (Resilience, error) {
	timeout, err := whole(env, "PARTNER_TIMEOUT_MS", 2000)
	if err != nil {
		return Resilience{}, err
	}
	failures, err := whole(env, "BREAKER_CONSECUTIVE_FAILURES", 5)
	if err != nil {
		return Resilience{}, err
	}
	openFor, err := whole(env, "BREAKER_OPEN_SECONDS", 10)
	if err != nil {
		return Resilience{}, err
	}
	probes, err := whole(env, "BREAKER_HALF_OPEN_MAX", 2)
	if err != nil {
		return Resilience{}, err
	}
	return Resilience{
		PartnerTimeout:     time.Duration(timeout) * time.Millisecond,
		BreakerFailures:    uint32(failures),
		BreakerOpenFor:     time.Duration(openFor) * time.Second,
		BreakerHalfOpenMax: uint32(probes),
	}, nil
}

func parseCache(env func(string) string) (Cache, error) {
	ttl, err := whole(env, "CACHE_TTL_SECONDS", 300)
	if err != nil {
		return Cache{}, err
	}
	stale, err := whole(env, "CACHE_STALE_SECONDS", 3600)
	if err != nil {
		return Cache{}, err
	}
	if ttl+stale > maxCacheSeconds {
		return Cache{}, fmt.Errorf("CACHE_TTL_SECONDS + CACHE_STALE_SECONDS = %d exceeds the 24h commercial ceiling (%d s)", ttl+stale, maxCacheSeconds)
	}
	return Cache{
		Addr:     strings.TrimSpace(text(env, "REDIS_ADDR", "localhost:6379")),
		TTL:      time.Duration(ttl) * time.Second,
		StaleTTL: time.Duration(stale) * time.Second,
	}, nil
}

// whole lê um inteiro positivo; zero ou negativo nunca é um parâmetro válido aqui.
func whole(env func(string) string, key string, fallback int) (int, error) {
	n, err := strconv.Atoi(text(env, key, strconv.Itoa(fallback)))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s: %q is not a positive integer", key, env(key))
	}
	return n, nil
}

func parseTelemetry(env func(string) string) (Telemetry, error) {
	telemetry := Telemetry{
		ServiceName: text(env, "OTEL_SERVICE_NAME", defaultServiceName),
		Endpoint:    strings.TrimSpace(text(env, "OTEL_EXPORTER_OTLP_ENDPOINT", defaultCollector)),
	}

	switch disabled := strings.ToLower(strings.TrimSpace(env("OTEL_SDK_DISABLED"))); disabled {
	case "", "false":
		telemetry.Enabled = true
	case "true":
		return Telemetry{Enabled: false}, nil
	default:
		return Telemetry{}, fmt.Errorf("OTEL_SDK_DISABLED: %q is neither true nor false", disabled)
	}

	if u, err := url.Parse(telemetry.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		return Telemetry{}, fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT: %q is not an absolute URL", telemetry.Endpoint)
	}
	return telemetry, nil
}

func parsePartners(raw string) ([]Partner, error) {
	var partners []Partner
	seen := map[string]bool{}

	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		name, endpoint, found := strings.Cut(item, "=")
		name, endpoint = strings.TrimSpace(name), strings.TrimSpace(endpoint)
		if !found || name == "" || endpoint == "" {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: %q is not in the name=url format", item)
		}

		if u, err := url.Parse(endpoint); err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: %q is not an absolute URL", endpoint)
		}
		if seen[name] {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: partner %q is repeated", name)
		}

		seen[name] = true
		partners = append(partners, Partner{Name: name, BaseURL: strings.TrimRight(endpoint, "/")})
	}

	if len(partners) == 0 {
		return nil, fmt.Errorf("PARTNER_ENDPOINTS: at least one partner is required")
	}
	return partners, nil
}

func parseTenants(raw string) ([]string, error) {
	var tenants []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tenants = append(tenants, t)
		}
	}
	if len(tenants) == 0 {
		return nil, fmt.Errorf("TENANTS: at least one broker is required")
	}
	return tenants, nil
}

func Environment() func(string) string { return os.Getenv }

func text(env func(string) string, key, fallback string) string {
	if v := env(key); v != "" {
		return v
	}
	return fallback
}
