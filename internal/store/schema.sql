CREATE TABLE IF NOT EXISTS house (id INTEGER PRIMARY KEY, name TEXT, faction TEXT);

CREATE TABLE IF NOT EXISTS snapshot (
  id INTEGER PRIMARY KEY,
  house_id INT,
  kind TEXT CHECK (kind IN ('gear','commodity')),
  fetched_at INT,
  last_modified INT,
  n_auctions INT,
  body_sha256 TEXT,
  UNIQUE (house_id, kind, body_sha256));
CREATE INDEX IF NOT EXISTS snapshot_house_time ON snapshot (house_id, kind, fetched_at);

CREATE TABLE IF NOT EXISTS item (
  id INTEGER PRIMARY KEY, name TEXT, quality INT, class TEXT, subclass TEXT, stackable INT,
  sell_price INT, item_level INT, required_level INT, icon TEXT, looked_up_at INT);

-- one row per auction for its whole life. kind is part of the key because gear and
-- commodity auction IDs are not guaranteed to share an ID space.
CREATE TABLE IF NOT EXISTS auction (
  house_id INT, kind TEXT, auction_id INT, item_id INT, variant TEXT DEFAULT '',
  unit_price INT, buyout INT, bid INT,
  qty_first INT, qty_last INT, first_seen INT, last_seen INT,
  tl_first TEXT, tl_last TEXT, closed_at INT, outcome TEXT,
  PRIMARY KEY (house_id, kind, auction_id));
CREATE INDEX IF NOT EXISTS auction_open ON auction (house_id, kind) WHERE closed_at IS NULL;
CREATE INDEX IF NOT EXISTS auction_item ON auction (house_id, item_id, closed_at);
CREATE INDEX IF NOT EXISTS auction_closed ON auction (house_id, closed_at) WHERE closed_at IS NOT NULL;

-- partial commodity buys (rule 1). Closures are not duplicated here: an auction's outcome,
-- closed_at and qty_last on the auction row are the closure event.
CREATE TABLE IF NOT EXISTS sale_event (
  house_id INT, kind TEXT, auction_id INT, item_id INT, variant TEXT,
  ts INT, outcome TEXT, units INT, unit_price INT);
CREATE INDEX IF NOT EXISTS sale_event_time ON sale_event (house_id, ts);

CREATE TABLE IF NOT EXISTS item_snapshot (
  snapshot_id INT, item_id INT, variant TEXT, listed_qty INT, n_auctions INT,
  min_unit INT, p20_unit INT, median_unit INT,
  PRIMARY KEY (snapshot_id, item_id, variant));
CREATE INDEX IF NOT EXISTS item_snapshot_item ON item_snapshot (item_id, variant);

CREATE TABLE IF NOT EXISTS item_daily (
  house_id INT, item_id INT, variant TEXT, day TEXT,
  fair_unit INT, min_unit INT, avg_listed INT,
  sold_est INT, sold_lo INT, sold_hi INT, ended INT, price_cv REAL,
  sales INT DEFAULT 0, largest_sale INT DEFAULT 0, -- separate certain sales, and the biggest one
  PRIMARY KEY (house_id, item_id, variant, day));
CREATE INDEX IF NOT EXISTS item_daily_day ON item_daily (house_id, day);

-- flip inputs, recomputed after each publish (single row)
CREATE TABLE IF NOT EXISTS flip_cache (id INTEGER PRIMARY KEY CHECK (id = 1), computed_at INT, data BLOB);

-- one-off markers (e.g. completed backfills)
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);
