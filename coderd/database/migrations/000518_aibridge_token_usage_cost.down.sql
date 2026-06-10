ALTER TABLE aibridge_token_usages
    DROP COLUMN effective_group_id,
    DROP COLUMN input_price,
    DROP COLUMN output_price,
    DROP COLUMN cache_read_price,
    DROP COLUMN cache_write_price,
    DROP COLUMN cost;
