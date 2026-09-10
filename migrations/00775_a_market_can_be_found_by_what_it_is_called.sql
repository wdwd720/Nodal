-- +goose Up
-- Discovery indexes for the markets page (product goal §12 MARKETS PAGE, §35 SEARCH / DISCOVERY).
--
-- §35 asks for search "backed by authoritative API", not for a search cluster. A native economy's
-- asset list is bounded by how many assets people have created, and the queries the markets page
-- runs are: filter by status, order by a summary figure, and match a short string against a name, a
-- symbol or a description. PostgreSQL answers all three; adding an external index would put a second
-- copy of the catalogue somewhere it could go stale, for a corpus that fits in a page of results.
--
-- Two indexes, and a note on why not a third:
--
--   * a GIN index over a `simple`-configuration tsvector of name, symbol and description, which is
--     what a word query uses. The `simple` dictionary rather than `english` on purpose: a native
--     asset's name is usually not English, and stemming "COINS" to "coin" across an invented ticker
--     produces matches nobody asked for.
--   * a btree on upper(symbol), for the exact and prefix ticker lookup, which is how somebody who
--     already knows the symbol searches.
--
--   * NOT pg_trgm. A trigram index would make an unanchored `%foo%` fast, but it is an extension,
--     and CREATE EXTENSION needs a privilege the migration role should not have to hold on a managed
--     Postgres. The list query anchors its ILIKE to a prefix, which the btree above serves, and
--     falls back to the tsvector for words inside a description.

CREATE INDEX native_assets_search_idx ON native_assets
    USING gin (to_tsvector('simple', name || ' ' || symbol || ' ' || description));
CREATE INDEX native_assets_symbol_prefix_idx ON native_assets (upper(symbol) text_pattern_ops);

-- Ordering a market list by recency without reading every row.
CREATE INDEX native_markets_created_idx ON native_markets (created_at DESC, id DESC);

-- +goose Down
SELECT 1; -- protected: this migration is at or above the protected version, and every Down there is a
-- no-op by rule. Nothing here holds a fact -- these are indexes -- but the rule is about the FILE, not
-- about what is in it: a rollback that can run statements past this line is a rollback that could one
-- day run one that destroys accounting history. An operator who genuinely wants these indexes gone
-- writes a new migration that says so.
