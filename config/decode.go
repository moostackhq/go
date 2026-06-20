package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var durationType = reflect.TypeOf(Duration{})

// walkLeaves visits every exported scalar field of rv, recursing into
// nested structs (other than [Duration]) and extending the dotted path
// with each field's YAML name. It is the single traversal shared by
// defaults, env overrides, and Dump.
func walkLeaves(rv reflect.Value, path string, fn func(fv reflect.Value, sf reflect.StructField, path string)) {
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
		p := name
		if path != "" {
			p = path + "." + name
		}
		fv := rv.Field(i)
		if fv.Kind() == reflect.Struct && fv.Type() != durationType {
			// Anonymous (embedded) structs are inlined by yaml.v3, so
			// their fields live at the parent's path, not under a segment.
			if sf.Anonymous {
				walkLeaves(fv, path, fn)
			} else {
				walkLeaves(fv, p, fn)
			}
			continue
		}
		fn(fv, sf, p)
	}
}

// yamlName returns the field's YAML key: the `yaml:"name"` tag if set,
// otherwise the lower-cased field name (matching yaml.v3's default).
func yamlName(sf reflect.StructField) string {
	tag, ok := sf.Tag.Lookup("yaml")
	if !ok {
		return strings.ToLower(sf.Name)
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return strings.ToLower(sf.Name)
	}
	return name
}

// applyDefaults sets fields carrying a `default:"..."` tag, before any
// YAML layer is read (so the file overrides a default).
func applyDefaults(rv reflect.Value, probs *[]Problem) {
	walkLeaves(rv, "", func(fv reflect.Value, sf reflect.StructField, path string) {
		def, ok := sf.Tag.Lookup("default")
		if !ok {
			return
		}
		if err := setScalar(fv, def); err != nil {
			*probs = append(*probs, Problem{Key: path, Message: "invalid default: " + err.Error()})
		}
	})
}

// applyEnv overrides fields whose `env:"NAME"` variable is set, after the
// YAML layers (so the environment wins for the few fields opted in).
func applyEnv(rv reflect.Value, probs *[]Problem) {
	walkLeaves(rv, "", func(fv reflect.Value, sf reflect.StructField, path string) {
		tag, ok := sf.Tag.Lookup("env")
		if !ok {
			return
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			return
		}
		// An empty value is treated as unset: a blanked-out variable in a
		// deploy environment must not silently wipe a configured value.
		// Use the file to set a field to "".
		val, ok := os.LookupEnv(name)
		if !ok || val == "" {
			return
		}
		if err := setScalar(fv, val); err != nil {
			*probs = append(*probs, Problem{Key: name, Message: err.Error()})
		}
	})
}

// setScalar parses s into fv. It backs the default-tag and env-override
// paths (YAML decoding is yaml.v3's job). Supports string, bool, the
// sized int/uint/float kinds, [Duration], []string (comma-separated),
// and pointers to any of those.
func setScalar(fv reflect.Value, s string) error {
	if fv.Type() == durationType {
		d, err := time.ParseDuration(s)
		if err != nil {
			return errors.New(`must be a duration like "30s" or "5m"`)
		}
		fv.Set(reflect.ValueOf(Duration{d: d}))
		return nil
	}
	if fv.Kind() == reflect.Pointer {
		elem := reflect.New(fv.Type().Elem())
		if err := setScalar(elem.Elem(), s); err != nil {
			return err
		}
		fv.Set(elem)
		return nil
	}

	switch fv.Kind() {
	case reflect.String:
		fv.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(strings.TrimSpace(s))
		if err != nil {
			return errors.New("must be true or false")
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, fv.Type().Bits())
		if err != nil {
			return errors.New("must be a whole number")
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, fv.Type().Bits())
		if err != nil {
			return errors.New("must be a non-negative whole number")
		}
		fv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, fv.Type().Bits())
		if err != nil {
			return errors.New("must be a number")
		}
		fv.SetFloat(f)
	case reflect.Slice:
		if fv.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported slice element type %s", fv.Type().Elem())
		}
		parts := splitComma(s)
		out := reflect.MakeSlice(fv.Type(), len(parts), len(parts))
		for i, p := range parts {
			out.Index(i).SetString(p)
		}
		fv.Set(out)
	default:
		return fmt.Errorf("unsupported type %s", fv.Type())
	}
	return nil
}

// splitComma splits a comma-separated string into trimmed, non-empty
// parts. An empty input yields an empty (non-nil) slice.
func splitComma(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
