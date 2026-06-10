package aibridgedserver

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/aibridge/budget"
	"github.com/coder/coder/v2/coderd/aibridged/proto"
	"github.com/coder/coder/v2/coderd/database"
)

// microUnitsPerMillion is the divisor for the per-token price columns, which
// are stored in micro-units per million tokens (1 unit = 1,000,000 micro-units).
const microUnitsPerMillion = 1_000_000

// tokenUsageCost holds the cost-attribution columns snapshotted onto a token
// usage record. Zero-value fields are NULL when recorded.
type tokenUsageCost struct {
	effectiveGroupID uuid.NullUUID
	inputPrice       sql.NullInt64
	outputPrice      sql.NullInt64
	cacheReadPrice   sql.NullInt64
	cacheWritePrice  sql.NullInt64
	cost             sql.NullInt64
}

// resolveTokenUsageCost resolves the effective group and per-token prices for an
// interception and computes its cost. Two outcomes are expected and yield NULL
// columns rather than an error: a user with no configured budget (NULL group)
// and a model absent from the price table (NULL prices and cost). Any other
// error is returned so the caller can fail the record; a NULL cost then
// unambiguously means "model not priced" rather than "lookup failed".
func (s *Server) resolveTokenUsageCost(ctx context.Context, intc database.AIBridgeInterception, in *proto.RecordTokenUsageRequest) (tokenUsageCost, error) {
	var result tokenUsageCost

	// Resolve the effective group for attribution. This is independent of
	// whether the model is priced. ok is false when no budget is configured,
	// which leaves the group attribution NULL.
	eb, ok, err := budget.ResolveUserAIBudget(ctx, s.store, intc.InitiatorID, s.budgetPolicy)
	if err != nil {
		return tokenUsageCost{}, xerrors.Errorf("resolve effective AI budget for user %q: %w", intc.InitiatorID, err)
	}
	if ok {
		result.effectiveGroupID = uuid.NullUUID{UUID: eb.GroupID, Valid: true}
	}

	// Snapshot the price for this (provider, model) and compute cost.
	price, err := s.store.GetAIModelPriceByProviderModel(ctx, database.GetAIModelPriceByProviderModelParams{
		Provider: intc.Provider,
		Model:    intc.Model,
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Model not in the price table: record tokens but leave cost NULL.
		s.logger.Debug(ctx, "no price found for model, recording token usage with NULL cost",
			slog.F("provider", intc.Provider), slog.F("model", intc.Model))
		return result, nil
	case err != nil:
		return tokenUsageCost{}, xerrors.Errorf("look up model price for %s/%s: %w", intc.Provider, intc.Model, err)
	}

	result.inputPrice = price.InputPrice
	result.outputPrice = price.OutputPrice
	result.cacheReadPrice = price.CacheReadPrice
	result.cacheWritePrice = price.CacheWritePrice
	result.cost = sql.NullInt64{
		Int64: computeCost(price,
			in.GetInputTokens(), in.GetOutputTokens(),
			in.GetCacheReadInputTokens(), in.GetCacheWriteInputTokens()),
		Valid: true,
	}
	return result, nil
}

// computeCost returns the cost of an interception in micro-units, snapshotting
// the per-token prices from the price row. Prices are expressed per million
// tokens; a NULL price column is treated as zero (e.g. providers that do not
// charge for cache writes). Per-term integer division mirrors the cost formula
// in the AI Governance RFC and avoids floating-point precision drift.
func computeCost(price database.AiModelPrice, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64) int64 {
	return tokenCost(inputTokens, price.InputPrice) +
		tokenCost(outputTokens, price.OutputPrice) +
		tokenCost(cacheReadTokens, price.CacheReadPrice) +
		tokenCost(cacheWriteTokens, price.CacheWritePrice)
}

// tokenCost returns tokens * price / 1,000,000, treating a NULL price as zero.
func tokenCost(tokens int64, pricePerMillion sql.NullInt64) int64 {
	if !pricePerMillion.Valid {
		return 0
	}
	return tokens * pricePerMillion.Int64 / microUnitsPerMillion
}
