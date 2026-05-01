# Mission : migrer mysql → PostgreSQL via squishy, corriger le code au besoin, jusqu'à zéro erreur ni warning

Tu es sur le repo squishy (C:\Users\robin\Documents\git\gitlab.com\dalibo\squishy).
Les instructions projet sont dans CLAUDE.md ; lis-le avant de commencer.

Tu disposes du MCP `squishy` (outils `mcp__squishy__*`, 21 outils, backend sur localhost:8080).
Le stack tourne déjà (`make up` démarre postgres + mysql-sample + api + web). Si
`mysql-sample` est down, relance-le avec `make up` et vérifie qu'il est healthy
avec `docker compose ps mysql-sample` avant d'aller plus loin.

## Contexte source/cible

- Source : MySQL (service `mysql-sample`, user=squishy, password=squishy,
  database=sample). Kind de connexion : "mysql". Tu peux aussi ouvrir un client
  CLI avec `make mysql` pour inspecter le schéma source si besoin.
- Cible : la base PG applicative elle-même (host=postgres, port=5432, db=squishy,
  user=squishy, password=squishy), schéma cible `mig` (convention squishy,
  `make reset-dest` wipe ce schéma sans toucher au schéma `squishy`).
- Crée un projet dédié via `create_project` (slug="mysql") ou réutilise un
  projet existant — fais comme tu le sens.

## Workflow (boucle principale, budget : 15 itérations max)

À chaque itération :

1. Assure-toi que la connexion source et la connexion target sont configurées
   (`set_connection` role=source kind=mysql …, role=target kind=postgres …)
   puis valide avec `test_connection` pour les deux rôles.
2. Appelle `inspect_project` puis `plan_project` sur le project_id.
3. Lis la réponse de `plan_project` : elle contient `warnings` (parsing
   errors, types non mappés, prérequis, features MySQL non supportées) et
   `prerequisites`.
4. Si `warnings` est non vide OU si un prerequisite bloquant ne vient pas
   d'une limitation intrinsèque Postgres (ex: "installer pgcron" est
   acceptable, "type SET non mappé" ou "ENUM non géré" doit être corrigé) :
   - Identifie le fichier Go responsable. Points d'entrée :
     * Parser MySQL : `internal/dialects/mysql/` (lexer.go, parser.go, token.go)
     * Parser MariaDB (souvent partagé) : `internal/dialects/mariadb/`
     * AST agnostique : `internal/sqlparse/ast/`
     * Traduction vers PG : `internal/translate/` (mapping types, colonnes, contraintes)
     * Introspection : `internal/inspect/mysql.go`
     * Grammaire de référence : `internal/dialects/mysql/reference/*.g4` (spec,
       pas de runtime ANTLR — les parseurs sont écrits à la main)
   - Édite le code pour traiter le cas. L'API tourne avec `go run` dans
     un conteneur — fais `docker compose restart api` pour recompiler.
   - Si tu ajoutes un mapping de type, ajoute aussi un test unitaire sous
     `internal/translate/` ou `internal/dialects/mysql/`.
   - Lance `make test` (dockerisé, ~30s) pour t'assurer que rien n'est cassé.
   - Reviens à l'étape 2.
5. Si `plan_project` est clean (warnings=[], prérequis acquittables) :
   - Si des prérequis de type "blocking" restent, lis-les et acquitte-les
     avec `ack_prerequisites` seulement s'ils décrivent une action opérationnelle
     légitime (install d'extension PG, etc.) — jamais pour masquer un bug.
   - Appelle `start_run` avec `mode: "auto"`.
6. Poll `get_run` toutes les 5 secondes (max ~10 min) jusqu'à status in
   (succeeded, failed). Complète avec `list_steps` pour voir les erreurs
   par step.
7. Si la run échoue :
   - Lis le champ `error` et `last_error` des steps failed, et si c'est un
     step `copy_table`, appelle `list_batches` pour identifier le batch
     fautif.
   - La cause est probablement dans `internal/dataxfer/` (COPY FROM STDIN,
     mapping de valeurs MySQL → PG par type — attention aux `DATETIME` zéro,
     `TINYINT(1)` → bool, `JSON`, `BIT`, `BLOB`, collations) ou dans
     `internal/worker/` (handlers de jobs). Édite, fais
     `docker compose restart api` pour recompiler, puis `retry_run` ou
     `replay_step` sur le step problématique.
   - Reviens à l'étape 6.
8. Avant de re-planifier après correction, si le schéma cible a été
   partiellement écrit : `make reset-dest` pour repartir propre.

## Critères de succès

La mission est terminée quand :
- `plan_project` renvoie `warnings: []` (ou uniquement des warnings
  informatifs `severity: "info"`) ET `prerequisites` ne contient que des
  items acquittés ou non-bloquants.
- `get_run` renvoie `status: "succeeded"` avec `steps_failed: 0` et
  `rows_done == rows_total`.
- `make test` passe.

## Contraintes

- Ne touche JAMAIS au schéma `squishy` de l'app DB. Seul `mig` est
  wipable (`make reset-dest`).
- N'altère pas la grammaire `.g4` de référence — c'est de la spec.
- Ne supprime pas les tests existants pour faire passer la migration ;
  mets à jour ou ajoute, ne masque pas.
- Si tu atteins le budget de 15 itérations sans converger, arrête-toi
  et fais un rapport structuré : ce qui marche, ce qui ne marche pas,
  warnings/erreurs restants, hypothèse de cause, et ce que tu tenterais
  ensuite. Ne fabrique pas un faux succès.
- Commits : ne commit rien sans que je te le demande.

## Au démarrage

Fais un bref plan d'attaque (3-5 lignes) puis lance-toi. Pas besoin de me
demander confirmation entre les itérations — travaille en autonomie et
rapporte à la fin.
