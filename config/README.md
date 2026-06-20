# config

Typed application configuration for Go: a **YAML file is the source of truth**, with explicit per-field **environment overrides** and validation.

## Why YAML-first

A flat namespace of environment variables has no structure — you can't browse it, review it as a unit, or see what knobs exist, and it pushes secrets through the process environment. So the structure lives in a YAML file that can be read, diffed, and documented. The environment is a *narrow override*: a field is env-reachable only if it carries an `env:` tag, so secrets and per-deploy values inject cleanly without anything else leaking out.

## Usage

`config.yaml` — the source of truth:

```yaml
server:
  addr: ":8080"
  dev: false
database:
  dsn: "demo.db"
cache:
  status_ttl: 30s
csrf:
  secret: ""        # injected per-deploy via env; never committed
```

The struct — `yaml` for structure, `env` to opt a field into override, `default` for a fallback:

```go
type Config struct {
    Server struct {
        Addr string `yaml:"addr" default:":8080"`
        Dev  bool   `yaml:"dev"`
    } `yaml:"server"`
    Database struct {
        DSN string `yaml:"dsn" default:"demo.db"`
    } `yaml:"database"`
    Cache struct {
        StatusTTL config.Duration `yaml:"status_ttl" default:"30s"`
    } `yaml:"cache"`
    CSRF struct {
        Secret string `yaml:"secret" env:"APP_CSRF_SECRET" secret:"true"`
    } `yaml:"csrf"`
}

// Optional: value-shape rules via the validation package.
func (c Config) Validate() validation.Errors {
    return validation.Check(
        validation.Field("server.addr", c.Server.Addr, validation.Required()),
        validation.Field("csrf.secret", c.CSRF.Secret, validation.Required(), validation.MinLen(32)),
    )
}
```

Load once at the composition root, then use plain typed fields:

```go
cfg, err := config.Load[Config](
    config.File("config.yaml"),                // required base
    config.FileOptional("config.local.yaml"),  // dev overlay, applied if present
)
if err != nil {
    return err // aggregated: every problem at once
}

addr := cfg.Server.Addr
ttl  := cfg.Cache.StatusTTL.Duration()
```

Override a single field in production without touching the file:

```console
$ APP_CSRF_SECRET=$(cat /run/secrets/csrf) ./app
```

## Resolution order

`default:` tags → YAML layers (in option order, later wins) → `env:` overrides → `Validate()`.

So defaults are the floor, the file is the source of truth, the environment overrides the few fields you opted in, and validation has the final say. Every problem found along the way is collected into one `*LoadError`:

```
config: 2 problems:
  csrf.secret: is required
  server.addr: is required
```

A few behaviors worth knowing:

- **An empty env value is treated as unset.** `FOO=` does *not* override a configured value (so a blanked-out deploy variable can't silently wipe config); set a field to empty via the file instead.
- **Across layers, a list replaces but a map merges.** A later YAML layer overwrites a sequence field wholesale, but deep-merges a mapping field key-by-key (this is yaml.v3's overlay behavior).
- **`*LoadError` keys mix namespaces.** Default/YAML/validation problems are keyed by the dotted field path (`csrf.secret`); an env-override parse error is keyed by the variable name (`APP_CSRF_SECRET`), since that's the actionable identifier for that failure.
- File-read and YAML-parse failures are returned directly (not aggregated) — a malformed or missing required file is a hard stop before per-field resolution begins.

## Field tags

| Tag | Meaning |
|---|---|
| `yaml:"name"` | Key in the file (and the dotted path in error messages). |
| `default:"v"` | Value used when nothing else sets the field. |
| `env:"NAME"` | This field — and only this one — may be overridden by `$NAME`. |
| `secret:"true"` | Redacted by `config.Dump`. |

## Types

`string`, `bool`, the sized `int`/`uint`/`float` kinds, `[]string` (comma-separated in a default/env value; a native list in YAML), pointers to scalars (optional), nested structs (grouping), and `config.Duration` — a `time.Duration` that reads `"30s"`/`"5m"` from YAML, defaults, and env (plain `time.Duration` can't, as YAML has no duration scalar).

Unknown YAML keys are rejected (catches typos), and an unsupported field type is a load error rather than a silent skip.

## Dump

```go
slog.Info("config loaded", "effective", config.Dump(cfg))
```

Renders `path: value` lines with `secret:"true"` fields shown as `[redacted]` — a safe startup record of what was actually loaded.

## Status

Reference code. One dependency beyond the standard library and `validation`: `gopkg.in/yaml.v3`. JSON config (zero extra deps) could be added behind the same struct later; YAML is the primary source.
