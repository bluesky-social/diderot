package migrations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// schemaGoldenPath records the schema and rows the baseline migration builds
// on an empty database: exactly what the migration history it replaced
// built. The baseline never changes, so neither does this file; later
// migrations don't touch it. Regenerate with DIDEROT_UPDATE_GOLDEN=1 only if
// the baseline itself is meant to change.
const schemaGoldenPath = "testdata/schema.golden.txt"

func TestBaselineSchemaMatchesGolden(t *testing.T) {
	dbURL := emptyDatabase(t)
	conn, err := pgx.Connect(t.Context(), dbURL)
	require.NoError(t, err)
	_, err = conn.Exec(t.Context(), string(baselineSQL(t)))
	require.NoError(t, err)
	require.NoError(t, conn.Close(t.Context()))
	got := describeDatabase(t, dbURL)
	if os.Getenv("DIDEROT_UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.WriteFile(schemaGoldenPath, []byte(got), 0o644))
	}
	want, err := os.ReadFile(schemaGoldenPath)
	require.NoError(t, err)
	require.Equal(t, string(want), got)
}

// baselineVersion is the baseline's version: the version of the last
// migration of the history it replaced.
const baselineVersion = "20261120000001"

// baselineSQL returns the first migration, the baseline.
func baselineSQL(t *testing.T) []byte {
	t.Helper()
	entries, err := migrationsFS.ReadDir("sql")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(entries[0].Name(), baselineVersion+"_"), entries[0].Name())
	data, err := migrationsFS.ReadFile("sql/" + entries[0].Name())
	require.NoError(t, err)
	return data
}

// emptyDatabase creates a database with nothing in it on the cluster
// DIDEROT_TEST_DB_URL names, drops it when the test ends, and returns its URL.
func emptyDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("DIDEROT_TEST_DB_URL")
	if base == "" {
		t.Skip("DIDEROT_TEST_DB_URL not set; skipping DB integration test")
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "diderot_test_migrations_" + hex.EncodeToString(suffix)
	admin := withDatabase(t, base, "postgres")
	exec := func(ctx context.Context, sql string) error {
		conn, err := pgx.Connect(ctx, admin)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close(ctx) }()
		_, err = conn.Exec(ctx, sql)
		return err
	}
	require.NoError(t, exec(t.Context(), "CREATE DATABASE "+name))
	t.Cleanup(func() {
		if err := exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	return withDatabase(t, base, name)
}

func withDatabase(t *testing.T, base, name string) string {
	t.Helper()
	u, err := url.Parse(base)
	require.NoError(t, err)
	u.Path = "/" + name
	return u.String()
}

// describeDatabase renders every object in the database's public schema and
// every row of its tables as sorted text lines. It reads the catalogs, so it
// needs no pg_dump of the server's version.
func describeDatabase(t *testing.T, dbURL string) string {
	t.Helper()
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, dbURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	var out strings.Builder
	for _, section := range schemaQueries {
		fmt.Fprintf(&out, "-- %s\n", section.name)
		rows, err := conn.Query(ctx, section.query)
		require.NoError(t, err, section.name)
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err, section.name)
		for _, line := range lines {
			out.WriteString(line + "\n")
		}
	}

	// Rows, without timestamps: a fresh database's rows are the ones a new
	// install starts with, and their timestamps say only when it was built.
	out.WriteString("-- rows\n")
	rows, err := conn.Query(ctx, `
		SELECT c.relname,
		       coalesce(array_agg(a.attname::text ORDER BY a.attnum)
		                FILTER (WHERE a.atttypid = 'timestamptz'::regtype), '{}')
		  FROM pg_class c
		  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		 WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'p')
		 GROUP BY c.relname
		 ORDER BY c.relname`)
	require.NoError(t, err)
	type table struct {
		Name       string
		Timestamps []string
	}
	tables, err := pgx.CollectRows(rows, pgx.RowToStructByPos[table])
	require.NoError(t, err)
	for _, table := range tables {
		rows, err := conn.Query(ctx, fmt.Sprintf(
			`SELECT (to_jsonb(t) - $1::text[])::text FROM %s t ORDER BY 1`,
			pgx.Identifier{table.Name}.Sanitize()), table.Timestamps)
		require.NoError(t, err, table.Name)
		values, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err, table.Name)
		for _, value := range values {
			fmt.Fprintf(&out, "%s %s\n", table.Name, value)
		}
	}
	return out.String()
}

