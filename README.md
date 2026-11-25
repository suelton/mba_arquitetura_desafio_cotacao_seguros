# Prumo Cota — breaker, cache e fallback na fronteira com as seguradoras

Entrega do desafio *Fundamentos de Arquitetura de Solução* (o enunciado está em [`docs/enunciado.md`](docs/enunciado.md)).

- **SAD (entrega 1):** [`docs/sad.md`](docs/sad.md) — oito seções, quatro diagramas C4 em Mermaid, todo número defendido.
- **Evidências (entrega 2):** [`docs/evidencias/`](docs/evidencias/) — tabela completa mais abaixo.

## O coração da solução

A `quotation-api` tinha a disponibilidade igual ao **produto** da disponibilidade de três parceiras instáveis: esperava sem limite, em série, e jogava fora o que já tinha chegado quando uma falhava. Protegi **a fronteira com as parceiras** com três mecanismos que se sustentam entre si. Um **timeout de 2 s que conta como falha** transforma a `partner-degrading` (que nunca devolve erro, só afunda) em sinal para um **circuit breaker por parceira** (5 falhas consecutivas abrem, 10 s aberto, 2 sondas no meio aberto), e com o circuito aberto a chamada **não sai**: nem espera, nem custa os R$ 0,04. Um **cache Redis por parceira**, com chave `quote:v1:{tenant_id}:{partner}:{hash do risco}`, evita pagar de novo a consulta repetida (TTL 300 s, igual à validade que a parceira declara) e guarda a cotação vencida por mais 3.600 s **só** para servir de fallback. O **fallback** devolve o que existe, com a degradação dita no contrato (`degraded`, `missing_partners`, `source`, `age_seconds`), e só recusa (503 explícito) quando não há nada a mostrar. Protegi a **disponibilidade** (60 % → 100 % de sucesso na carga padrão) e o **bolso** (≈ R$ 82 mil/mês de economia no caso base), e isso custou: complexidade na resposta (o cliente agora precisa tratar `degraded`), um breaker que vive na memória de cada réplica, e uma agregação em paralelo que concentra concorrência nas parceiras — medi isso (13 estouros de timeout na `partner-degrading` na partida a frio) e o documentei como limite, não o escondi. A obrigação de isolamento é inegociável e fica na chave: sem `tenant_id` a corretora A receberia o prêmio da B.

## Como subir e reproduzir

```bash
make down && make reproduce     # carga padrão: 10 de baseline + 200 com 50 em voo
make test                       # suíte inteira
make smoke                      # os limiares do breaker abrem com os defaults (sem Docker)
```

Parâmetros (todos em `docker-compose.yml`, serviço `quotation-api`, lidos em `internal/platform/config.go`, defendidos em `docs/sad.md` §4): `PARTNER_TIMEOUT_MS=2000`, `BREAKER_CONSECUTIVE_FAILURES=5`, `BREAKER_OPEN_SECONDS=10`, `BREAKER_HALF_OPEN_MAX=2`, `CACHE_TTL_SECONDS=300`, `CACHE_STALE_SECONDS=3600`, `REDIS_ADDR=redis:6379`.

### Limitações do meu ambiente (documentadas, não contornadas em silêncio)

