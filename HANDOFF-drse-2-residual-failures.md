# Handoff : 2 échecs résiduels DRSE → PostgreSQL

> Prompt self-contained pour reprendre dans une nouvelle session Claude Code
> et traiter les 2 dernières routines en échec sur la migration Oracle DRSE.

## Contexte

Repo : `C:\Users\robin\Documents\git\gitlab.com\dalibo\squishy`

squishy migre Oracle → PG. La migration DRSE (notice dump) est à
**961/963 steps OK** ; 2 procédures restent en échec, toutes deux sur des
bugs de parser PL/SQL pour des routines >300 lignes avec nesting profond.

- Migration ID : `1fd10220-4bef-4309-a716-0b84a763cfe4`
- Run de référence : `bbae48c0-aee2-4f43-ac62-3f4bdda2b265`
- Stack : `make up` puis `docker compose ps` doit montrer postgres+api+oracle-sample healthy

## Workflow standard à chaque itération

1. Démarrer les services :
   ```bash
   docker compose up -d --wait postgres api oracle-sample
   ```

2. Pour chaque fix :
   - Éditer le code Go (voir fichiers ci-dessous)
   - Tests :
     ```bash
     docker compose --profile test run --rm unit-tests go test ./... -count=1
     ```
   - Restart API + replan + relaunch :
     ```bash
     docker compose restart api && sleep 5
     curl -sS -X POST http://localhost:8080/api/v1/runs/<old-run>/cancel
     curl -sS -X POST http://localhost:8080/api/v1/migrations/1fd10220-4bef-4309-a716-0b84a763cfe4/plan
     curl -sS -X POST http://localhost:8080/api/v1/migrations/1fd10220-4bef-4309-a716-0b84a763cfe4/runs \
       -H 'content-type: application/json' \
       -d '{"mode":"auto","skip_data":true}'
     ```
   - Vérifier les échecs :
     ```bash
     docker compose exec -T postgres bash -c "psql -U squishy -d squishy -t -A -c \"SELECT target || ' :: ' || left(error,200) FROM squishy.steps WHERE run_id='<new-run>' AND status='failed' AND error != '' ORDER BY seq\""
     ```

> **Note** : `skip_data=true` saute le step `create_ddl`. Si la base `notice`
> est vide il faut d'abord appliquer le DDL :
>
> ```bash
> docker compose exec -T postgres psql -U squishy -d postgres -c 'CREATE DATABASE notice;'
> docker compose exec -T postgres psql -U squishy -d notice -c 'CREATE EXTENSION IF NOT EXISTS orafce;'
> docker compose exec -T postgres bash -c "psql -U squishy -d squishy -t -A -c \"SELECT ddl_script FROM squishy.migrations WHERE id='1fd10220-4bef-4309-a716-0b84a763cfe4'\" > /tmp/ddl.sql && psql -U squishy -d notice < /tmp/ddl.sql"
> ```

### Outils de debug utiles

Source Oracle d'une routine donnée :

```bash
docker compose exec -T oracle-sample sqlplus -s system/oracle@FREEPDB1 <<'EOF'
set heading off feedback off pagesize 0 linesize 32767 long 99999999
SELECT TEXT FROM dba_source WHERE owner='DRSE' AND name='<NAME>' ORDER BY line;
exit
EOF
```

