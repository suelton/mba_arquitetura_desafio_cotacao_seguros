// Package cache guarda no Redis a cotação de UMA parceira para UM risco de UMA corretora
// (docs/sad.md, seção 4.2). A chave sempre contém a corretora: ver quotation.Request.CacheKey.
package cache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
)

// opTimeout limita cada ida ao Redis: cache lento ou fora do ar vira miss, nunca latência na cotação.
const opTimeout = 100 * time.Millisecond

// Entry é o que se guarda: a cotação como a parceira a devolveu (mesmo quote_id e mesmo prêmio) e
// o instante em que foi comprada. Sem o instante não há idade para mostrar à corretora.
type Entry struct {
	Quote    partner.Quote `json:"quote"`
	StoredAt time.Time     `json:"stored_at"`
}

type Redis struct{ client *redis.Client }

func NewRedis(addr string) *Redis {
	return &Redis{client: redis.NewClient(&redis.Options{
		Addr: addr, DialTimeout: opTimeout, ReadTimeout: opTimeout, WriteTimeout: opTimeout, MaxRetries: -1,
	})}
}

func (r *Redis) Close() error { return r.client.Close() }

// Get devolve a entrada se existir. Qualquer erro do Redis é tratado como ausência: a cotação
// continua funcionando, só sem cache (degradação, não parada).
func (r *Redis) Get(ctx context.Context, key string) (Entry, bool) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	raw, err := r.client.Get(ctx, key).Bytes()
	if err != nil {
		return Entry{}, false // redis.Nil (miss) ou Redis indisponível
	}
	var e Entry
	if json.Unmarshal(raw, &e) != nil {
		return Entry{}, false
	}
	return e, true
}

// Set grava com expiração física `retain`: depois dela o Redis descarta a entrada sozinho. É assim
// que o prazo de descarte da LGPD é cumprido sem job nenhum. Falha de escrita não derruba a cotação.
func (r *Redis) Set(ctx context.Context, key string, e Entry, retain time.Duration) {
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	_ = r.client.Set(ctx, key, raw, retain).Err()
}
