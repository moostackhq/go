# go

Lightweight, opinionated Go libraries for building web applications.

- [cli](./cli/README.md): typed CLI library with type-safe flag access via generics, auto-generated help, shell completion, and middleware
- [migrations](./migrations/README.md): forward-only SQL database migration library
- [session](./session/README.md): typed HTTP session library with optimistic concurrency, identity-aware operations, and pluggable stores
- [jobs](./jobs/README.md): durable, embeddable job engine with typed jobs, durable steps, cron scheduling, and pluggable storage (memory / SQLite / PostgreSQL)
- [assetmapper](./assetmapper/README.md): hash, serve, and vendor frontend assets without a JS bundler — importmap-based, with an html/template integration and a standalone CLI
- [router](./router/README.md): lightweight HTTP routing library on top of net/http — method shortcuts, middleware chains, prefix groups, Mount, customisable 404 / 405
