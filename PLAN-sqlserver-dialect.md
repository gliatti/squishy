# Plan — dialecte SQL Server (T-SQL) → PostgreSQL, AST only

Rédigé le 2026-11-04 (Fable). Exécution : workflow d'agents Opus 5.5, branche
`feat/sqlserver-dialect`. Modèle de référence : **le chemin MySQL/MariaDB**
(parser `internal/dialects/mysql` → `internal/sqlparse/ast` →
`ast.Rewrite(node, RewriteMySQLAST)` dans `internal/translate/mysql_visitors.go`
→ writer `internal/dialects/postgres`). Oracle/DB2 ne servent que de référence
de **plomberie** (connexion, discover, inspect, dataxfer, compose), jamais de
traduction (ils portent encore du réécrivain texte).

Carte du code : `graphify-out/GRAPH_REPORT.md` (rafraîchi le 2026-11-04). Hubs à
étendre : `Translate()` (translator.go), `parseRoutineBodyByKind()` /
`translateRoutineBodyWithSigs()` (plsql_xlate.go), `MapType()` (types_map.go),
`connCache.forRun()` (main.go), `discover.Schemas()`, `inspectSourceForSchema()`
/ `openSourceForInstance()` / `validKind()` (httpapi), `planner.Build()`,
`dataxfer.SourceDialect`, `connection.Test()`.

## 1. Dumps retenus

| Dump | Source | Taille | Ce qu'il exerce |
|---|---|---|---|
| `dumps/sqlserver/adventureworks/` (`instawdb.sql` + CSV) | `Microsoft/sql-server-samples` release `adventureworks`, `AdventureWorks-oltp-install-script.zip` (17 MB) | ~70 tables, 5 schémas (`HumanResources`, `Person`, `Production`, `Purchasing`, `Sales`) | **Le plus exhaustif** : UDT alias (`Name`, `Flag`, `Phone`…), XML + schema collections, `hierarchyid`, `uniqueidentifier ROWGUIDCOL`, colonnes calculées (PERSISTED et non), `IDENTITY`, CHECK/DEFAULT nommés, FK inter-schémas, index filtrés / INCLUDE / XML / fulltext, vues (CTE, XML), fonctions scalaires + TVF inline + TVF multi-instructions, procédures (TRY/CATCH, OUTPUT, `uspLogError`), triggers AFTER multi-événements et INSTEAD OF (`inserted`/`deleted`, `UPDATE()`, `@@ROWCOUNT`), trigger DDL, `sp_addextendedproperty` partout (commentaires), `BULK INSERT` de CSV |
| `dumps/sqlserver/instnwnd.sql` (Northwind) | idem repo, `samples/databases/northwind-pubs/instnwnd.sql` (1 MB) | 13 tables `dbo`, identifiants avec espaces `[Order Details]` | Premier dump de la campagne (petit, mono-schéma) |
| `dumps/sqlserver/instpubs.sql` (pubs) | idem (125 KB) | triggers, procs, `image`/`text` | Bonus |
| `test/fixtures/sqlserver_sample.sql` + `sqlserver_seed.sql` | à écrire (comme `mysql_sample.sql`) | synthèse de tous les types + idiomes | fixture e2e + fixture des tests parser |

Licences : MIT (sql-server-samples). Les `.bak` (WideWorldImporters,
AdventureWorks2022.bak) ne sont pas des dumps SQL : hors périmètre.

## 2. Décisions de design

1. **Id de dialecte** `sqlserver` (package Go `internal/dialects/tsql`, label
   « Microsoft SQL Server 2016–2022 »), `dialects.KindSQLServer` +
   `dialects.IsSQLServer(k)`. Grammaire vendorée `reference/TSqlLexer.g4` /
   `TSqlParser.g4` (grammars-v4 `sql/tsql`, MIT) — spec, jamais modifiée, pas
   de runtime ANTLR.
2. **Unité de découverte = la base SQL Server** (`sys.databases` moins
   `master`, `tempdb`, `model`, `msdb`, `ReportServer*`, `distribution`,
   `SSISDB`). `migration.source_schema_name` = nom de la base ; le pool source
   est ouvert sur cette base (comme MySQL : `srcDatabase = sourceSchema`).
