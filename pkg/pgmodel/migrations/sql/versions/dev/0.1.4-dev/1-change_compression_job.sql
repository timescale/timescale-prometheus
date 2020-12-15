ALTER TABLE SCHEMA_CATALOG.metric
    ADD COLUMN delay_compression_until TIMESTAMPTZ DEFAULT NULL;

    DROP PROCEDURE IF EXISTS SCHEMA_CATALOG.decompress_chunks_after(NAME, TIMESTAMPTZ);
    DROP PROCEDURE IF EXISTS SCHEMA_CATALOG.do_decompress_chunks_after(NAME, TIMESTAMPTZ);
    DROP PROCEDURE IF EXISTS SCHEMA_CATALOG.compression_job(INT, JSONB);
