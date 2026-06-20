package config

import (
	"fmt"
	"reflect"
	"strings"
)

// Dump renders the effective configuration as "path: value" lines, with
// any field tagged `secret:"true"` shown as [redacted]. Handy for a
// startup log line that records what the app actually loaded without
// leaking secrets. cfg may be a struct or a pointer to one.
//
// The secret tag redacts whole subtrees: tag a credentials group struct
// (or pointer to one) and none of its fields are printed. Pointers are
// dereferenced so a value is never shown as an address or raw struct.
func Dump(cfg any) string {
	rv := reflect.ValueOf(cfg)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return ""
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return ""
	}

	var lines []string
	dumpStruct(rv, "", false, &lines)
	return strings.Join(lines, "\n")
}

// dumpStruct walks rv, appending "path: value" lines. secret propagates
// down from any ancestor tagged secret, so a redacted group hides every
// descendant.
func dumpStruct(rv reflect.Value, path string, secret bool, lines *[]string) {
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if !sf.IsExported() {
			continue
		}
		name := yamlName(sf)
		if name == "-" {
			continue
		}
		fieldSecret := secret || isSecret(sf)
		p := name
		if path != "" {
			p = path + "." + name
		}

		// Dereference pointers so a value is inspected, never printed as
		// an address or "&{...}".
		fv := rv.Field(i)
		if fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				*lines = append(*lines, leafLine(p, "<nil>", fieldSecret))
				continue
			}
			fv = fv.Elem()
		}

		if fv.Kind() == reflect.Struct && fv.Type() != durationType {
			if fieldSecret {
				// Redact the whole subtree rather than descending into it.
				*lines = append(*lines, p+": [redacted]")
				continue
			}
			if sf.Anonymous {
				dumpStruct(fv, path, fieldSecret, lines) // inlined, like yaml.v3
			} else {
				dumpStruct(fv, p, fieldSecret, lines)
			}
			continue
		}
		*lines = append(*lines, leafLine(p, fmt.Sprintf("%v", fv.Interface()), fieldSecret))
	}
}

func leafLine(path, val string, secret bool) string {
	if secret {
		val = "[redacted]"
	}
	return path + ": " + val
}

// isSecret reports whether the field is tagged secret (any value other
// than "false").
func isSecret(sf reflect.StructField) bool {
	v, ok := sf.Tag.Lookup("secret")
	return ok && v != "false"
}