DDL générée par squishy (depuis l'app DB) :

```bash
docker compose exec -T postgres bash -c "psql -U squishy -d squishy -t -A -c \"SELECT payload->>'ddl' FROM squishy.steps WHERE id='<step-id>'\""
```

Test PG isolé d'une routine :

```bash
(echo "SET search_path TO \"drse\",\"public\";"; cat /tmp/ddl.sql) \
  | docker compose exec -T postgres psql -U squishy -d notice -v ON_ERROR_STOP=1
```

Récupérer l'ID de step d'une routine :

```bash
docker compose exec -T postgres bash -c "psql -U squishy -d squishy -t -A -c \"SELECT id FROM squishy.steps WHERE run_id='<run-id>' AND target='procedure:PRC_MAJ_JRN'\""
```

---

## Échec #1 : PRC_MAJ_JRN — body tronqué après le 1er LOOP

### Erreur PG

```
syntax error at or near ";"
```

### Symptôme

La procédure Oracle fait 370 lignes avec **9 patterns** de la forme :

```sql
OPEN l_rc_curs FOR
  SELECT ... FROM all_tab_columns WHERE ... ORDER BY COLUMN_ID;
LOOP
  FETCH l_rc_curs INTO L_VC_COLONNE_REQ;
  EXIT WHEN l_rc_curs%NOTFOUND;
  L_VC_STMT := l_vc_stmt || ... || l_vc_colonne_req;
END LOOP;
IF (l_rc_curs%ISOPEN) THEN CLOSE l_rc_curs; END IF;
... (suite procédure : EXECUTE l_vc_stmt; trigger TJU; trigger TJD; etc.)
END PRC_MAJ_JRN;
```

Sur le **dernier** tel pattern, la DDL générée se termine en :

```sql
LOOP
  FETCH l_rc_curs INTO L_VC_COLONNE_REQ;
  EXIT WHEN l_rc_curs%NOTFOUND;
  L_VC_STMT := l_vc_stmt || l || t || t || t || l_vc_colonne_req;
END;        -- <-- BUG : devrait être END LOOP;
END;        -- <-- procedure END
$body$;
```

Tout ce qui suit le LOOP body (le reste de la procédure : ~150 lignes)
est **PERDU**.

### Hypothèse

`parseLoopStmt` ou un parseur amont (probablement `parseOpenStmt` quand il
consomme le `OPEN cur FOR <SELECT> ... ;` avec une SELECT multi-ligne contenant
des sous-requêtes corrélées) avale plus de tokens que prévu, laissant le LOOP
suivant orphelin.

Le writer émet alors `END;` au lieu de `END LOOP;` parce que la statement
reçue est un `Block` (qui émet `END;`) au lieu d'un `LoopStmt` (qui émettrait
`END LOOP;`).

### Fichiers à inspecter

- `internal/dialects/oracle/parser_plsql.go` :
  - `parseLoopStmt()` ligne ~589
  - `parseOpenStmt()` autour de ligne ~407 (`case p.isKw("OPEN")`)
  - le bloc OPEN cur FOR <query> à la ligne 446-494 capture jusqu'à `USING`
    ou fin-de-statement — vérifier que `;` à depth==0 le termine bien
- `internal/dialects/postgres/plpgsql.go` :
  - writer pour `LoopStmt` (émet `END LOOP;`) vs `Block` (émet `END;`)

### Test cible à ajouter

Parser une procédure avec :

```sql
OPEN cur FOR SELECT ... ORDER BY x;
LOOP
  FETCH cur INTO v;
  EXIT WHEN cur%NOTFOUND;
  v2 := v || 'foo';
END LOOP;
suite_de_la_proc();
```

et vérifier que `suite_de_la_proc();` reste dans la sortie du writer + que
le LOOP émet bien `END LOOP;`.

### Source Oracle complète

```bash
docker compose exec -T oracle-sample sqlplus -s system/oracle@FREEPDB1 <<'EOF' > /tmp/prc_maj_jrn.sql
set heading off feedback off pagesize 0 linesize 32767 long 99999999
SELECT TEXT FROM dba_source WHERE owner='DRSE' AND name='PRC_MAJ_JRN' ORDER BY line;
exit
EOF
```

(370 lignes)

---

## Échec #2 : PRC_MAJ_TRC — END IF en trop dans nesting profond

### Erreur PG

```
syntax error at or near "IF"
```

à `END IF;` ligne 259 de la DDL générée.

### Symptôme

La procédure Oracle (1008 lignes) a un FOR cursor LOOP body contenant
**7 IFs imbriqués**. La DDL générée émet **9 END IF** au lieu de 7 — 2 en trop.

Chaque `PLIf` node en AST émet exactement 1 `END IF` dans le writer, donc
l'AST contient 2 `IfStmt` fantômes.

### Source Oracle (extrait pertinent du LOOP body)

```sql
LOOP
  FETCH l_rc_curs INTO L_VC_COLONNE_REQ, L_VC_COL_OLD, L_VC_COL_NEW,
                       L_VC_COL, L_VC_COMMENTS;
  EXIT WHEN l_rc_curs%NOTFOUND;
  L_VC_LST_CP_COL := ... ;
  ...
  SELECT count(*) INTO L_I_NB_COL
    FROM all_tab_columns
   WHERE ...;

  IF (l_i_nb_col = 0 and l_i_nb_tab!=0) THEN          -- IF #1
    L_VC_STMT_ADD := ...;
  END IF;

  IF (l_bo_add_comment = TRUE or l_i_nb_col = 0) THEN -- IF #2
    L_I_INDICE_INDEX := ...;

    IF ((l_tab_index[...] IS NOT NULL) = FALSE) THEN  -- IF #3
      L_TAB_INDEX := array_append(L_TAB_INDEX, NULL);
    END IF;

    L_TAB_INDEX[...] := 'CREATE INDEX ' || ...;

    IF (length(l_vc_comments) > 0) THEN               -- IF #4
      L_I_INDICE_COMMENT := ...;

      IF ((l_tab_commentaire[...] IS NOT NULL) = FALSE) THEN  -- IF #5
        L_TAB_COMMENTAIRE := array_append(L_TAB_COMMENTAIRE, NULL);
      END IF;

      L_TAB_COMMENTAIRE[...] := 'COMMENT ON COLUMN ' || ...;
    END IF;
  END IF;

  IF (l_i_nb_col != 0 and l_i_nb_tab!=0) THEN         -- IF #6
    SELECT count(*) INTO L_I_NB_COL
      FROM all_tab_columns ori, all_tab_columns mvt
     WHERE ...;

    IF (l_i_nb_col != 0 and l_i_nb_tab!=0) THEN       -- IF #7
      L_VC_STMT_MODIFY := ...;
    END IF;
  END IF;
END LOOP;

IF (l_i_nb_tab = 0) THEN
  L_SQLCMD := ...;
END IF;
```

