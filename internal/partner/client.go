package partner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

// Origem da cotação devolvida à corretora (campo `source`): de onde saiu o prêmio.
const (
	SourcePartner    = "partner"     // consulta feita agora à parceira
	SourceCache      = "cache"       // cotação recente (dentro do TTL), originada numa consulta anterior
	SourceStaleCache = "stale_cache" // cotação vencida, servida só porque a parceira estava indisponível
)

type Quote struct {
	Partner         string `json:"partner"`
	QuoteID         string `json:"quote_id"`
	PremiumCents    int64  `json:"premium_cents"`
	Currency        string `json:"currency"`
	CoverageCents   int64  `json:"coverage_cents"`
	ValidForSeconds int64  `json:"valid_for_seconds"`
	// Source e AgeSeconds são da plataforma, nunca da parceira (o cliente os sobrescreve). O prêmio
	// e o quote_id são sempre os da consulta original: é o que mantém a cotação auditável.
	Source     string `json:"source,omitempty"`
	AgeSeconds int64  `json:"age_seconds,omitempty"`
}

// DefaultTimeout é o teto de espera por uma parceira (docs/sad.md, seção 4.1): 18% acima do pior
// caso da partner-slow (1500 ms + 200 ms de jitter).
const DefaultTimeout = 2 * time.Second

type Client struct {
	http *http.Client

	breakerCfg *BreakerConfig // nil: sem breaker
	metrics    *platform.Metrics
	mu         sync.Mutex
	breakers   map[string]*gobreaker.CircuitBreaker[Quote]
}

type ClientOption func(*Client)

// WithTimeout troca o teto de espera por parceira. O estouro vira *Error{Timeout: true} e conta
// como falha para o breaker.
func WithTimeout(d time.Duration) ClientOption { return func(c *Client) { c.http.Timeout = d } }

// WithBreaker liga um circuit breaker POR PARCEIRA, criado na primeira chamada a ela.
func WithBreaker(cfg BreakerConfig) ClientOption { return func(c *Client) { c.breakerCfg = &cfg } }

func WithMetrics(m *platform.Metrics) ClientOption { return func(c *Client) { c.metrics = m } }

func NewClient(opts ...ClientOption) *Client {
	c := &Client{
		http:     &http.Client{Timeout: DefaultTimeout, Transport: platform.InstrumentTransport(http.DefaultTransport)},
		breakers: map[string]*gobreaker.CircuitBreaker[Quote]{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

const responseLimit = 1 << 20

func (c *Client) breaker(name string) *gobreaker.CircuitBreaker[Quote] {
	if c.breakerCfg == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.breakers[name]
	if !ok {
		b = newBreaker(name, *c.breakerCfg, c.metrics)
		c.breakers[name] = b
	}
	return b
}

// Quote consulta a parceira atrás do breaker. Com o circuito aberto, a requisição HTTP NÃO sai:
// volta um *Error com ShortCircuit=true sem tocar a rede (nem pagar os R$ 0,04 da consulta).
func (c *Client) Quote(ctx context.Context, p platform.Partner, request any) (Quote, error) {
	b := c.breaker(p.Name)
	if b == nil {
		return c.call(ctx, p, request)
	}

	quote, err := b.Execute(func() (Quote, error) { return c.call(ctx, p, request) })
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		return Quote{}, &Error{Partner: p.Name, ShortCircuit: true, Reason: "circuit breaker " + stateName(b.State()), Cause: err}
	}
	return quote, err
}

func (c *Client) call(ctx context.Context, p platform.Partner, request any) (quote Quote, err error) {
	start := time.Now()
	defer func() {
		outcome := "ok"
		var failure *Error
		if errors.As(err, &failure) {
			outcome = "error"
			if failure.Timeout {
				outcome = "timeout"
			}
		}
		c.metrics.PartnerCall(ctx, p.Name, outcome, time.Since(start).Seconds())
	}()

	body, err := json.Marshal(request)
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Reason: fmt.Sprintf("invalid request: %v", err), Cause: err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/quotes", bytes.NewReader(body))
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Reason: err.Error(), Cause: err}
	}
	req.Header.Set("Content-Type", "application/json")

	response, err := c.http.Do(req)
	if err != nil {
		var netErr net.Error
		timedOut := errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
		return Quote{}, &Error{Partner: p.Name, Reason: err.Error(), Timeout: timedOut, Cause: err}
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, responseLimit))
		return Quote{}, &Error{
			Partner: p.Name,
			Status:  response.StatusCode,
			Reason:  fmt.Sprintf("partner replied %d", response.StatusCode),
		}
	}

	if err := json.NewDecoder(io.LimitReader(response.Body, responseLimit)).Decode(&quote); err != nil {
		// O corpo é lido sob o mesmo Timeout do cliente: lentidão aqui também é timeout.
		var netErr net.Error
		timedOut := errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
		return Quote{}, &Error{Partner: p.Name, Reason: fmt.Sprintf("unreadable response: %v", err), Timeout: timedOut, Cause: err}
	}

	quote.Partner = p.Name
	quote.Source, quote.AgeSeconds = SourcePartner, 0
	return quote, nil
}

type Error struct {
	Partner      string
	Status       int
	Reason       string
	Timeout      bool  // estourou o teto de espera
	ShortCircuit bool  // o breaker não deixou a chamada sair
	Cause        error // erro original, para errors.Is/As
}

func (e *Error) Error() string {
	return fmt.Sprintf("partner %s: %s", e.Partner, e.Reason)
}

func (e *Error) Unwrap() error { return e.Cause }