- **Portas ocupadas:** `9001`, `9002` e `6379` já estavam em uso por outros contêineres na minha máquina. Rodei com uma **cópia local** do compose (`docker-compose.local.yml`, ignorada pelo git) que só troca as portas **publicadas no host** (`19001`, `19002`, `16379`). A rede interna, as variáveis e os perfis das parceiras são idênticos ao `docker-compose.yml` versionado, que **não** foi alterado nesse ponto.
- **Docker Compose 2.6.1** (antigo): o `docker compose run` não aceita `--build`, então o `make load` falha aqui. Rodei `docker compose build loadgen` e depois `docker compose run --rm loadgen`, que é o mesmo comando sem a flag. O `Makefile` não foi alterado.
- **Telemetria de outra aplicação:** o Collector publica `:4317` no host e **outra aplicação minha (`order-api`, .NET)** também enviava telemetria para ele, então a métrica genérica `http_server_request_duration_seconds` do Prometheus mistura serviços. As consultas do "depois" filtram `{service_name="quotation-api"}`; a do "antes" **não** filtra (o ambiente já tinha sido derrubado quando percebi). O efeito é desprezível: o outro serviço somava 20 requisições contra 210 nossas, e o p95 do "antes" no gráfico (≈ 9,7 s, interpolação do bucket) segue o p95 real do relatório (8,00 s). As métricas `quotation_*` são só nossas.
- **Go:** `go.mod` continua em `go 1.25.0`; fixei `go-redis v9.7.3` e `gobreaker/v2 v2.1.0` porque as versões mais novas exigiam Go 1.26.

## O que foi implementado e o que ficou como proposta

| Implementado e provado | Proposta (no SAD, **não** construída) |
|---|---|
| Timeout 2 s que conta como falha — `internal/partner/client.go` | Auditoria WORM por cotação apresentada (§4.5, §8.2) |
| Circuit breaker **por parceira**, três estados nomeados, aberto não chama a parceira — `internal/partner/breaker.go` (`gobreaker/v2`) | Alertas A-1 a A-5 e Alertmanager (§6.1; as consultas foram executadas no Prometheus) |
| Cache Redis por (corretora, parceira, risco), erro do Redis vira miss — `internal/cache/cache.go`, `internal/quotation/request.go` | DR em segunda região (§7) |
| Fallback: parcial + cotação vencida marcada + recusa 503 — `internal/quotation/service.go`, `handler.go` | Invalidação explícita do cache (L-3), bulkhead e coalescência de requisições (L-1), breaker compartilhado entre réplicas (L-2), cota por corretora (L-4) |
| Agregação em paralelo (bônus) — `WithParallel` | Hospedagem em nuvem com região no Brasil (§5.2) |
| Métricas de negócio e marcas de trace — `internal/platform/metrics.go` | |

Sem nenhuma `Option`, `quotation.NewService` continua sendo o serviço original (série, tudo-ou-nada): é isso que mantém verdes os testes que já vinham no repositório. O `main.go` liga paralelo, cache e fallback.

## Contrato de resposta (o que mudou)

```jsonc
{
  "tenant_id": "corretora-a",
  "quotes": [{
    "partner": "partner-flaky", "quote_id": "…", "premium_cents": 147172, "currency": "BRL",
    "coverage_cents": 9000000, "valid_for_seconds": 300,
    "source": "partner" | "cache" | "stale_cache",   // novo: de onde veio o prêmio
    "age_seconds": 1                                  // novo (omitido se 0): idade da consulta original
  }],
  "degraded": false,                    // novo: há parceira ausente OU cotação stale_cache
  "missing_partners": ["partner-flaky"],// novo (omitido se vazio)
  "elapsed_ms": 1683
}
```

Cabeçalho `X-Quote-Degraded: true` quando `degraded`. Sem nenhuma cotação: **503** `{"error":"no partner quote available","degraded":true,"missing_partners":[…]}` com `Retry-After: 10`. As respostas 400/403/502 originais não mudaram; o 502 passou a existir só no modo legado (sem fallback). `source: "cache"` dentro do TTL não é degradação. O prêmio e o `quote_id` são sempre os da consulta original à parceira.

## A chave do cache, por extenso

```
quote:v1:{tenant_id}:{partner}:{RiskID}
tenant_id = cabeçalho X-Tenant-Id (nunca o corpo)
RiskID    = hex(16 primeiros bytes de SHA-256(JSON [document, birth_year, plate, lower(model), year, value_cents, coverage]))
```

