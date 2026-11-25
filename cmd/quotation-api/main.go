package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/cache"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/quotation"
)

func main() {
	cfg, err := platform.LoadConfig(platform.Environment())
	if err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	ctx, stopListening := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopListening()

	stopTelemetry, err := platform.StartTelemetry(context.Background(), cfg.Telemetry)
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}

	// As métricas de negócio só podem nascer depois de StartTelemetry, que instala o MeterProvider.
	metrics, err := platform.NewMetrics()
	if err != nil {
		log.Fatalf("business metrics: %v", err)
	}

	client := partner.NewClient(
		partner.WithTimeout(cfg.Resilience.PartnerTimeout),
		partner.WithBreaker(partner.BreakerConfig{
			ConsecutiveFailures: cfg.Resilience.BreakerFailures,
			OpenFor:             cfg.Resilience.BreakerOpenFor,
			HalfOpenMax:         cfg.Resilience.BreakerHalfOpenMax,
		}),
		partner.WithMetrics(metrics),
	)

	options := []quotation.Option{quotation.WithFallback(), quotation.WithParallel(), quotation.WithMetrics(metrics)}
	if cfg.Cache.Addr != "" {
		store := cache.NewRedis(cfg.Cache.Addr)
		defer func() { _ = store.Close() }()
		options = append(options, quotation.WithCache(store, cfg.Cache.TTL, cfg.Cache.StaleTTL))
	}

	api := quotation.NewAPI(quotation.NewService(cfg.Partners, client, options...), cfg.Tenants)

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           platform.InstrumentHandler(api.Routes(), cfg.Telemetry.ServiceName),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		log.Print("quotation-api: signal received, shutting down")
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Printf("quotation-api: forced shutdown: %v", err)
		}
		if err := stopTelemetry(shutdown); err != nil {
			log.Printf("quotation-api: telemetry shutdown: %v", err)
		}
	}()

	log.Printf("quotation-api listening on %s | partners=%d brokers=%v timeout=%s breaker=%d falhas/%s/%d sondas cache=%q ttl=%s stale=%s",
		server.Addr, len(cfg.Partners), cfg.Tenants, cfg.Resilience.PartnerTimeout,
		cfg.Resilience.BreakerFailures, cfg.Resilience.BreakerOpenFor, cfg.Resilience.BreakerHalfOpenMax,
		cfg.Cache.Addr, cfg.Cache.TTL, cfg.Cache.StaleTTL)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server stopped: %v", err)
	}
}
