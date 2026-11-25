package partner

import (
	"context"
	"errors"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

// Os três estados do circuit breaker, com os nomes que aparecem nos rótulos from/to das métricas e
// nos atributos de span (docs/sad.md, seção 4.1).
const (
	StateClosed   = "closed"    // chamadas passam; falhas consecutivas são contadas
	StateOpen     = "open"      // a parceira NÃO é chamada: a requisição é curto-circuitada
	StateHalfOpen = "half_open" // só HalfOpenMax sondas passam; as demais são curto-circuitadas
)

// BreakerConfig são os parâmetros de UM breaker; cada parceira recebe o seu (escopo: por parceira).
type BreakerConfig struct {
	ConsecutiveFailures uint32        // falhas seguidas (inclui estouro de timeout) que abrem o circuito
	OpenFor             time.Duration // tempo em aberto antes de passar a meio aberto
	HalfOpenMax         uint32        // sondas no meio aberto; todas com sucesso fecham, uma falha reabre
}

func stateName(s gobreaker.State) string {
	switch s {
	case gobreaker.StateOpen:
		return StateOpen
	case gobreaker.StateHalfOpen:
		return StateHalfOpen
	default:
		return StateClosed
	}
}

func newBreaker(partner string, cfg BreakerConfig, m *platform.Metrics) *gobreaker.CircuitBreaker[Quote] {
	cb := gobreaker.NewCircuitBreaker[Quote](gobreaker.Settings{
		Name:         partner,
		MaxRequests:  cfg.HalfOpenMax,
		Timeout:      cfg.OpenFor,
		ReadyToTrip:  func(c gobreaker.Counts) bool { return c.ConsecutiveFailures >= cfg.ConsecutiveFailures },
		IsSuccessful: isHealthy,
		OnStateChange: func(name string, from, to gobreaker.State) {
			m.BreakerTransition(name, stateName(from), stateName(to))
		},
	})
	m.WatchBreaker(partner, func() string { return stateName(cb.State()) })
	return cb
}

// isHealthy decide o que conta como falha para o breaker: erro de transporte, estouro do timeout
// e resposta 5xx. Um 4xx é problema do NOSSO pedido, não saúde da parceira. O cancelamento feito
// pela corretora que desistiu também não é falha da parceira.
func isHealthy(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return true
	}
	var failure *Error
	return errors.As(err, &failure) && failure.Status >= 400 && failure.Status < 500
}
