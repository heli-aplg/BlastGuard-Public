package analyzer

import (
	"context"
	"testing"

	"blastguard/internal/catalog"
)

func TestNormalizeInput(t *testing.T) {
	cases := map[string]string{
		"  42 ":     "42",
		"'abc'":     "abc",
		"'it''s'":   "it's",
		"a,b":       "a,b", // commas are legal in text keys; the server validates the type
		"\t'x y'\n": "x y",
	}
	for in, want := range cases {
		got, err := NormalizeInput(in)
		if err != nil || got != want {
			t.Errorf("NormalizeInput(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "   ", "''", "1\n2"} {
		if _, err := NormalizeInput(bad); err == nil {
			t.Errorf("NormalizeInput(%q) accepted", bad)
		}
	}
}

func TestStaticAnalyzer(t *testing.T) {
	src := `CREATE OR REPLACE FUNCTION s.f() RETURNS trigger LANGUAGE plpgsql AS $function$
BEGIN
  -- DELETE FROM commented_out;
  /* UPDATE also_commented SET x = 1; */
  DELETE FROM Audit.Log WHERE id = OLD.id;
  UPDATE stats SET n = n - 1;
  INSERT INTO "Weird"."Name" VALUES (1);
  EXECUTE 'TRUNCATE ' || TG_ARGV[0];
  PERFORM other.fn(OLD.id);
  PERFORM dblink_exec('remote', 'DELETE FROM t');
  IF OLD.locked THEN RAISE EXCEPTION 'locked'; END IF;
  RETURN NULL;
END $function$`
	tg := catalog.Trigger{Name: "t", Type: 1 | 2 | 8, Lang: "plpgsql"} // ROW BEFORE DELETE
	got, err := StaticAnalyzer{}.Analyze(context.Background(), TriggerInput{Trigger: tg, Source: src})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"writes|audit.log": false, "writes|stats": false, `writes|"Weird"."Name"`: false,
		"dynamic-sql|": false, "calls|other.fn": false, "external|": false,
		"may-abort|": false, "may-skip|": false,
	}
	for _, f := range got {
		if f.Label != staticLabel {
			t.Errorf("finding without unverified label: %+v", f)
		}
		if f.Ref == "commented_out" || f.Ref == "also_commented" {
			t.Errorf("comment was analyzed: %+v", f)
		}
		k := f.Kind + "|" + f.Ref
		if f.Kind == "calls" {
			k = "calls|other.fn"
		}
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing finding %s in %+v", k, got)
		}
	}

	c, _ := StaticAnalyzer{}.Analyze(context.Background(), TriggerInput{Trigger: catalog.Trigger{Lang: "c"}})
	if len(c) != 1 || c[0].Kind != "language" {
		t.Errorf("C function: %+v", c)
	}
}

func TestParentTuplesSkipsNulls(t *testing.T) {
	info := &catalog.TableInfo{Columns: []catalog.Column{{Name: "a"}, {Name: "b"}}}
	rows := []*Row{{Lits: []string{"'1'", "'x'"}}, {Lits: []string{"'1'", "'x'"}}, {Lits: []string{"NULL", "'y'"}}}
	got := parentTuples(info, rows, []string{"a", "b"})
	if len(got) != 1 || got[0][0] != "'1'" {
		t.Errorf("parentTuples = %v", got)
	}
}
