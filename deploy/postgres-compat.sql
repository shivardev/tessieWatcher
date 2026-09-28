-- SQLite date/format compatibility used by the existing teslalog dashboards.
-- This is transitional: new API endpoints should eventually own dashboard SQL.
CREATE OR REPLACE FUNCTION julianday(value text) RETURNS double precision
LANGUAGE SQL STABLE PARALLEL SAFE AS $$
  SELECT EXTRACT(EPOCH FROM value::timestamptz) / 86400.0 + 2440587.5
$$;

CREATE OR REPLACE FUNCTION datetime(base text, modifier text DEFAULT NULL) RETURNS text
LANGUAGE plpgsql STABLE AS $$
DECLARE result timestamptz;
BEGIN
  result := CASE WHEN lower(base) = 'now' THEN CURRENT_TIMESTAMP ELSE base::timestamptz END;
  IF modifier IS NOT NULL AND lower(modifier) <> 'localtime' THEN
    result := result + modifier::interval;
  END IF;
  RETURN to_char(result AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"');
END $$;

CREATE OR REPLACE FUNCTION strftime(fmt text, value text, modifier text DEFAULT NULL) RETURNS text
LANGUAGE plpgsql STABLE AS $$
DECLARE ts timestamptz;
BEGIN
  ts := CASE WHEN lower(value) = 'now' THEN CURRENT_TIMESTAMP ELSE value::timestamptz END;
  IF modifier IS NOT NULL AND lower(modifier) = 'localtime' THEN ts := ts AT TIME ZONE current_setting('TIMEZONE'); END IF;
  IF fmt = '%s' THEN RETURN floor(EXTRACT(EPOCH FROM ts))::bigint::text; END IF;
  RETURN to_char(ts AT TIME ZONE 'UTC', CASE fmt
    WHEN '%Y-%m-%dT%H:00:00Z' THEN 'YYYY-MM-DD"T"HH24:00:00"Z"'
    WHEN '%Y-%m-%d' THEN 'YYYY-MM-DD'
    WHEN '%Y-W%W' THEN 'IYYY-"W"IW'
    WHEN '%Y' THEN 'YYYY'
    WHEN '%Y-%m' THEN 'YYYY-MM'
    WHEN '%H:%M' THEN 'HH24:MI'
    ELSE 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"' END);
END $$;

CREATE OR REPLACE FUNCTION printf(fmt text, a double precision, b double precision) RETURNS text
LANGUAGE SQL IMMUTABLE PARALLEL SAFE AS $$ SELECT to_char(a, 'FM999990.0000') || ', ' || to_char(b, 'FM999990.0000') $$;
CREATE OR REPLACE FUNCTION round(value double precision, digits integer) RETURNS numeric
LANGUAGE SQL IMMUTABLE PARALLEL SAFE AS $$ SELECT pg_catalog.round(value::numeric, digits) $$;
CREATE OR REPLACE FUNCTION max(a double precision, b double precision) RETURNS double precision
LANGUAGE SQL IMMUTABLE PARALLEL SAFE AS $$ SELECT greatest(a,b) $$;
CREATE OR REPLACE FUNCTION max(a bigint, b bigint) RETURNS bigint
LANGUAGE SQL IMMUTABLE PARALLEL SAFE AS $$ SELECT greatest(a,b) $$;
CREATE OR REPLACE FUNCTION min(a double precision, b double precision) RETURNS double precision
LANGUAGE SQL IMMUTABLE PARALLEL SAFE AS $$ SELECT least(a,b) $$;
CREATE OR REPLACE FUNCTION min(a bigint, b bigint) RETURNS bigint
LANGUAGE SQL IMMUTABLE PARALLEL SAFE AS $$ SELECT least(a,b) $$;
