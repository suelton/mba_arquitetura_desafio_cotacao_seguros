package platform

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics são as métricas de NEGÓCIO da fronteira com as parceiras (docs/sad.md, seção 6).
// Nenhum rótulo carrega CPF, placa ou quote_id; tenant_id é o único dado de cliente e só entra
// onde o alerta precisa separar corretoras (cache).
//
// Nomes OTel → nomes no Prometheus (o Collector acrescenta _total em contadores e _seconds no
// histograma):
//
//	quotation.breaker.state        → quotation_breaker_state{partner}                    gauge 0=closed 1=half_open 2=open
//	quotation.breaker.transitions  → quotation_breaker_transitions_total{partner,from,to}
//	quotation.cache.requests       → quotation_cache_requests_total{tenant_id,result}    result=hit|miss
//	quotation.partner.duration     → quotation_partner_duration_seconds_*{partner,outcome} outcome=ok|error|timeout
//	quotation.fallback.responses   → quotation_fallback_responses_total{kind}            kind=stale_cache|partial|refused
//
// Todos os métodos aceitam receptor nil, para que testes e usos sem telemetria não precisem de if.
type Metrics struct {
	transitions metric.Int64Counter
	cache       metric.Int64Counter
	duration    metric.Float64Histogram
	fallback    metric.Int64Counter

	mu       sync.Mutex
	watchers map[string]func() string
}

// BreakerStateValue mapeia o estado para o valor do gauge quotation.breaker.state.
var BreakerStateValue = map[string]int64{"closed": 0, "half_open": 1, "open": 2}

func NewMetrics() (*Metrics, error) {
	meter := otel.Meter("quotation-api/business")
	m := &Metrics{watchers: map[string]func() string{}}
	var err error

	if m.transitions, err = meter.Int64Counter("quotation.breaker.transitions",
		metric.WithDescription("Transições de estado do circuit breaker, por parceira")); err != nil {
		return nil, err
	}
	if m.cache, err = meter.Int64Counter("quotation.cache.requests",
		metric.WithDescription("Consultas ao cache de cotações: hit ou miss")); err != nil {
		return nil, err
	}
	if m.duration, err = meter.Float64Histogram("quotation.partner.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Latência da consulta à parceira, medida pelo cliente (não conta chamadas curto-circuitadas)"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 1, 1.5, 2, 2.5, 5)); err != nil {
		return nil, err
	}
	if m.fallback, err = meter.Int64Counter("quotation.fallback.responses",
		metric.WithDescription("Respostas degradadas devolvidas à corretora, por tipo")); err != nil {
		return nil, err
	}

	gauge, err := meter.Int64ObservableGauge("quotation.breaker.state",
		metric.WithDescription("Estado atual do breaker por parceira: 0=closed 1=half_open 2=open"))
	if err != nil {
		return nil, err
	}
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		// Copia sob lock e chama fora dele: state() pode disparar OnStateChange → BreakerTransition.
		m.mu.Lock()
		watchers := make(map[string]func() string, len(m.watchers))
		for partner, state := range m.watchers {
			watchers[partner] = state
		}
		m.mu.Unlock()
		for partner, state := range watchers {
			o.ObserveInt64(gauge, BreakerStateValue[state()], metric.WithAttributes(attribute.String("partner", partner)))
		}
		return nil
	}, gauge)
	return m, err
}

// WatchBreaker registra como ler o estado ATUAL de um breaker. O gauge lê no momento da coleta (e não
// na última transição): o estado aberto → meio aberto só acontece quando alguém o consulta, e sem
// isto um circuito que já venceu continuaria aparecendo como aberto num sistema sem tráfego.
func (m *Metrics) WatchBreaker(partner string, state func() string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.watchers[partner] = state
	m.mu.Unlock()
}

func (m *Metrics) BreakerTransition(partner, from, to string) {
	if m == nil {
		return
	}
	m.transitions.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("partner", partner), attribute.String("from", from), attribute.String("to", to)))
}

func (m *Metrics) CacheLookup(ctx context.Context, tenant, result string) {
	if m == nil {
		return
	}
	m.cache.Add(ctx, 1, metric.WithAttributes(attribute.String("tenant_id", tenant), attribute.String("result", result)))
}

func (m *Metrics) PartnerCall(ctx context.Context, partner, outcome string, seconds float64) {
	if m == nil {
		return
	}
	m.duration.Record(ctx, seconds, metric.WithAttributes(attribute.String("partner", partner), attribute.String("outcome", outcome)))
}

func (m *Metrics) Fallback(ctx context.Context, kind string) {
	if m == nil {
		return
	}
	m.fallback.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
}
