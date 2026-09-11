-- GPU load alongside CPU/memory in the metrics chart.
ALTER TABLE system_metrics ADD COLUMN gpu_percent REAL;
ALTER TABLE system_metrics ADD COLUMN gpu_encoder_percent REAL;
ALTER TABLE system_metrics ADD COLUMN gpu_memory_percent REAL;
