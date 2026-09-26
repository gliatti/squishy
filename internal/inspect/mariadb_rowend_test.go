package inspect

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIsMariaDBVersion(t *testing.T) {
	require.True(t, isMariaDBVersion("11.8.8-MariaDB-ubu2404"))
	require.True(t, isMariaDBVersion("10.6.18-MariaDB"))
	require.False(t, isMariaDBVersion("8.4.2"))
}

// The ROW END sentinel of current rows is the server's TIMESTAMP maximum,
// probed rather than hard-coded: 64-bit MariaDB 11.5+ answers both
// FROM_UNIXTIME probes (2106), older servers only the 32-bit one (2038).
func TestRowEndMaxText(t *testing.T) {
	wide := sql.NullTime{Valid: true, Time: time.Date(2106, 2, 7, 6, 28, 15, 999999000, time.UTC)}
	narrow := sql.NullTime{Valid: true, Time: time.Date(2038, 1, 19, 3, 14, 7, 999999000, time.UTC)}

	got, err := rowEndMaxText(wide, narrow)
	require.NoError(t, err)
	require.Equal(t, "2106-02-07 06:28:15.999999", got)

	got, err = rowEndMaxText(sql.NullTime{}, narrow)
	require.NoError(t, err)
	require.Equal(t, "2038-01-19 03:14:07.999999", got)

	_, err = rowEndMaxText(sql.NullTime{}, sql.NullTime{})
	require.Error(t, err)
}
