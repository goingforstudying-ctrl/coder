package aibridgedserver

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"

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
// interception and computes its cost. It is best-effort: any failure leaves the
// affected columns NULL rather than dropping the token usage record. A model
// missing from the price table is expected and yields a NULL cost.
func (s *Server) resolveTokenUsageCost(ctx context.Context, intcID uuid.UUID, in *proto.RecordTokenUsageRequest) tokenUsageCost {
	var result tokenUsageCost

	intc, err := s.store.GetAIBridgeInterceptionByID(ctx, intcID)
	if err != nil {
		s.logger.Warn(ctx, "failed to load interception for cost attribution, recording token usage without cost",
			slog.F("interception_id", intcID.String()), slog.Error(err))
		return result
	}

	// Resolve the effective group for attribution. This is independent of
	// whether the model is priced.
	eb, ok, err := budget.ResolveUserAIBudget(ctx, s.store, intc.InitiatorID, s.budgetPolicy)
	switch {
	case err != nil:
		s.logger.Warn(ctx, "failed to resolve effective AI budget, recording token usage without group attribution",
			slog.F("interception_id", intcID.String()),
			slog.F("initiator_id", intc.InitiatorID.String()),
			slog.Error(err))
	case ok:
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
		return result
	case err != nil:
		s.logger.Warn(ctx, "failed to look up model price, recording token usage without cost",
			slog.F("provider", intc.Provider), slog.F("model", intc.Model), slog.Error(err))
		return result
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
	return result
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