CPF e placa **não** aparecem na chave nem no valor guardado (`evidencias/redis-chaves-e-ttl.txt`). Política: TTL fresco 300 s; `stale` 3.600 s só com a parceira indisponível; a entrada some sozinha aos 3.900 s (`SET … EX`); a configuração recusa `TTL + stale > 86.400 s` (teto comercial de 24 h); invalidação só por TTL (limite L-3 do SAD).

## Métricas e atributos criados

| Métrica (no Prometheus) | Atributos | Onde nasce |
|---|---|---|
| `quotation_breaker_state` (gauge: 0 closed · 1 half_open · 2 open) | `partner` | `internal/platform/metrics.go` |
| `quotation_breaker_transitions_total` | `partner`, `from`, `to` | `internal/partner/breaker.go` |
| `quotation_cache_requests_total` | `tenant_id`, `result` (`hit`/`miss`) | `internal/quotation/service.go` |
| `quotation_partner_duration_seconds` (histograma) | `partner`, `outcome` (`ok`/`error`/`timeout`) | `internal/partner/client.go` |
| `quotation_fallback_responses_total` | `kind` (`stale_cache`/`partial`/`refused`) | `internal/quotation/service.go` |

**Spans** (nome `partner.quote`, um por parceira por requisição): `partner`, `tenant_id`, `cache.result` (`hit`/`miss`), `breaker.short_circuited` (a requisição curto-circuitada), `partner.timeout`, `cache.stale_served`. No span `POST /quotes`: `quote.degraded`, `quote.served_from_cache`, `quote.missing_partners`, `tenant_id`. **Nenhum** atributo carrega CPF, placa ou `quote_id`. A latência por parceira é métrica de domínio própria; **não** reaproveitei `http_client_request_duration_seconds`.

Hit rate: `sum(rate(quotation_cache_requests_total{result="hit"}[5m])) / sum(rate(quotation_cache_requests_total[5m]))`.

## Evidências

Toda evidência tem o comando ou a consulta, o artefato e uma legenda. Gráficos foram desenhados por [`docs/evidencias/capturar-grafico.ps1`](docs/evidencias/capturar-grafico.ps1) a partir da resposta crua do Prometheus (o `.json` ao lado), durante a execução; a consulta e a janela estão no `.txt`.

| # | Evidência | Artefato | Comando / consulta (janela) | Legenda |
|---|---|---|---|---|
| 1 | Relatório antes/depois | [`antes-reproduce.txt`](docs/evidencias/antes-reproduce.txt) · [`depois-reproduce.txt`](docs/evidencias/depois-reproduce.txt) | `make down && make reproduce`; no depois, mesma carga | carga: p95 **8,00 s → 2,11 s**, sucesso **60 % → 100 %**, vazão **8,0 → 94,1 req/s** |
| 2 | `make test` | [`make-test.txt`](docs/evidencias/make-test.txt) · [`make-test-verbose.txt`](docs/evidencias/make-test-verbose.txt) | `make test` | suíte inteira verde: 75 `PASS`, 0 `FAIL`, 0 `SKIP`; os testes originais continuam passando |
| 3 | Trace com breaker aberto | [`trace-breaker-aberto-curto-circuito.json`](docs/evidencias/trace-breaker-aberto-curto-circuito.json) | export do Jaeger, tag `breaker.short_circuited=true` (Last Hour) | a `partner-flaky` curto-circuitada: **0 spans `HTTP POST`**, resposta em 169 µs |
| 4 | Trace servido de cache | [`trace-servido-de-cache.json`](docs/evidencias/trace-servido-de-cache.json) | export do Jaeger, tag `quote.served_from_cache=true` (Last Hour) | três `partner.quote` com `cache.result=hit`, **sem** spans de saída, 1,4 ms |
| 5 | Estado do breaker no tempo | [`breaker-estado-no-tempo.png`](docs/evidencias/breaker-estado-no-tempo.png) | `quotation_breaker_state` (900 s) | `partner-degrading` e `partner-flaky`: 0 → 2 → 1 → 0 (fechado → aberto → meio aberto → fechado) |
| 6 | Hit rate do cache | [`cache-hit-rate.png`](docs/evidencias/cache-hit-rate.png) | `sum(rate(quotation_cache_requests_total{tenant_id="corretora-b",result="hit"}[30s])) / sum(rate(quotation_cache_requests_total{tenant_id="corretora-b"}[30s]))` (150 s) | a curva sobe de 0 a ≈ 95 % enquanto o cache aquece. É propriedade da carga (15 riscos recotados ao acaso), **não** o hit da planilha (SAD §8.1) |
| 7 | p95 do `POST /quotes` | [`antes-p95.png`](docs/evidencias/antes-p95.png) · [`depois-p95.png`](docs/evidencias/depois-p95.png) | `histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_seconds_bucket[1m])))` (300 s, mesma escala 0–12 s; o depois com `{service_name="quotation-api"}`) | antes ≈ 9,7 s (interpolação do bucket; 8,00 s no relatório) · depois ≈ 2,0 s |