var schemaQueries = []struct{ name, query string }{
	{"extensions", `SELECT extname::text FROM pg_extension ORDER BY 1`},
	{"types", `
		SELECT format('%s %s%s', t.typname, t.typtype,
		       coalesce(' (' || (SELECT string_agg(quote_literal(e.enumlabel), ', ' ORDER BY e.enumsortorder)
		                           FROM pg_enum e WHERE e.enumtypid = t.oid) || ')', ''))
		  FROM pg_type t
		 WHERE t.typnamespace = 'public'::regnamespace
		   AND t.typtype IN ('e', 'd', 'r', 'm')
		 ORDER BY 1`},
	{"relations", `
		SELECT format('%s kind=%s persistence=%s options=%s rls=%s replica_identity=%s',
		       c.relname, c.relkind, c.relpersistence, coalesce(c.reloptions::text, '{}'),
		       c.relrowsecurity, c.relreplident)
		  FROM pg_class c
		 WHERE c.relnamespace = 'public'::regnamespace
		 ORDER BY 1`},
	{"columns", `
		SELECT format('%s.%s %s%s%s%s%s', c.relname, a.attname, format_type(a.atttypid, a.atttypmod),
		       CASE WHEN a.attcollation <> ty.typcollation THEN ' collate ' || co.collname ELSE '' END,
		       CASE WHEN a.attnotnull THEN ' not null' ELSE '' END,
		       CASE a.attidentity WHEN 'a' THEN ' identity always' WHEN 'd' THEN ' identity by default' ELSE '' END,
		       CASE WHEN a.attgenerated = 's' THEN ' generated as ' || pg_get_expr(d.adbin, d.adrelid)
		            WHEN d.adbin IS NOT NULL THEN ' default ' || pg_get_expr(d.adbin, d.adrelid)
		            ELSE '' END)
		  FROM pg_class c
		  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		  JOIN pg_type ty ON ty.oid = a.atttypid
		  LEFT JOIN pg_collation co ON co.oid = a.attcollation
		  LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
		 WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'p', 'v', 'm', 'c')
		 ORDER BY c.relname, a.attnum`},
	{"constraints", `
		SELECT format('%s %s %s', coalesce(c.relname, ty.typname), con.conname, pg_get_constraintdef(con.oid))
		  FROM pg_constraint con
		  LEFT JOIN pg_class c ON c.oid = con.conrelid
		  LEFT JOIN pg_type ty ON ty.oid = con.contypid
		 WHERE con.connamespace = 'public'::regnamespace
		 ORDER BY 1`},
	{"indexes", `
		SELECT pg_get_indexdef(i.indexrelid)
		  FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE c.relnamespace = 'public'::regnamespace
		 ORDER BY c.relname`},
	{"sequences", `
		SELECT format('%s %s start=%s increment=%s min=%s max=%s cache=%s cycle=%s last=%s owned_by=%s',
		       s.sequencename, s.data_type, s.start_value, s.increment_by, s.min_value, s.max_value,
		       s.cache_size, s.cycle, coalesce(s.last_value::text, 'none'),
		       coalesce((SELECT format('%s.%s', tc.relname, ta.attname)
		                   FROM pg_depend dep
		                   JOIN pg_class tc ON tc.oid = dep.refobjid
		                   JOIN pg_attribute ta ON ta.attrelid = dep.refobjid AND ta.attnum = dep.refobjsubid
		                  WHERE dep.classid = 'pg_class'::regclass
		                    AND dep.objid = format('%I.%I', s.schemaname, s.sequencename)::regclass
		                    AND dep.deptype IN ('a', 'i')), 'none'))
		  FROM pg_sequences s
		 WHERE s.schemaname = 'public'
		 ORDER BY 1`},
	{"functions", `
		SELECT pg_get_functiondef(p.oid)
		  FROM pg_proc p
		 WHERE p.pronamespace = 'public'::regnamespace
		 ORDER BY p.proname`},
	{"triggers", `
		SELECT format('%s enabled=%s', pg_get_triggerdef(t.oid), t.tgenabled)
		  FROM pg_trigger t
		  JOIN pg_class c ON c.oid = t.tgrelid
		 WHERE c.relnamespace = 'public'::regnamespace AND NOT t.tgisinternal
		 ORDER BY c.relname, t.tgname`},
	{"views", `
		SELECT format('%s %s', c.relname, pg_get_viewdef(c.oid))
		  FROM pg_class c
		 WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('v', 'm')
		 ORDER BY 1`},
	{"policies", `
		SELECT format('%s %s %s %s %s', tablename, policyname, cmd, coalesce(qual, ''), coalesce(with_check, ''))
		  FROM pg_policies
		 WHERE schemaname = 'public'
		 ORDER BY 1`},
	{"comments", `
		SELECT format('%s: %s', pg_describe_object(d.classoid, d.objoid, d.objsubid), d.description)
		  FROM pg_description d
		 WHERE (d.classoid = 'pg_class'::regclass
		        AND d.objoid IN (SELECT oid FROM pg_class WHERE relnamespace = 'public'::regnamespace))
		    OR (d.classoid = 'pg_proc'::regclass
		        AND d.objoid IN (SELECT oid FROM pg_proc WHERE pronamespace = 'public'::regnamespace))
		    OR (d.classoid = 'pg_constraint'::regclass
		        AND d.objoid IN (SELECT oid FROM pg_constraint WHERE connamespace = 'public'::regnamespace))
		    OR (d.classoid = 'pg_type'::regclass
		        AND d.objoid IN (SELECT oid FROM pg_type WHERE typnamespace = 'public'::regnamespace))
		    OR (d.classoid = 'pg_trigger'::regclass
		        AND d.objoid IN (SELECT t.oid FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
		                          WHERE c.relnamespace = 'public'::regnamespace))
		 ORDER BY 1`},
}