3. **Préservation des schémas T-SQL** (AdventureWorks est inter-schémas) :
   chaque objet garde son schéma, mappé vers un schéma PG par
   `schemaMap(src)` : stratégie `dedicated_db` → `lower(src)` (`dbo` reste
   `dbo`), `dedicated_schema` → `lower(db) + "_" + lower(src)`. Le traducteur
   émet un `CREATE SCHEMA IF NOT EXISTS` par schéma distinct et un
   `search_path` qui les liste tous. `inspect.ObjectSnapshot` gagne un champ
   `Schema` ; le planner met `schema = snapshot.Schema` (schéma T-SQL) et
   `target_schema = PGTable.Schema` dans chaque payload `copy_table` ;
   `squishy.migrations.target_schema_name` reste `public` (non significatif).
4. **Pliage des identifiants en minuscules** (collation CI par défaut) :
   `[SalesOrderHeader]` → `salesorderheader`, `[Order Details]` →
   `"order details"`. Visiteur AST sur `ast.Ident` (modèle :
   `VisitOracleIdentCaseFold`) + `dataxfer.normalizeDstIdent` et
   `worker.pgNormalizeIdent` pour le kind `sqlserver`.
5. **Source du DDL** : pas de `SHOW CREATE` en T-SQL. `inspect/sqlserver.go`
   **synthétise un script T-SQL canonique par table** depuis `sys.tables`,
   `sys.columns`, `sys.types` (alias + types tables), `sys.computed_columns`,
   `sys.identity_columns`, `sys.default_constraints`, `sys.check_constraints`,
   `sys.key_constraints`, `sys.indexes`/`sys.index_columns` (INCLUDE, filtre,
   unique), `sys.foreign_keys`/`sys.foreign_key_columns`, `sys.sequences`
   (+ `ALTER SEQUENCE … RESTART WITH`), `sys.extended_properties`
   (`MS_Description` → `EXEC sys.sp_addextendedproperty` pour n'avoir qu'un
   seul chemin avec les vrais scripts), `sys.periods` / `temporal_type`
   (tables historiques exclues de `Tables`). Vues, procédures, fonctions,
   triggers : `sys.sql_modules.definition` verbatim. Comptages :
   `sys.dm_db_partition_stats`. Ce texte est **parsé par le même parser** que
   les scripts réels (Northwind, AdventureWorks) : un seul chemin AST.
