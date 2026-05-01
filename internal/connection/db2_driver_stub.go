//go:build !db2

package connection

// This file is the no-op stub used when the binary is compiled WITHOUT the
// `db2` build tag. The real driver lives in db2_driver_cgo.go and brings in
// the IBM go_ibm_db CGO bridge which requires the clidriver library to be
// available at link time.
//
// Pure-Go builds (unit tests, the dev image without clidriver, hosts without
// CGO_ENABLED=1) compile this file: OpenDB2() falls back to sql.Open which
// returns the standard `sql: unknown driver "go_ibm_db" (forgotten import?)`
// error at runtime — clean signal that the binary needs to be rebuilt with
// `go build -tags db2`.
//
// Production builds running inside the api/ docker image MUST be compiled
// with `-tags db2`; the Dockerfile sets CGO_ENABLED=1 and exports
// IBM_DB_HOME / LD_LIBRARY_PATH so the cgo link step finds libdb2.
