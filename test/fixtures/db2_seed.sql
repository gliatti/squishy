-- Seed data for the SQUISHY schema. Sized to exercise copy_table batching
-- without bloating the e2e cycle (DB2 boot already costs 4 min).
SET CURRENT SCHEMA = SQUISHY;

INSERT INTO SQUISHY.COUNTRIES (COUNTRY_ID, ISO_CODE, NAME, POPULATION) VALUES
    (1, 'FR', 'France',         67750000),
    (2, 'DE', 'Germany',        83240000),
    (3, 'IT', 'Italy',          59257000),
    (4, 'US', 'United States', 333287000),
    (5, 'JP', 'Japan',          125700000);

INSERT INTO SQUISHY.CUSTOMERS (FIRST_NAME, LAST_NAME, EMAIL, BIRTH_DATE, BALANCE, IS_ACTIVE, COUNTRY_ID) VALUES
    ('Alice',   'Martin',  'alice@example.com',   DATE '1985-03-12', 1500.00, TRUE,  1),
    ('Bob',     'Schmidt', 'bob@example.com',     DATE '1979-11-05', 2750.50, TRUE,  2),
    ('Carla',   'Rossi',   'carla@example.com',   DATE '1992-07-22',   800.00, FALSE, 3),
    ('Daniel',  'Smith',   'daniel@example.com',  DATE '1988-01-30', 4200.75, TRUE,  4),
    ('Emi',     'Tanaka',  'emi@example.com',     DATE '1995-09-14',     0.00, TRUE,  5);

INSERT INTO SQUISHY.ORDERS (CUSTOMER_ID, ORDER_DATE, AMOUNT, NOTES, STATUS) VALUES
    (1, TIMESTAMP '2026-01-15 10:00:00', 199.99, VARGRAPHIC('first order'),  'CL'),
    (1, TIMESTAMP '2026-02-04 14:21:11',  49.50, VARGRAPHIC('addon'),         'CL'),
    (2, TIMESTAMP '2026-01-18 09:05:32', 999.00, VARGRAPHIC('annual order'),  'PR'),
    (3, TIMESTAMP '2026-03-01 11:11:11',  19.99, NULL,                        'CN'),
    (4, TIMESTAMP '2026-03-12 16:42:00', 250.00, VARGRAPHIC('rush'),          'PR');

INSERT INTO SQUISHY.ORDER_LINES (ORDER_ID, LINE_NO, PRODUCT_SKU, QTY, UNIT_PRICE) VALUES
    (1, 1, 'SKU-A', 1, 199.9900),
    (2, 1, 'SKU-B', 5,   9.9000),
    (3, 1, 'SKU-C', 1, 999.0000),
    (4, 1, 'SKU-A', 1,  19.9900),
    (5, 1, 'SKU-D', 2, 125.0000);

COMMIT;