6. **Parser `tsql`** (hand-roll, lexer char-par-char = seule exception
   texte autorisée) : `[crochets]`, `"id"`, `N'…'`, `0x…`, `@var`, `@@global`,
   `GO` (séparateur de batch, rôle du `DELIMITER` MySQL), `--`/`/* */`.
   DDL : CREATE/ALTER TABLE (IDENTITY, computed [PERSISTED], DEFAULT/CHECK
   nommés, ROWGUIDCOL, SPARSE, COLLATE, `WITH (SYSTEM_VERSIONING…)`,
   `PERIOD FOR SYSTEM_TIME`, `ON [PRIMARY]` ignoré, options `WITH (…)`
   ignorées), CREATE INDEX (CLUSTERED/NONCLUSTERED ignorés, INCLUDE, WHERE),
   CREATE/ALTER VIEW (SCHEMABINDING, CHECK OPTION), PROCEDURE/PROC, FUNCTION
   (scalaire, TVF inline `RETURNS TABLE AS RETURN (…)`, TVF multi
   `RETURNS @t TABLE(…)`), TRIGGER (AFTER/FOR/INSTEAD OF, multi-événements ;
   DDL/LOGON → `NoopStmt` + avertissement), CREATE TYPE … FROM (alias) / AS
   TABLE, CREATE SEQUENCE, CREATE SCHEMA, SYNONYM / XML SCHEMA COLLECTION /
   FULLTEXT / PARTITION FUNCTION-SCHEME → `NoopStmt` expliqué,
   `EXEC sp_addextendedproperty` → nouveau nœud `ast.CommentStmt`, `USE` /
   `SET …` / `IF OBJECT_ID(…) IS NOT NULL DROP …` / `PRINT` au niveau script
   → noop. DML : SELECT (TOP [PERCENT] [WITH TIES], OFFSET/FETCH, CTE,
   OUTER/CROSS APPLY, hints `WITH (NOLOCK)` ignorés, FOR XML → avertissement),
   INSERT (OUTPUT), UPDATE/DELETE … FROM/JOIN, MERGE (`ast.MergeStmt`).
   Expressions : CASE, CAST, `CONVERT(type, expr[, style])`, IIF, ISNULL,
   COALESCE, `+` (concat), `%`, `& | ^`, LIKE [ESCAPE], IN/EXISTS/BETWEEN,
   sous-requêtes, OVER, `COLLATE` (ignoré), `NEXT VALUE FOR` →
   `SequenceRef`, méthodes XML `.value()`/`.query()` → `RawExpr` + warning.
   Procédural (réutilise `ast.Block`, `DeclareVar`, `DeclareCursor`,
   `AssignStmt`, `IfStmt`, `WhileStmt`, `LeaveStmt`/`IterateStmt`
   (BREAK/CONTINUE), `ReturnStmt`, `CallStmt`, `OpenStmt`/`FetchStmt`/
   `CloseStmt`, `RaiseStmt`, `ExceptionBlock` (TRY/CATCH), `SelectInto`,
   `ExecuteImmediateStmt`, `GotoStmt`, `NullStmt`) : `DECLARE @v t [= e]`,
   `DECLARE @t TABLE(…)`, `SET @v =`, `SELECT @v = … FROM`, IF/ELSE, WHILE,
   TRY/CATCH, THROW/RAISERROR, PRINT, EXEC[UTE] (proc, `sp_executesql`,
   `EXEC(@sql)`), BEGIN TRAN/COMMIT/ROLLBACK, `SET NOCOUNT/XACT_ABORT`
   (ignorés), curseurs, DEALLOCATE, WAITFOR (warning). Nouveaux types AST
   préfixés `TSQL*` dans `ast/types.go` quand aucun nœud générique ne
   convient (money, datetime/datetime2/smalldatetime/datetimeoffset,
   uniqueidentifier, rowversion, hierarchyid, sql_variant,
   geography/geometry, `(MAX)`, image/ntext, type table). Tout nœud nouveau
   est couvert par `ast.Rewrite` (rewrite.go) et par les writers PG.
