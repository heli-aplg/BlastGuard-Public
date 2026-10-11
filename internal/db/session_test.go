package db

import "testing"

func TestGuard(t *testing.T) {
	ok := []string{"SELECT 1", "  select 1", "-- c\nSELECT 1", "/* c */ WITH x AS (SELECT 1) SELECT * FROM x", "SHOW search_path"}
	bad := []string{"DELETE FROM t", "UPDATE t SET a=1", "INSERT INTO t VALUES (1)", "TRUNCATE t", "/* SELECT */ DROP TABLE t", "-- SELECT\nDELETE FROM t", "", "COPY t TO stdout", "CALL p()", "DO $$ BEGIN END $$"}
	for _, s := range ok {
		if err := guard(s); err != nil {
			t.Errorf("guard rejected %q: %v", s, err)
		}
	}
	for _, s := range bad {
		if err := guard(s); err == nil {
			t.Errorf("guard accepted %q", s)
		}
	}
}
