CREATE TABLE IF NOT EXISTS canvases (
  id TEXT PRIMARY KEY,
  grid_size INTEGER NOT NULL CHECK (grid_size > 0),
  cells JSONB NOT NULL CHECK (jsonb_typeof(cells) = 'array'),
  version BIGINT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO canvases (id, grid_size, cells, version)
VALUES (
  'global',
  32,
  (SELECT jsonb_agg('#FFFFFF'::text) FROM generate_series(1, 1024)),
  0
)
ON CONFLICT (id) DO NOTHING;
