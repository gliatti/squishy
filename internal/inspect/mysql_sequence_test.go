package inspect

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// SHOW CREATE SEQUENCE only reports the original START; a used sequence
// must carry its next value as ALTER SEQUENCE … RESTART WITH.
func TestMariaDBSequenceDDL(t *testing.T) {
	show := "CREATE SEQUENCE `order_seq` start with 1 minvalue 1 maxvalue 9223372036854775806 increment by 1 nocache nocycle ENGINE=InnoDB"
	require.Equal(t, show, mariadbSequenceDDL(show, "order_seq", 1, 1))
	require.Equal(t, show+";\nALTER SEQUENCE `order_seq` RESTART WITH 1001",
		mariadbSequenceDDL(show, "order_seq", 1001, 1))
	require.Equal(t, "x;\nALTER SEQUENCE `we``ird` RESTART WITH -5",
		mariadbSequenceDDL("x", "we`ird", -5, 1))
}
