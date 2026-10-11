package internal_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"blastguard/internal/catalog"
	"blastguard/internal/i18n"
)

func TestQuoteAndPredicates(t *testing.T) {
	if got := catalog.QuoteIdent(`we"ird`); got != `"we""ird"` {
		t.Errorf("QuoteIdent = %s", got)
	}
	if got := catalog.InPredicate([]string{"a"}, [][]string{{"'1'"}, {"'2'"}}); got != `"a" IN ('1', '2')` {
		t.Errorf("single = %s", got)
	}
	if got := catalog.InPredicate([]string{"a", "b"}, [][]string{{"'1'", "'x'"}}); got != `("a", "b") IN (('1', 'x'))` {
		t.Errorf("multi = %s", got)
	}
}

// Every i18n.T("key") in the code must exist in the English catalog.
func TestI18nKeysExist(t *testing.T) {
	re := regexp.MustCompile(`i18n\.T\("([^"]+)"`)
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			if !i18n.Has(m[1]) {
				t.Errorf("%s: missing i18n key %q", path, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
