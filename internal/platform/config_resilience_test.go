package platform

import (
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResilienceAndCacheDefaultsAreTheSADValues(t *testing.T) {
	cfg, err := LoadConfig(envOf(nil))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	r, c := cfg.Resilience, cfg.Cache
	if r.PartnerTimeout != 2*time.Second || r.BreakerFailures != 5 || r.BreakerOpenFor != 10*time.Second || r.BreakerHalfOpenMax != 2 {
		t.Errorf("resilience defaults diverge from docs/sad.md: %+v", r)
	}
	if c.TTL != 5*time.Minute || c.StaleTTL != time.Hour {
		t.Errorf("cache defaults diverge from docs/sad.md: %+v", c)
	}
}

func TestCacheCannotExceedTheCommercialCeiling(t *testing.T) {
	_, err := LoadConfig(envOf(map[string]string{"CACHE_TTL_SECONDS": "80000", "CACHE_STALE_SECONDS": "10000"}))
	if err == nil {
		t.Fatal("TTL + stale above 24h was accepted")
	}
	if _, err := LoadConfig(envOf(map[string]string{"PARTNER_TIMEOUT_MS": "0"})); err == nil {
		t.Fatal("a zero timeout was accepted: that is the original hole")
	}
}
