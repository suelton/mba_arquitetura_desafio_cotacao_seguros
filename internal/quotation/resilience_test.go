package quotation

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/cache"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

type memoryCache struct {
	mu      sync.Mutex
	entries map[string]cache.Entry
}

func newMemoryCache() *memoryCache { return &memoryCache{entries: map[string]cache.Entry{}} }

func (m *memoryCache) Get(_ context.Context, key string) (cache.Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	return e, ok
}

func (m *memoryCache) Set(_ context.Context, key string, e cache.Entry, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[key] = e
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

const ttl, stale = 5 * time.Minute, time.Hour

func resilient(q Quoter, c Cache, clk *clock) *Service {
	return NewService(threePartners, q, WithFallback(), WithParallel(), WithCache(c, ttl, stale), WithClock(clk.now))
}

// Segunda cotação do mesmo risco pela mesma corretora sai do cache: a parceira não é consultada
// e o prêmio e o quote_id são os da consulta original.
func TestCacheHitDoesNotCallThePartners(t *testing.T) {
	quoter := &recorder{premiums: defaultPremiums()}
	svc := resilient(quoter, newMemoryCache(), &clock{time.Now()})

	first, _ := svc.Quote(context.Background(), "corretora-a", validRequest())
	callsAfterFirst := quoter.count()
	second, err := svc.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	if callsAfterFirst != 3 || quoter.count() != 3 {
		t.Fatalf("partner calls: %d after the 1st request, %d after the 2nd; expected 3 and 3", callsAfterFirst, quoter.count())
	}
	for i, q := range second.Quotes {
		if q.Source != partner.SourceCache || q.PremiumCents != first.Quotes[i].PremiumCents {
			t.Errorf("quote %d: source %q premium %d; expected cache with the original premium %d", i, q.Source, q.PremiumCents, first.Quotes[i].PremiumCents)
		}
	}
	if second.Degraded {
		t.Error("a fresh cache hit is not degraded")
	}
}

// O teste que protege o contrato de isolamento: a mesma placa cotada pela corretora A nunca
// aparece para a B, e a chave carrega a corretora.
func TestCacheKeyIsolatesBrokers(t *testing.T) {
	quoter := &recorder{premiums: defaultPremiums()}
	svc := resilient(quoter, newMemoryCache(), &clock{time.Now()})

	_, _ = svc.Quote(context.Background(), "corretora-a", validRequest())
	b, err := svc.Quote(context.Background(), "corretora-b", validRequest())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	if quoter.count() != 6 {
		t.Fatalf("%d partner calls; corretora-b must NOT be served from corretora-a's cache (expected 6)", quoter.count())
	}
	for _, q := range b.Quotes {
		if q.Source != partner.SourcePartner {
			t.Errorf("corretora-b got a quote with source %q", q.Source)
		}
	}

	r := validRequest()
	keyA, keyB := r.CacheKey("corretora-a", "partner-flaky"), r.CacheKey("corretora-b", "partner-flaky")
	if keyA == keyB || !strings.HasPrefix(keyA, "quote:v1:corretora-a:partner-flaky:") {
		t.Fatalf("keys do not carry the tenant: %q / %q", keyA, keyB)
	}
	if strings.Contains(keyA, r.Vehicle.Plate) || strings.Contains(keyA, r.Driver.Document) {
		t.Fatalf("cache key leaks plate or document: %q", keyA)
	}
}

// Parceira cai: com cotação vencida (mas dentro da janela stale) ela é servida, marcada e com
// idade; sem nenhuma, a parceira vira missing_partners. Nunca se inventa prêmio.
func TestFallbackServesStaleAndReportsMissingPartners(t *testing.T) {
	clk := &clock{time.Now()}
	quoter := &recorder{premiums: defaultPremiums()}
	mem := newMemoryCache()
	svc := resilient(quoter, mem, clk)

	if _, err := svc.Quote(context.Background(), "corretora-a", validRequest()); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	clk.t = clk.t.Add(ttl + 10*time.Minute) // vencida, mas dentro de ttl+stale
	quoter.mu.Lock()
	quoter.failOn = "partner-flaky"
	quoter.mu.Unlock()
	response, err := svc.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	var flaky *partner.Quote
	for i := range response.Quotes {
		if response.Quotes[i].Partner == "partner-flaky" {
			flaky = &response.Quotes[i]
		}
	}
	if flaky == nil || flaky.Source != partner.SourceStaleCache || flaky.AgeSeconds < int64((ttl+10*time.Minute).Seconds()) {
		t.Fatalf("expected a stale_cache quote with age, got %+v", flaky)
	}
	if !response.Degraded {
		t.Error("serving a stale quote must mark the response degraded")
	}

	clk.t = clk.t.Add(2 * time.Hour) // além de ttl+stale: a entrada não pode mais servir
	response, err = svc.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if !response.Degraded || len(response.MissingPartners) != 1 || response.MissingPartners[0] != "partner-flaky" || len(response.Quotes) != 2 {
		t.Fatalf("expected partial response missing partner-flaky, got %+v", response)
	}
}

func TestAllPartnersDownWithoutCacheIsAnExplicitRefusal(t *testing.T) {
	failing := &failingQuoter{}
	svc := NewService(threePartners, failing, WithFallback(), WithParallel())

	_, err := svc.Quote(context.Background(), "corretora-a", validRequest())
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) || len(unavailable.Missing) != 3 {
		t.Fatalf("expected UnavailableError with 3 missing partners, got %v", err)
	}

	response := postQuotes(NewAPI(svc, []string{"corretora-a"}).Routes(), "corretora-a", validBody)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d (Retry-After %q), expected 503 with Retry-After: %s", response.Code, response.Header().Get("Retry-After"), response.Body)
	}
}

func TestParallelAggregationOverlapsThePartners(t *testing.T) {
	quoter := &recorder{delay: 40 * time.Millisecond, premiums: defaultPremiums()}
	svc := NewService(threePartners, quoter, WithParallel())

	if _, err := svc.Quote(context.Background(), "corretora-a", validRequest()); err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if peak := quoter.peak.Load(); peak != 3 {
		t.Fatalf("%d concurrent calls at the peak, expected 3", peak)
	}
}

type failingQuoter struct{}

func (failingQuoter) Quote(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
	return partner.Quote{}, &partner.Error{Partner: p.Name, ShortCircuit: true, Reason: "circuit breaker open"}
}

// recorder é um Quoter seguro para uso concorrente (o fakeQuoter do starter não é).
type recorder struct {
	delay    time.Duration
	premiums map[string]int64
	failOn   string

	mu         sync.Mutex
	calls      []string
	concurrent atomic.Int32
	peak       atomic.Int32
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recorder) Quote(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
	if now := r.concurrent.Add(1); now > r.peak.Load() {
		r.peak.Store(now)
	}
	defer r.concurrent.Add(-1)

	r.mu.Lock()
	r.calls = append(r.calls, p.Name)
	failOn := r.failOn
	r.mu.Unlock()
	time.Sleep(r.delay)

	if p.Name == failOn {
		return partner.Quote{}, &partner.Error{Partner: p.Name, Status: 503, Reason: "partner answered 503"}
	}
	return partner.Quote{Partner: p.Name, QuoteID: p.Name + "-q1", PremiumCents: r.premiums[p.Name], Currency: "BRL", Source: partner.SourcePartner}, nil
}
