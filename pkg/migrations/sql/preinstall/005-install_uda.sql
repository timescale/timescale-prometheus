CREATE OR REPLACE FUNCTION SCHEMA_CATALOG.get_timescale_major_version()
    RETURNS INT
AS $func$
    SELECT split_part(extversion, '.', 1)::INT FROM pg_catalog.pg_extension WHERE extname='timescaledb' LIMIT 1;
$func$
LANGUAGE SQL STABLE PARALLEL SAFE;

--just a stub will be replaced in the idempotent scripts
CREATE OR REPLACE PROCEDURE SCHEMA_CATALOG.execute_maintenance_job(job_id int, config jsonb)
AS $$
BEGIN
    RAISE 'calling execute_maintenance_job stub, should have been replaced';
END
$$ LANGUAGE PLPGSQL;

CREATE OR REPLACE FUNCTION SCHEMA_CATALOG.is_timescaledb_installed()
RETURNS BOOLEAN
AS $func$
    SELECT count(*) > 0 FROM pg_extension WHERE extname='timescaledb';
$func$
LANGUAGE SQL STABLE;
GRANT EXECUTE ON FUNCTION SCHEMA_CATALOG.is_timescaledb_installed() TO prom_reader;

CREATE OR REPLACE FUNCTION SCHEMA_CATALOG.is_timescaledb_oss()
RETURNS BOOLEAN AS
$$
BEGIN
    IF SCHEMA_CATALOG.is_timescaledb_installed() THEN
        IF SCHEMA_CATALOG.get_timescale_major_version() >= 2 THEN
            RETURN (SELECT current_setting('timescaledb.license') = 'apache');
        ELSE
            RETURN (SELECT current_setting('timescaledb.license_key') = 'ApacheOnly');
        END IF;
    END IF;
    RETURN false;
END;
$$
LANGUAGE plpgsql;
GRANT EXECUTE ON FUNCTION SCHEMA_CATALOG.is_timescaledb_oss() TO prom_reader;

--add 2 jobs executing every 30 min by default for timescaledb 2.0
DO $$
BEGIN
    IF NOT SCHEMA_CATALOG.is_timescaledb_oss() AND SCHEMA_CATALOG.get_timescale_major_version() >= 2 THEN
       PERFORM add_job('SCHEMA_CATALOG.execute_maintenance_job', '30 min');
       PERFORM add_job('SCHEMA_CATALOG.execute_maintenance_job', '30 min');
    END IF;
END
$$;
