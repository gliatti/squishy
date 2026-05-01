-- ============================================================================
-- squishy — préparation utilisateur pour Oracle Database 19c
--
-- Exécuté comme SYSDBA dans CDB$ROOT par l'image
-- container-registry.oracle.com/database/enterprise:19.3.0.0 via le répertoire
-- /opt/oracle/scripts/setup/. Doit basculer explicitement dans la PDB
-- ORCLPDB1 et y créer l'utilisateur SQUISHY avec les droits requis par le
-- fixture (tables, vues, MV, séquences, procédures/triggers, types, synonymes,
-- pragma AUTONOMOUS_TRANSACTION).
-- ============================================================================

ALTER SESSION SET CONTAINER = ORCLPDB1;

-- 19c a durci la sécurité : le rôle RESOURCE ne porte plus UNLIMITED
-- TABLESPACE, il faut donc attribuer un quota explicite.
CREATE USER squishy IDENTIFIED BY squishy
  DEFAULT TABLESPACE USERS
  TEMPORARY TABLESPACE TEMP
  QUOTA UNLIMITED ON USERS;

GRANT CONNECT, RESOURCE TO squishy;
GRANT CREATE SESSION,
      CREATE TABLE,
      CREATE VIEW,
      CREATE MATERIALIZED VIEW,
      CREATE SEQUENCE,
      CREATE PROCEDURE,
      CREATE TRIGGER,
      CREATE TYPE,
      CREATE SYNONYM
  TO squishy;

EXIT;
