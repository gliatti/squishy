package discover

// oracleSystemSchemas is the fallback exclude list when DBA_USERS is unavailable
// (super-user not granted SELECT on DBA_USERS, so we read ALL_USERS instead and
// filter by name). It is also applied on top of the DBA_USERS query as a safety
// net: some maintenance users (e.g. PDBADMIN in 23ai) are NOT marked
// oracle_maintained='Y' yet are clearly not migration material.
var oracleSystemSchemas = map[string]bool{
	"SYS": true, "SYSTEM": true, "OUTLN": true, "DBSNMP": true,
	"APPQOSSYS": true, "AUDSYS": true, "XDB": true, "WMSYS": true,
	"CTXSYS": true, "GSMADMIN_INTERNAL": true, "GSMCATUSER": true,
	"GSMUSER": true, "GGSYS": true, "ANONYMOUS": true, "OJVMSYS": true,
	"LBACSYS": true, "DVSYS": true, "DVF": true, "OLAPSYS": true,
	"ORDDATA": true, "ORDPLUGINS": true, "ORDSYS": true, "MDDATA": true,
	"REMOTE_SCHEDULER_AGENT": true, "SYSBACKUP": true, "SYSDG": true,
	"SYSKM": true, "SYSRAC": true, "SYSMAN": true, "MDSYS": true,
	"XS$NULL": true, "FLOWS_FILES": true, "EXFSYS": true, "CTXAPP": true,
	"OWBSYS": true, "OWBSYS_AUDIT": true,
	// 23ai / PDB-specific maintenance accounts.
	"PDBADMIN":          true,
	"DGPDB_INT":         true,
	"DIP":               true,
	"ORACLE_OCM":        true,
	"DBSFWUSER":         true,
	"GSMROOTUSER":       true,
	"SYS$UMF":           true,
	"AUDIT_ADMIN":       true,
	"AUDIT_VIEWER":      true,
	"DGPDB_INT_VIEWER":  true,
	"GGSHAREDCAP":       true,
}

var mysqlSystemDatabases = map[string]bool{
	"information_schema": true, "mysql": true,
	"performance_schema": true, "sys": true,
}

// db2SystemSchemas is the in-Go fallback exclude list applied on top of the
// catalog query, covering schemas that the upstream `NOT LIKE 'SYS%'` filter
// misses (SQLJ, NULLID, IBM tooling, replication, monitoring, …).
var db2SystemSchemas = map[string]bool{
	// LUW catalog + system stored procs.
	"SYSCAT": true, "SYSSTAT": true, "SYSIBM": true, "SYSFUN": true,
	"SYSPROC": true, "SYSIBMADM": true, "SYSIBMTS": true,
	"SYSIBMINTERNAL": true, "SYSPUBLIC": true,
	// SQLJ + JDBC compatibility.
	"NULLID": true, "SQLJ": true,
	// z/OS specific.
	"DSNRGFDB": true, "DSNRLST": true, "DSNULI": true, "DSN8910": true,
	"SYSACCEL": true, "DSNAOCLI": true,
	// Tooling / monitoring / IBM-shipped frameworks.
	"IBM_RTMON": true, "IBM_SNAP_REP": true,
	// dataq / replication / federation appliances.
	"DB2QREP": true, "ASN": true, "DB2OSC": true, "DB2INST1_CONNECT": true,
}
