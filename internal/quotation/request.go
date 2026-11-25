package quotation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
)

type Request struct {
	Driver   Driver  `json:"driver"`
	Vehicle  Vehicle `json:"vehicle"`
	Coverage string  `json:"coverage"`
}

type Driver struct {
	Document  string `json:"document"`
	BirthYear int    `json:"birth_year"`
}

type Vehicle struct {
	Plate      string `json:"plate"`
	Model      string `json:"model"`
	Year       int    `json:"year"`
	ValueCents int64  `json:"value_cents"`
}

var coverages = map[string]bool{"comprehensive": true, "third_party": true}

const defaultCoverage = "comprehensive"

func (r *Request) Normalize() error {
	r.Coverage = strings.TrimSpace(r.Coverage)
	if r.Coverage == "" {
		r.Coverage = defaultCoverage
	}

	r.Driver.Document = strings.TrimSpace(r.Driver.Document)
	r.Vehicle.Plate = strings.ToUpper(strings.TrimSpace(r.Vehicle.Plate))
	r.Vehicle.Model = strings.TrimSpace(r.Vehicle.Model)

	switch {
	case r.Driver.Document == "":
		return fmt.Errorf("driver.document is required")
	case r.Driver.BirthYear < 1900:
		return fmt.Errorf("driver.birth_year is required and must be a valid year")
	case r.Vehicle.Plate == "":
		return fmt.Errorf("vehicle.plate is required")
	case r.Vehicle.Year < 1900:
		return fmt.Errorf("vehicle.year is required and must be a valid year")
	case r.Vehicle.ValueCents <= 0:
		return fmt.Errorf("vehicle.value_cents must be greater than zero")
	case !coverages[r.Coverage]:
		return fmt.Errorf("coverage must be comprehensive or third_party")
	}
	return nil
}

// RiskID identifica o risco cotado: hash SHA-256 (32 hex) dos campos normalizados que mudam o
// prêmio (documento, ano de nascimento, placa, modelo, ano, valor e cobertura). O hash mantém
// CPF e placa fora das chaves do Redis (minimização, LGPD); o modelo entra sem caixa para que
// "Onix 1.0" e "onix 1.0" sejam o mesmo risco. Chamar só depois de Normalize.
func (r Request) RiskID() string {
	canonical, _ := json.Marshal([]any{
		r.Driver.Document, r.Driver.BirthYear,
		r.Vehicle.Plate, strings.ToLower(r.Vehicle.Model), r.Vehicle.Year, r.Vehicle.ValueCents,
		r.Coverage,
	})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:16])
}

// CacheKey é a chave do cache, por extenso:
//
//	quote:v1:{tenant_id}:{partner}:{RiskID}
//
// A corretora vem do cabeçalho X-Tenant-Id e NUNCA do corpo; sem ela na chave, a corretora A
// receberia o prêmio negociado pela B. Mudou o formato? Suba o v1.
func (r Request) CacheKey(tenant, partnerName string) string {
	return "quote:v1:" + tenant + ":" + partnerName + ":" + r.RiskID()
}

type partnerRequest struct {
	Broker string `json:"broker"`
	Request
}

// Response é o contrato de resposta. Além do original, `degraded` e `missing_partners` dizem à
// corretora que a cotação está incompleta ou vencida, e cada quote traz `source` (e `age_seconds`
// quando não veio de uma consulta feita agora). Ver README.md, "Contrato de resposta".
type Response struct {
	TenantID        string          `json:"tenant_id"`
	Quotes          []partner.Quote `json:"quotes"`
	Degraded        bool            `json:"degraded"`
	MissingPartners []string        `json:"missing_partners,omitempty"`
	ElapsedMs       int64           `json:"elapsed_ms"`
}
