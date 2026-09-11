-- GPU load alongside CPU/memory in the metrics chart.
ALTER TABLE system_metrics
    ADD COLUMN IF NOT EXISTS gpu_percent         REAL,
    ADD COLUMN IF NOT EXISTS gpu_encoder_percent REAL,
    ADD COLUMN IF NOT EXISTS gpu_memory_percent  REAL;
