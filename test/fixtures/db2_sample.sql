-- IBM DB2 11.5 LUW sample DDL — exercises the squishy parser/translator on
-- the type catalogue, identity columns, generated columns, constraints,
-- views, sequences and SQL PL routines.
--
-- This file is mounted at /var/custom/01-schema.sql by the db2-sample
-- container (see docker-compose.yml). It runs as db2inst1 against the
-- SAMPLE database.

-- A dedicated schema for the migration source; CURRENT_SCHEMA is set per
-- session by squishy via the CLI keyword (cmd/squishy/main.go).
CREATE SCHEMA SQUISHY;
SET CURRENT SCHEMA = SQUISHY;

-- --------------------------------------------------------------------------
-- COUNTRIES — small reference table, INTEGER PK, VARCHAR + CHAR FOR BIT DATA
-- --------------------------------------------------------------------------
CREATE TABLE SQUISHY.COUNTRIES (
    COUNTRY_ID    INTEGER NOT NULL,
    ISO_CODE      CHAR(2)  FOR BIT DATA NOT NULL,
    NAME          VARCHAR(80) NOT NULL,
    POPULATION    BIGINT,
    CONSTRAINT PK_COUNTRIES PRIMARY KEY (COUNTRY_ID),
    CONSTRAINT UQ_COUNTRIES_ISO UNIQUE (ISO_CODE)
);

-- --------------------------------------------------------------------------
-- CUSTOMERS — IDENTITY column, DECIMAL, DATE, TIMESTAMP, BOOLEAN, FK
-- --------------------------------------------------------------------------
CREATE TABLE SQUISHY.CUSTOMERS (
    CUSTOMER_ID  INTEGER GENERATED ALWAYS AS IDENTITY (START WITH 1 INCREMENT BY 1) NOT NULL,
    FIRST_NAME   VARCHAR(40) NOT NULL,
    LAST_NAME    VARCHAR(40) NOT NULL,
    EMAIL        VARCHAR(120),
    BIRTH_DATE   DATE,
    BALANCE      DECIMAL(12,2) DEFAULT 0,
    IS_ACTIVE    BOOLEAN DEFAULT TRUE,
    CREATED_AT   TIMESTAMP(6) NOT NULL WITH DEFAULT CURRENT TIMESTAMP,
    COUNTRY_ID   INTEGER,
    CONSTRAINT PK_CUSTOMERS PRIMARY KEY (CUSTOMER_ID),
    CONSTRAINT FK_CUST_COUNTRY FOREIGN KEY (COUNTRY_ID)
        REFERENCES SQUISHY.COUNTRIES (COUNTRY_ID) ON DELETE NO ACTION
);

-- --------------------------------------------------------------------------
-- ORDERS — DECFLOAT, GRAPHIC strings, BLOB, FK chain
-- --------------------------------------------------------------------------
CREATE TABLE SQUISHY.ORDERS (
    ORDER_ID     BIGINT GENERATED ALWAYS AS IDENTITY NOT NULL,
    CUSTOMER_ID  INTEGER NOT NULL,
    ORDER_DATE   TIMESTAMP NOT NULL,
    AMOUNT       DECFLOAT(34) NOT NULL,
    NOTES        VARGRAPHIC(200),
    INVOICE_PDF  BLOB(2M),
    STATUS       CHAR(2) NOT NULL,
    CONSTRAINT PK_ORDERS PRIMARY KEY (ORDER_ID),
    CONSTRAINT CK_ORDERS_STATUS CHECK (STATUS IN ('NW','PR','CL','CN')),
    CONSTRAINT FK_ORDERS_CUST FOREIGN KEY (CUSTOMER_ID)
        REFERENCES SQUISHY.CUSTOMERS (CUSTOMER_ID) ON DELETE CASCADE
);

CREATE INDEX SQUISHY.IDX_ORDERS_CUST ON SQUISHY.ORDERS (CUSTOMER_ID, ORDER_DATE DESC);

-- --------------------------------------------------------------------------
-- ORDER_LINES — composite PK, FK to ORDERS, NUMERIC + smallint
-- --------------------------------------------------------------------------
CREATE TABLE SQUISHY.ORDER_LINES (
    ORDER_ID     BIGINT NOT NULL,
    LINE_NO      SMALLINT NOT NULL,
    PRODUCT_SKU  VARCHAR(32) NOT NULL,
    QTY          INTEGER NOT NULL,
    UNIT_PRICE   DECIMAL(10,4) NOT NULL,
    CONSTRAINT PK_ORDER_LINES PRIMARY KEY (ORDER_ID, LINE_NO),
    CONSTRAINT FK_LINES_ORDERS FOREIGN KEY (ORDER_ID)
        REFERENCES SQUISHY.ORDERS (ORDER_ID) ON DELETE CASCADE
);

-- --------------------------------------------------------------------------
-- AUDIT_LOG — XML column, ROWID surrogate, exercises rare types
-- --------------------------------------------------------------------------
CREATE TABLE SQUISHY.AUDIT_LOG (
    AUDIT_ID     ROWID NOT NULL,
    AT_TS        TIMESTAMP(6) WITH TIME ZONE NOT NULL,
    PAYLOAD      XML,
    SUMMARY      CLOB(64K)
);

-- --------------------------------------------------------------------------
-- SEQUENCE
-- --------------------------------------------------------------------------
CREATE SEQUENCE SQUISHY.ORDER_SEQ AS BIGINT
    START WITH 1000 INCREMENT BY 1 NO CYCLE NO CACHE;

-- --------------------------------------------------------------------------
-- VIEW
-- --------------------------------------------------------------------------
CREATE VIEW SQUISHY.V_CUSTOMER_TOTALS AS
    SELECT C.CUSTOMER_ID, C.FIRST_NAME, C.LAST_NAME,
           COALESCE(SUM(O.AMOUNT), 0) AS TOTAL_AMOUNT
      FROM SQUISHY.CUSTOMERS C
      LEFT JOIN SQUISHY.ORDERS O ON O.CUSTOMER_ID = C.CUSTOMER_ID
     GROUP BY C.CUSTOMER_ID, C.FIRST_NAME, C.LAST_NAME;

-- --------------------------------------------------------------------------
-- SQL PL PROCEDURE — BEGIN ATOMIC + control flow + handler
-- --------------------------------------------------------------------------
--#SET TERMINATOR @
CREATE PROCEDURE SQUISHY.GIVE_BONUS (IN p_customer_id INTEGER, IN p_amount DECIMAL(10,2))
LANGUAGE SQL
MODIFIES SQL DATA
BEGIN ATOMIC
    DECLARE v_balance DECIMAL(12,2);
    DECLARE EXIT HANDLER FOR SQLEXCEPTION
        RESIGNAL SQLSTATE '38001' SET MESSAGE_TEXT = 'give_bonus failed';

    SELECT BALANCE INTO v_balance FROM SQUISHY.CUSTOMERS
        WHERE CUSTOMER_ID = p_customer_id;
    IF v_balance IS NULL THEN
        SIGNAL SQLSTATE '02000' SET MESSAGE_TEXT = 'customer not found';
    END IF;
    UPDATE SQUISHY.CUSTOMERS SET BALANCE = v_balance + p_amount
        WHERE CUSTOMER_ID = p_customer_id;
END@
--#SET TERMINATOR ;
