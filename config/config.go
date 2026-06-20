// Package config loads a typed configuration struct from YAML, with
// per-field environment overrides and validation.
//
// A YAML file is the source of truth — structured, reviewable, and
// self-documenting. Environment variables are a narrow, explicit
// override: a field is env-reachable only if it carries an `env:"NAME"`
// tag, so secrets and per-deploy values can be injected without anything
// else leaking into the process environment.
//
//	type Config struct {
//	    Server struct {
//	        Addr string `yaml:"addr" default:":8080"`
//	    } `yaml:"server"`
//	    CSRF struct {
//	        Secret string `yaml:"secret" env:"APP_CSRF_SECRET" secret:"true"`
//	    } `yaml:"csrf"`
//	}
//
//	cfg, err := config.Load[Config](
//	    config.File("config.yaml"),
//	    config.FileOptional("config.local.yaml"),
//	)
//
// Resolution order is: `default:` tags, then each YAML layer in option
// order (later wins), then `env:` overrides, then — if the struct
// implements [validation.Validatable] — its Validate method. Every
// problem found along the way is collected and returned together as a
// [*LoadError], so a misconfigured deployment surfaces all of its issues
// at once.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"

	"github.com/moostackhq/go/validation"
	"gopkg.in/yaml.v3"
)

type fileLayer struct {
	path     string
	optional bool
}

type loader struct {
	layers []fileLayer
}

// Option configures [Load].
type Option func(*loader)

// File adds a required YAML layer. A missing or unreadable file is a load
// error. Layers apply in option order; a later layer overrides an earlier
// one field by field.
func File(path string) Option {
	return func(l *loader) { l.layers = append(l.layers, fileLayer{path: path}) }
}

// FileOptional adds a YAML layer that is applied only if the file exists
// — for a local-development overlay that need not be present.
func FileOptional(path string) Option {
	return func(l *loader) { l.layers = append(l.layers, fileLayer{path: path, optional: true}) }
}

// Load builds a *T from the configured layers. T must be a struct. It
// returns a [*LoadError] aggregating every default/parse/override/
// validation problem, or a fully populated *T.
func Load[T any](opts ...Option) (*T, error) {
	var l loader
	for _, o := range opts {
		o(&l)
	}

	dst := new(T)
	rv := reflect.ValueOf(dst).Elem()
	if rv.Kind() != reflect.Struct {
		return nil, fmt.Errorf("config: Load[T]: T must be a struct, got %s", rv.Kind())
	}

	var probs []Problem

	applyDefaults(rv, &probs)

	for _, layer := range l.layers {
		data, err := os.ReadFile(layer.path)
		if err != nil {
			if layer.optional && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("config: read %s: %w", layer.path, err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true) // reject unknown keys — catches typos in the file
		if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("config: parse %s: %w", layer.path, err)
		}
	}

	applyEnv(rv, &probs)

	if v, ok := any(dst).(validation.Validatable); ok {
		for key, msg := range v.Validate() {
			probs = append(probs, Problem{Key: key, Message: msg})
		}
	}

	if len(probs) > 0 {
		return nil, &LoadError{Problems: probs}
	}
	return dst, nil
}
