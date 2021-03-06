DROP PROCEDURE IF EXISTS SCHEMA_CATALOG.drop_metric_chunks(TEXT, TIMESTAMPTZ, TIMESTAMPTZ);
DROP PROCEDURE IF EXISTS SCHEMA_CATALOG.execute_data_retention_policy();
DROP PROCEDURE IF EXISTS SCHEMA_PROM.execute_maintenance();
DROP FUNCTION IF EXISTS SCHEMA_PROM.config_maintenance_jobs(int, interval);
DROP PROCEDURE IF EXISTS SCHEMA_CATALOG.execute_compression_policy();