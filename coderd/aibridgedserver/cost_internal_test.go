package aibridgedserver

import (
	"database/sql"
	"testing"

	"github.com/coder/coder/v2/coderd/database"
)

func TestComputeCost(t *testing.T) {
	t.Parallel()

	micros := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

	tests := []struct {
		name                                                         string
		price                                                        database.AiModelPrice
		inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64
		want                                                         int64
	}{
		{
			name: "all priced",
			price: database.AiModelPrice{
				InputPrice:      micros(3_000_000),
				OutputPrice:     micros(6_000_000),
				CacheReadPrice:  micros(300_000),
				CacheWritePrice: micros(3_750_000),
			},
			inputTokens:      100,
			outputTokens:     200,
			cacheReadTokens:  50,
			cacheWriteTokens: 10,
			// 300 + 1200 + 15 + 37 (10*3_750_000/1e6 = 37, integer division).
			want: 1552,
		},
		{
			name: "null cache write price treated as zero",
			price: database.AiModelPrice{
				InputPrice:      micros(3_000_000),
				OutputPrice:     micros(6_000_000),
				CacheReadPrice:  micros(300_000),
				CacheWritePrice: sql.NullInt64{Valid: false},
			},
			inputTokens:      100,
			outputTokens:     200,
			cacheReadTokens:  50,
			cacheWriteTokens: 10,
			// 300 + 1200 + 15 + 0.
			want: 1515,
		},
		{
			name:             "all prices null is zero cost",
			price:            database.AiModelPrice{},
			inputTokens:      100,
			outputTokens:     200,
			cacheReadTokens:  50,
			cacheWriteTokens: 10,
			want:             0,
		},
		{
			name: "zero tokens is zero cost",
			price: database.AiModelPrice{
				InputPrice:  micros(3_000_000),
				OutputPrice: micros(6_000_000),
			},
			want: 0,
		},
		{
			name: "integer division truncates",
			price: database.AiModelPrice{
				// 1 token at 1 micro-unit per million tokens rounds down to 0.
				InputPrice: micros(1),
			},
			inputTokens: 1,
			want:        0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := computeCost(tt.price, tt.inputTokens, tt.outputTokens, tt.cacheReadTokens, tt.cacheWriteTokens)
			if got != tt.want {
				t.Fatalf("computeCost = %d, want %d", got, tt.want)
			}
		})
	}
}
