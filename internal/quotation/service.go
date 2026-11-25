package quotation

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/cache"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

type Quoter interface {
	Quote(ctx context.Context, p platform.Partner, request any) (partner.Quote, error)
}

// Cache é o que o serviço precisa do Redis (internal/cache); em teste entra um mapa em memória.
type Cache interface {
	Get(ctx context.Context, key string) (cache.Entry, bool)
	Set(ctx context.Context, key string, e cache.Entry, retain time.Duration)
}

// UnavailableError: nenhuma parceira respondeu e não havia cotação anterior utilizável. É a
// "recusa explícita" do fallback: a API a traduz em 503 com missing_partners, e não em 502.
type UnavailableError struct{ Missing []string }

func (e *UnavailableError) Error() string {
	return "no partner quote available: " + strings.Join(e.Missing, ", ")
}

type Service struct {
	partners []platform.Partner
	quoter   Quoter

	// Sem nenhuma Option o serviço é o original do starter: em série e tudo-ou-nada. É isso que
	// mantém verdes os testes que já vinham no repositório; main.go liga tudo.
	cache    Cache
	ttl      time.Duration
	stale    time.Duration
	tolerant bool // fallback: parceira que falha não derruba a cotação
	parallel bool // consulta as parceiras ao mesmo tempo
	metrics  *platform.Metrics
	now      func() time.Time
}

type Option func(*Service)

// WithCache liga o cache. ttl é a vida fresca; stale é quanto além do ttl a entrada ainda pode
// servir de fallback com a parceira indisponível (e, portanto, quanto o Redis a retém).
func WithCache(c Cache, ttl, stale time.Duration) Option {
	return func(s *Service) { s.cache, s.ttl, s.stale = c, ttl, stale }
}

// WithFallback liga a resposta degradada: parceira ausente vira missing_partners, cotação vencida
// vira source=stale_cache, e só a ausência total vira UnavailableError.
func WithFallback() Option { return func(s *Service) { s.tolerant = true } }

// WithParallel sobrepõe as consultas às parceiras: a latência passa de soma para máximo.
func WithParallel() Option { return func(s *Service) { s.parallel = true } }

func WithMetrics(m *platform.Metrics) Option { return func(s *Service) { s.metrics = m } }

// WithClock existe para testar idade de cache sem sleep.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

func NewService(partners []platform.Partner, quoter Quoter, opts ...Option) *Service {
	s := &Service{partners: partners, quoter: quoter, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var tracer = otel.Tracer("quotation-api/quotation")

type outcome struct {
	quote   partner.Quote
	missing bool
	err     error
}

func (s *Service) Quote(ctx context.Context, tenant string, request Request) (Response, error) {
	start := time.Now()
	forPartner := partnerRequest{Broker: tenant, Request: request}

	results := make([]outcome, len(s.partners))
	if s.parallel {
		var wg sync.WaitGroup
		for i := range s.partners {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = s.fetch(ctx, tenant, s.partners[i], request, forPartner)
			}()
		}
		wg.Wait()
	} else {
		for i := range s.partners {
			results[i] = s.fetch(ctx, tenant, s.partners[i], request, forPartner)
			if !s.tolerant && results[i].err != nil {
				return Response{}, results[i].err
			}
		}
	}

	quotes := make([]partner.Quote, 0, len(s.partners))
	var missing []string
	degraded, fromCache := false, false
	for i, r := range results {
		switch {
		case r.missing:
			missing = append(missing, s.partners[i].Name)
		case !s.tolerant && r.err != nil:
			return Response{}, r.err
		default:
			quotes = append(quotes, r.quote)
			fromCache = fromCache || r.quote.Source != partner.SourcePartner
			if r.quote.Source == partner.SourceStaleCache {
				degraded = true
				s.metrics.Fallback(ctx, "stale_cache")
			}
		}
	}

	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String("tenant_id", tenant),
		attribute.Bool("quote.served_from_cache", fromCache),
		attribute.Int("quote.missing_partners", len(missing)),
	)

	if len(quotes) == 0 {
		s.metrics.Fallback(ctx, "refused")
		span.SetAttributes(attribute.Bool("quote.degraded", true))
		return Response{}, &UnavailableError{Missing: missing}
	}
	if len(missing) > 0 {
		degraded = true
		s.metrics.Fallback(ctx, "partial")
	}
	span.SetAttributes(attribute.Bool("quote.degraded", degraded))

	sort.Slice(quotes, func(i, j int) bool {
		return quotes[i].PremiumCents < quotes[j].PremiumCents
	})

	return Response{
		TenantID:        tenant,
		Quotes:          quotes,
		Degraded:        degraded,
		MissingPartners: missing,
		ElapsedMs:       time.Since(start).Milliseconds(),
	}, nil
}

// fetch resolve UMA parceira: cache fresco → consulta à parceira (atrás do breaker) → cotação
// vencida, só se a parceira falhou e o fallback está ligado.
func (s *Service) fetch(ctx context.Context, tenant string, p platform.Partner, request Request, forPartner partnerRequest) outcome {
	ctx, span := tracer.Start(ctx, "partner.quote", trace.WithAttributes(
		attribute.String("partner", p.Name), attribute.String("tenant_id", tenant)))
	defer span.End()

	var entry cache.Entry
	var found bool
	key := request.CacheKey(tenant, p.Name)
	if s.cache != nil {
		entry, found = s.cache.Get(ctx, key)
		if found && s.now().Sub(entry.StoredAt) <= s.ttl {
			s.metrics.CacheLookup(ctx, tenant, "hit")
			span.SetAttributes(attribute.String("cache.result", "hit"))
			return outcome{quote: aged(entry, partner.SourceCache, s.now())}
		}
		s.metrics.CacheLookup(ctx, tenant, "miss")
		span.SetAttributes(attribute.String("cache.result", "miss"))
	}

	quote, err := s.quoter.Quote(ctx, p, forPartner)
	if err == nil {
		if s.cache != nil {
			s.cache.Set(ctx, key, cache.Entry{Quote: quote, StoredAt: s.now()}, s.ttl+s.stale)
		}
		return outcome{quote: quote}
	}

	var failure *partner.Error
	if errors.As(err, &failure) {
		span.SetAttributes(
			attribute.Bool("breaker.short_circuited", failure.ShortCircuit),
			attribute.Bool("partner.timeout", failure.Timeout),
		)
	}
	if !s.tolerant {
		return outcome{err: err}
	}
	if found && s.now().Sub(entry.StoredAt) <= s.ttl+s.stale {
		span.SetAttributes(attribute.Bool("cache.stale_served", true))
		return outcome{quote: aged(entry, partner.SourceStaleCache, s.now())}
	}
	return outcome{missing: true, err: err}
}

func aged(e cache.Entry, source string, now time.Time) partner.Quote {
	q := e.Quote
	q.Source, q.AgeSeconds = source, int64(now.Sub(e.StoredAt).Seconds())
	return q
}
