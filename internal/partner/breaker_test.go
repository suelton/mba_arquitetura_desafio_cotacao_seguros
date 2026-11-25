package partner

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

const okBody = `{"quote_id":"q-1","premium_cents":100000,"currency":"BRL","coverage_cents":5000000,"valid_for_seconds":300}`

// flakyServer responde 503 enquanto healthy for falso e conta quantas requisições chegaram.
func flakyServer(healthy *atomic.Bool, calls *atomic.Int32) (platform.Partner, func()) {
	return testPartner(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if healthy.Load() {
			_, _ = w.Write([]byte(okBody))
			return
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
}

// O breaker abre depois de N falhas consecutivas e, ABERTO, a parceira não é chamada: o contador
// do servidor não anda e o erro diz que foi curto-circuito. Sem sleep: o tempo em aberto é 1 h.
func TestBreakerOpensAfterConsecutiveFailuresAndStopsCallingThePartner(t *testing.T) {
	var healthy atomic.Bool
	var calls atomic.Int32
	p, closeServer := flakyServer(&healthy, &calls)
	defer closeServer()

	client := NewClient(WithBreaker(BreakerConfig{ConsecutiveFailures: 3, OpenFor: time.Hour, HalfOpenMax: 2}))

	for i := 0; i < 3; i++ {
		_, err := client.Quote(context.Background(), p, map[string]string{})
		var failure *Error
		if !errors.As(err, &failure) || failure.ShortCircuit || failure.Status != 503 {
			t.Fatalf("call %d: expected a real 503, got %v", i+1, err)
		}
	}
	if state := stateName(client.breaker(p.Name).State()); state != StateOpen {
		t.Fatalf("after 3 consecutive failures the breaker is %q, expected %q", state, StateOpen)
	}

	before := calls.Load()
	for i := 0; i < 10; i++ {
		_, err := client.Quote(context.Background(), p, map[string]string{})
		var failure *Error
		if !errors.As(err, &failure) || !failure.ShortCircuit {
			t.Fatalf("open breaker let call through or returned %v", err)
		}
	}
	if after := calls.Load(); after != before {
		t.Fatalf("partner received %d calls while the breaker was open, expected 0", after-before)
	}
}

// closed → open → half_open → closed, e o half_open que falha reabre. OpenFor de 1 ms faz o
// circuito passar a meio aberto quase de imediato; awaitState espera a CONDIÇÃO (com teto de 5 s), não um tempo calibrado.
func TestBreakerHalfOpenClosesOnProbesAndReopensOnFailure(t *testing.T) {
	var healthy atomic.Bool
	var calls atomic.Int32
	p, closeServer := flakyServer(&healthy, &calls)
	defer closeServer()

	client := NewClient(WithBreaker(BreakerConfig{ConsecutiveFailures: 2, OpenFor: time.Millisecond, HalfOpenMax: 2}))
	state := func() string { return stateName(client.breaker(p.Name).State()) }
	quote := func() error { _, err := client.Quote(context.Background(), p, map[string]string{}); return err }

	_ = quote()
	_ = quote()
	awaitState(t, state, StateHalfOpen) // abriu e o intervalo já venceu: a próxima chamada é uma sonda

	if err := quote(); err == nil { // sonda contra parceira ainda fora do ar
		t.Fatal("probe against a failing partner reported success")
	}
	awaitState(t, state, StateHalfOpen) // a sonda falhou: reabriu (open) e venceu de novo (half_open)

	healthy.Store(true)
	for i := 0; i < 2; i++ {
		if err := quote(); err != nil {
			t.Fatalf("probe %d with the partner healthy: %v", i+1, err)
		}
	}
	if state() != StateClosed {
		t.Fatalf("2 successful probes must close the circuit, got %q", state())
	}
}

// O estouro do timeout é falha para o breaker: é isso que permite à partner-degrading, que nunca
// devolve erro, abrir o circuito.
func TestTimeoutCountsAsFailureForTheBreaker(t *testing.T) {
	release := make(chan struct{})
	p, closeServer := testPartner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer closeServer()
	defer close(release)

	client := NewClient(WithTimeout(20*time.Millisecond),
		WithBreaker(BreakerConfig{ConsecutiveFailures: 2, OpenFor: time.Hour, HalfOpenMax: 1}))

	for i := 0; i < 2; i++ {
		_, err := client.Quote(context.Background(), p, map[string]string{})
		var failure *Error
		if !errors.As(err, &failure) || !failure.Timeout {
			t.Fatalf("call %d: expected a timeout error, got %v", i+1, err)
		}
	}
	if state := stateName(client.breaker(p.Name).State()); state != StateOpen {
		t.Fatalf("two timeouts left the breaker %q, expected open", state)
	}
}

// Um breaker por parceira: a falha de uma não abre o circuito da outra.
func TestBreakersAreIndependentPerPartner(t *testing.T) {
	var down, up atomic.Bool
	var callsDown, callsUp atomic.Int32
	up.Store(true)
	pDown, close1 := flakyServer(&down, &callsDown)
	defer close1()
	pUp, close2 := flakyServer(&up, &callsUp)
	defer close2()
	pUp.Name = "partner-slow"

	client := NewClient(WithBreaker(BreakerConfig{ConsecutiveFailures: 2, OpenFor: time.Hour, HalfOpenMax: 1}))
	for i := 0; i < 2; i++ {
		_, _ = client.Quote(context.Background(), pDown, map[string]string{})
	}
	if _, err := client.Quote(context.Background(), pUp, map[string]string{}); err != nil {
		t.Fatalf("healthy partner was blocked by another partner's breaker: %v", err)
	}
}

func awaitState(t *testing.T, state func() string, want string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if state() == want {
			return
		}
	}
	t.Fatalf("breaker never reached %q (now %q)", want, state())
}