7 IF dans le LOOP body → 7 END IF source. Output émet 9 END IF → 2 fantômes.

### Fichiers à inspecter

- `internal/dialects/oracle/parser_plsql.go` :
  - `parseIfStmt()` ligne ~539
  - `parseStmtsUntilAny()` (retourne dès qu'elle voit END/ELSIF/ELSE)
  - `parseExprUntilKeyword("THEN")` — comportement face à des conditions
    avec parens nestés et comparaison booléenne

### Hypothèse

Un IF sans branch parsé via une condition vide ou un body contenant un
sous-IF mal délimité génère un PLIf supplémentaire. Possiblement quand
`parseExprUntilKeyword("THEN")` capture trop de tokens : la condition
`((l_tab_index[l_i_indice_index] IS NOT NULL) = FALSE)` a des parens
nestés + comparaison booléenne — peut-être que le `THEN` est consommé
prématurément par cette capture.

Une autre piste : `parseStmtsUntilAny("END", ...)` ne descend pas dans les
sous-IF, mais traite chaque `END IF;` rencontré au top level comme un
terminateur. Si un IF interne est mal fermé, l'`END IF;` du IF interne ferme
le IF externe. Le compteur de IF reste alors décalé.

### Test cible à ajouter

Parse-test sur le pattern minimal :

```sql
DECLARE x int; BEGIN
  IF ((arr[i] IS NOT NULL) = FALSE) THEN x := 1; END IF;
END;
```

Vérifier `len(IfStmt.Branches) == 1` et que le writer émet exactement 1 END IF.

Puis test sur le LOOP body complet ci-dessus, vérifier que le total de PLIf
nodes émis correspond aux 7 IF source.

### Source Oracle complète

```bash
docker compose exec -T oracle-sample sqlplus -s system/oracle@FREEPDB1 <<'EOF' > /tmp/prc_maj_trc.sql
set heading off feedback off pagesize 0 linesize 32767 long 99999999
SELECT TEXT FROM dba_source WHERE owner='DRSE' AND name='PRC_MAJ_TRC' ORDER BY line;
exit
EOF
```

(1008 lignes)

---

## Critères de succès

- `make test` (dockerisé) reste vert
- Run `skip_data=true` retourne `steps_failed: 0` :
  ```bash
  curl -sS http://localhost:8080/api/v1/runs/<run-id>
  # → {"status":"succeeded", "steps_failed":0, ...}
  ```
- Tests de régression ajoutés pour les patterns identifiés :
  - LOOP+EXIT WHEN+suite après END LOOP préservée
  - IF avec condition booléenne complexe à parens nestés
  - 7 IF imbriqués dans un LOOP body produisent 7 PLIf nodes en AST

## Contraintes

- Ne pas toucher au schéma `squishy` de l'app DB
- Ne pas altérer la grammaire `internal/dialects/oracle/reference/*.g4` (spec)
- Ne pas supprimer de tests existants pour faire passer la migration
- Pas de commits sans demande explicite
- Budget : ~3-5 itérations par fix

## Fichiers déjà modifiés (29 fixes appliqués cette session)

Tous dans `internal/translate/` et `internal/dialects/oracle/`. Pour le détail,
`git log --oneline -50` une fois les commits faits ; sinon les fixes sont
visibles dans la diff non-commitée.

Notamment : Fix 17 (skip-comment in body walkers), Fix 19 (UPDATE tuple SET),
Fix 21-22 (BULK COLLECT + FETCH LIMIT), Fix 26 (record-of-collection), Fix 28
(synonymes externes émis comme VIEW au lieu de stub TABLE), Fix 29 (CONNECT BY
LEVEL split → string_to_array).
