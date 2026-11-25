package quotation

import (
	"testing"
	"time"
)

// O Retry-After do 503 acompanha BREAKER_OPEN_SECONDS em vez de um valor fixo.
func TestRetryAfterFollowsTheBreakerOpenTime(t *testing.T) {
	svc := NewService(threePartners, failingQuoter{}, WithFallback())
	api := NewAPI(svc, []string{"corretora-a"}).WithRetryAfter(25 * time.Second)

	response := postQuotes(api.Routes(), "corretora-a", validBody)
	if got := response.Header().Get("Retry-After"); got != "25" {
		t.Fatalf("Retry-After %q, expected 25", got)
	}
}
