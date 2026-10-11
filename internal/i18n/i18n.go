// Package i18n holds every user-facing message. Only English ships today;
// adding a language means adding a catalog with the same keys.
package i18n

import "fmt"

type Lang struct {
	Code string
	Name string
}

var Languages = []Lang{{Code: "en", Name: "English"}}

var catalogs = map[string]map[string]string{"en": en}

var current = en

// Set switches the active catalog. Unknown codes keep the current one.
func Set(code string) bool {
	c, ok := catalogs[code]
	if ok {
		current = c
	}
	return ok
}

// T formats the message for key. A missing key renders as the key itself so
// gaps are visible instead of silently empty.
func T(key string, args ...any) string {
	msg, ok := current[key]
	if !ok {
		msg = key
	}
	if len(args) == 0 {
		return msg
	}
	return fmt.Sprintf(msg, args...)
}

// Has reports whether key exists in the English catalog (used by tests).
func Has(key string) bool {
	_, ok := en[key]
	return ok
}
