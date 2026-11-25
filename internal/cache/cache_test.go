package cache

import (
	"context"
	"testing"
	"time"
)

// RF-08: Redis indisponível vira miss, rápido, e a escrita não derruba nada.
// Porta 1 recusa conexão na hora, sem depender de relógio.
func TestUnreachableRedisIsAMissNotAnError(t *testing.T) {
	r := NewRedis("127.0.0.1:1")
	defer func() { _ = r.Close() }()

	start := time.Now()
	if _, found := r.Get(context.Background(), "quote:v1:corretora-a:partner-slow:x"); found {
		t.Fatal("unreachable Redis reported a hit")
	}
	r.Set(context.Background(), "k", Entry{StoredAt: time.Now()}, time.Minute)

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Redis failure cost %s; the cap per operation is 100 ms", elapsed)
	}
}
