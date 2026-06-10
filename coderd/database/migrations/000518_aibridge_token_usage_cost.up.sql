ALTER TABLE aibridge_token_usages
    -- Group whose budget this interception counts against. NULL if the user
    -- has no effective group (no budget configured). SET NULL on group delete
    -- preserves the immutable spend record.
    ADD COLUMN effective_group_id UUID REFERENCES groups(id) ON DELETE SET NULL,
    -- Snapshotted prices at interception time, in micro-units per million
    -- tokens. Denormalized from ai_model_prices to guarantee historical
    -- accuracy regardless of future price table updates.
    ADD COLUMN input_price        BIGINT CHECK (input_price >= 0),
    ADD COLUMN output_price       BIGINT CHECK (output_price >= 0),
    ADD COLUMN cache_read_price   BIGINT CHECK (cache_read_price >= 0),
    ADD COLUMN cache_write_price  BIGINT CHECK (cache_write_price >= 0),
    -- Computed cost in micro-units at interception time. NULL if the model is
    -- not present in ai_model_prices.
    ADD COLUMN cost               BIGINT CHECK (cost >= 0);