7. **Traduction** (`internal/translate/tsql_*.go`, modèle `mysql_*.go`) :
   - `types_map_tsql.go` : int→integer, bigint, smallint, tinyint→smallint,
     bit→boolean, decimal/numeric, money→numeric(19,4), smallmoney→
     numeric(10,4), float(n)→double precision/real, date, time(n),
     datetime→timestamp(3), datetime2(n)→timestamp(n), smalldatetime→
     timestamp(0), datetimeoffset→timestamptz, char/varchar(n|max)→
     char/varchar/text, nchar/nvarchar→char/varchar/text, text/ntext→text,
     binary/varbinary/image→bytea, uniqueidentifier→uuid, xml→xml,
     rowversion/timestamp→bytea (note), hierarchyid→text (+ `ltree` si
     extension), geography/geometry→PostGIS si présent sinon text + prereq,
     sql_variant→text + warning, alias UDT→`CREATE DOMAIN`, type table→
     composite + warning.
   - `tsql_visitors.go` : `RewriteTSQLAST = ast.Compose(…)` — pliage
     d'identifiants et mapping de schéma, ISNULL→coalesce, GETDATE/
     SYSDATETIME/CURRENT_TIMESTAMP→now()/localtimestamp, GETUTCDATE→
     `now() at time zone 'utc'`, NEWID→gen_random_uuid, LEN→length(rtrim()),
     CHARINDEX→strpos (ordre inversé), SUBSTRING ok, STUFF→overlay,
     REPLICATE→repeat, SPACE→repeat(' ',n), IIF→CASE, CONVERT→CAST (style
     non trivial → note), `+` → `||` quand un opérande est prouvé texte
     (littéral, fonction texte, cast texte, colonne texte connue), sinon
     conservé + note, TOP n→LIMIT, TOP WITH TIES→FETCH FIRST … WITH TIES,
     TOP PERCENT→warning, APPLY→LATERAL, `@@IDENTITY`/`SCOPE_IDENTITY()`→
     lastval(), `@@ROWCOUNT`→variable + `GET DIAGNOSTICS … ROW_COUNT` après
     chaque DML (nouveau nœud `pgast.PLGetDiagnostics`), DATEADD/DATEDIFF/
     DATEPART/DATENAME et LIKE à classes `[…]` → appels au shim `tsql.*`,
     puis passe de précédence des opérandes (comme
     `VisitMySQLOperandPrecedence`). **Aucune inspection de texte SQL.**
   - Shim `postgres-init/03-tsql-shim.sql` (schéma `tsql`, PL/pgSQL
     char-par-char, **sans regex ni SIMILAR TO ni `~`**) : `tsql.like`,
     `tsql.dateadd`, `tsql.datediff` (sémantique de franchissement de
     frontière T-SQL), `tsql.datepart`, `tsql.datename`, `tsql.isnumeric`.
     Le même texte est embarqué en Go (`tsql_shim.go`) et émis en
     `PreActions` dès qu'une expression traduite appelle `tsql.*` (détection
     sur l'AST, pas sur le texte).
   - Mode « schémas préservés » dans `translator.go` (`Options.SchemaMap`
     / `SchemaPrefix` posés par `httpapi.planMigration` pour le kind
     `sqlserver`), `buildDDL` multi-schémas, IDENTITY(seed,inc) →
     `GENERATED BY DEFAULT AS IDENTITY (START WITH seed INCREMENT BY inc)` +
     restart post-copie (`emitIdentityRestarts`), colonnes calculées
     PERSISTED → `GENERATED ALWAYS AS (…) STORED` quand l'expression est
     traduite et immuable, sinon chemin `CopiedGenerated` existant (colonne
     plate copiée depuis la source + prérequis), DEFAULT/CHECK nommés,
     index INCLUDE/partiels, FK inter-schémas, `CommentStmt` → `COMMENT ON`,
     séquences, tables temporelles → prérequis bloquant (pas d'émulation
     en v1).
   - Routines : procédure → `CREATE PROCEDURE` (OUTPUT→INOUT, valeurs par
     défaut ; jeu de résultats renvoyé par SELECT → warning + prérequis),
     fonction scalaire → `CREATE FUNCTION`, TVF inline → `RETURNS TABLE(…)`
     (colonnes résolues depuis les tables connues, sinon prérequis), TVF
     multi → `RETURNS TABLE(…)` depuis la déclaration `@t TABLE`, corps avec
     table temporaire + `RETURN QUERY`. Triggers AFTER/FOR → trigger
     `FOR EACH STATEMENT` + `REFERENCING NEW TABLE AS inserted OLD TABLE AS
     deleted` (ajout `NewTable`/`OldTable` à `pgast.CreateTrigger` + writer),
     INSTEAD OF → `FOR EACH ROW` avec `inserted`/`deleted` ligne unique
     (note), `UPDATE(col)` → warning explicite. Corps : `parseRoutineBodyByKind`
     et `translateRoutineBodyWithSigs` ouverts via un **registre par kind**
     (`dialect_hooks.go`, même idée que `RegisterTypeMapper`) pour ne pas
     mélanger les chemins MySQL/Oracle/T-SQL.
   - `ast_only_guard_test.go` généralisé : interdiction de `NewLexer` /
     `Tokenize` de **tous** les dialectes (mysql, tsql) depuis
     `internal/translate` ; aucun `strings.*` sur du SQL dans `tsql_*.go`.
