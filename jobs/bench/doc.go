// Package bench houses end-to-end throughput benchmarks for the
// jobs library. It is intentionally test-only: every backend is
// exercised through the same pipeline so the resulting numbers
// (jobs/sec) are directly comparable.
//
// Run all:
//
//	go test -bench=. -benchtime=2s ./bench
//
// Slice by backend or worker count via the -bench regex:
//
//	go test -bench='Pipeline/sqlite' ./bench
//	go test -bench='workers=8' ./bench
//	go test -bench='Enqueue' ./bench
//
// The Postgres backend is skipped unless JOBS_PG_URL is set, the
// same as the regular Postgres tests.
package bench