Evidências de apoio: [`depois-reproduce-execucao-anterior.txt`](docs/evidencias/depois-reproduce-execucao-anterior.txt) (a mesma carga numa execução anterior do código: p95 **265 ms**), [`make-smoke.txt`](docs/evidencias/make-smoke.txt), [`exemplo-resposta-contrato.txt`](docs/evidencias/exemplo-resposta-contrato.txt), [`redis-chaves-e-ttl.txt`](docs/evidencias/redis-chaves-e-ttl.txt), as sondas de recuperação e as variações de carga declaradas (`variacao-*.txt`, `sonda-*.txt`, `recotacao-aleatoria-pool15-corretora-b.txt`).

**Leitura honesta dos números.** O "depois" da carga padrão **não é um número só**: a mesma carga deu p95 de 265 ms numa execução e 2,11 s na outra, porque na partida a frio os 50 primeiros pedidos chegam antes de o primeiro gravar no cache e mandam 50 chamadas simultâneas à `partner-degrading` (13 estouros do timeout de 2 s, breaker dela abre). Isso é o limite L-1 do SAD (sem bulkhead nem coalescência), medido, e é por isso que o alvo do RNF-01 é 2,5 s (o timeout mais uma margem), não 300 ms. O hit rate de **84 %** da carga padrão é propriedade da carga (5 riscos repetidos) e não entra na conta da §8. O hit rate da planilha (26 %) vem do pressuposto de recotação P-01, que **não foi medido**.

**Variações declaradas** (o enunciado permite explorar o limiar; a evidência de sustentação vem dos defaults e `make smoke` segue verde): `loadgen -baseline 0 -concurrency 4 -requests 300 -distinct 100` (breaker completa o ciclo e recupera), `loadgen -baseline 0 -concurrency 2 -requests 400 -distinct 40 -tenant corretora-b` (isolamento entre corretoras), e dois laços de `Invoke-WebRequest` (um por 1,5 s por 100 s, e recotação ao acaso de 15 riscos por 120 s).

## O que eu faria diferente com mais tempo

1. **Coalescência de requisições idênticas** (`singleflight` por chave de cache): é o que resolve a partida a frio medida (L-1) e o cenário "perdi o Redis sob rajada" (SAD §7a). É a primeira coisa.
2. **Bulkhead por parceira** (limite de chamadas simultâneas): o único jeito de proteger a `partner-degrading` de nós mesmos; o breaker só a protege depois do dano.
3. **Estado do breaker compartilhado** (ou ao menos um piso de requisições por réplica) para que 2 réplicas não aprendam a mesma falha 2 vezes (L-2).
4. **Medir a taxa de recotação em produção** antes de contratar o Redis definitivo: P-01 sustenta toda a §8 e é a minha premissa mais frágil.
5. **Persistir a auditoria** de verdade (§4.5) e deixar a resposta depender dela.
6. **Regras de alerta versionadas** (`deploy/prometheus/`) em vez de só descritas no SAD.