8. **Plomberie** : driver `github.com/microsoft/go-mssqldb` (pur Go, pas de
   CGO — l'image `dev` alpine suffit), DSN
   `sqlserver://user:pass@host:port?database=X&encrypt=disable`,
   placeholders `@p1…`, `connection.OpenSQLServer` / `Test`
   (`SELECT @@VERSION`), `dataxfer.SQLServerSource()` (`Quote` → `[x]`,
   `OFFSET … ROWS FETCH NEXT … ROWS ONLY` avec `ORDER BY` obligatoire,
   catalogues `INFORMATION_SCHEMA.*` + `sys.computed_columns` pour exclure
   les colonnes calculées / `rowversion` / spatiales), `discover/sqlserver.go`,
   `inspect/sqlserver.go`, `main.go`, `httpapi`, `mcp-server/tools.go`,
   `web/src/api/client.ts` + `AddInstance.vue`, migration
   `000005_add_sqlserver_connection_kind` (enum + check).
9. **Docker** : service `sqlserver-sample` (`mcr.microsoft.com/mssql/server:2022-latest`,
   profils `sqlserver` + `e2e`, `ACCEPT_EULA=Y`, `MSSQL_PID=Developer`,
   mot de passe SA conforme, port `${SQUISHY_SQLSERVER_HOST_PORT:-1433}` sur
   `SQUISHY_BIND_ADDR`, volume, healthcheck `sqlcmd -C … -Q "SELECT 1"`), plus
   un one-shot `sqlserver-init` (même image, `depends_on: service_healthy`,
   exécute les fixtures via `sqlcmd -C -i`). Overlay e2e : `ports: !reset []`.
   Makefile : `sqlserver-up`, `sqlserver-cli`, `sqlserver-down`. Aucune boucle
   `until`/`while` nulle part.
10. **Tests** : tests parser (fixture + Northwind + AdventureWorks parsés
    sans erreur), tests `translate` par règle, `test/integration/sqlserver_e2e_test.go`
    opt-in `SQUISHY_E2E_SQLSERVER=1` (comme DB2) câblé dans `run_e2e.sh`.
11. **Campagne** (`/squishy-migrate sqlserver <dump>` via le MCP `squishy`) :
    fixture → Northwind → AdventureWorks (→ pubs), boucle « corriger le Go
    jusqu'à zéro warning / zéro step en échec » ; `SKILL.md` du skill mis à
    jour (kind `sqlserver`, chargement des dumps, mot de passe SA).

## 3. Phases du workflow (agents Opus 5.5)

| Phase | Agents | Fichiers possédés |
|---|---|---|
| 1. Fondations (parallèle) | **A1 infra+dumps+fixtures** ; **A2 parser `tsql` + nœuds AST** ; **A3 plomberie** (connection, discover, inspect, dataxfer, worker ident, storage migration, httpapi hors import blanc, main.go, mcp, web) ; puis **A4 intégration** (`make test`, compile, commit) | A1 : compose, Makefile, scripts/, dumps/README.md, test/fixtures/sqlserver_*.sql, .claude/skills/squishy-migrate/SKILL.md, CLAUDE.md, README.md — A2 : internal/dialects/tsql/**, internal/sqlparse/ast/** — A3 : le reste listé |
| 2. Traduction | **B1 cœur** (translator.go, plan.go, planner, prereqs, types_map_tsql.go, dialect_hooks.go, pgast trigger, import blanc httpapi) ; puis en parallèle **B2 visiteurs expr/DML + shim** (tsql_visitors.go, tsql_shim.go, postgres-init) et **B3 corps procéduraux + routines** (tsql_routines.go, enregistrement des hooks) ; puis **B4 intégration** | fichiers nouveaux disjoints, translator.go uniquement par B1 |
| 3. Campagne (séquentiel) | **C1** fixture + Northwind + e2e opt-in ; **C2** AdventureWorks (+ pubs) | tout le Go, un agent à la fois |
| 4. Revue adverse | **R1** conformité AST-only/regex + garde, **R2** sémantique T-SQL↔PG (types, NULL, dates, LIKE, triggers), **R3** sécurité (injection dans le DDL émis, `DollarQuote`, quoting) — lecture seule, en parallèle ; puis **R4** applique les findings confirmés, `make test` + `make e2e` (`SQUISHY_E2E_SQLSERVER=1`), commit | — |

Critères de fin : `plan_migration` sans warning (hors `info`) et run
`succeeded` avec `rows_done == rows_total` sur Northwind et AdventureWorks ;
`make test` vert ; garde AST-only verte ; aucun `regexp`/`strings.*`-sur-SQL
dans le nouveau code. Commits locaux sur `feat/sqlserver-dialect`, pas de push.
