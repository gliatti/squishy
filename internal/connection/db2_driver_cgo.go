//go:build db2

package connection

// Compiled into the binary only when built with `-tags db2`. Bringing
// go_ibm_db in as a blank import registers the "go_ibm_db" driver name with
// database/sql; OpenDB2() then dials successfully.
//
// Build prerequisites :
//   - CGO_ENABLED=1
//   - clidriver installed (IBM_DB_HOME pointing at /opt/ibm/clidriver/lib)
//   - LD_LIBRARY_PATH including ${IBM_DB_HOME}/lib so libdb2.so is found at
//     dynamic link time.
//
// See Dockerfile (target `dev`) for the canonical install procedure.

import (
	_ "github.com/ibmdb/go_ibm_db"
)
