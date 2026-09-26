package worker

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/dalibo/squishy/internal/dataxfer"
)

func TestCopySourceDialectVariants(t *testing.T) {
	d := &Deps{SourceDialect: dataxfer.MySQLSource()}

	def, err := d.copySourceDialect("", "")
	require.NoError(t, err)
	require.Equal(t, dataxfer.MySQLSource(), def)

	hist, err := d.copySourceDialect(copySourceVariantSystemTimeHistory, "valid_to")
	require.NoError(t, err)
	require.Equal(t, dataxfer.MySQLSystemTimeHistorySource("valid_to"), hist)

	_, err = d.copySourceDialect(copySourceVariantSystemTimeHistory, "")
	require.Error(t, err)
	_, err = d.copySourceDialect("bogus", "x")
	require.Error(t, err)

	ora := &Deps{SourceDialect: dataxfer.OracleSource()}
	_, err = ora.copySourceDialect(copySourceVariantSystemTimeHistory, "valid_to")
	require.Error(t, err, "system-time history is MariaDB-only")
}
