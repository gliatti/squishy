
-- T-SQL hierarchyid methods. A hierarchyid column is migrated as text
-- holding its canonical path ('/' for the root, '/1/', '/1/3/', '/2.1/'
-- …: what hierarchyid::ToString() prints), so the methods are functions
-- over that text: ToString() is the value itself and hierarchyid::
-- GetRoot() the literal '/'. Every helper walks the path character by
-- character (each '/' closes one level), without any regular
-- expression. They are IMMUTABLE STRICT like the CLR methods (same
-- path, same result; NULL in, NULL out), so a computed column such as
-- `OrganizationNode.GetLevel()` can stay a generated column.

-- tsql.hierarchyid_getlevel(h) — hierarchyid::GetLevel(): the depth of
-- the node, 0 for the root '/'.
CREATE OR REPLACE FUNCTION tsql.hierarchyid_getlevel(h text)
RETURNS smallint
LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE AS $$
DECLARE
    i integer;
    n integer := 0;
BEGIN
    FOR i IN 1 .. length(h) LOOP
        IF substr(h, i, 1) = '/' THEN
            n := n + 1;
        END IF;
    END LOOP;
    RETURN greatest(n - 1, 0)::smallint;
END;
$$;

-- tsql.hierarchyid_getancestor(h, n) — hierarchyid::GetAncestor(n): the
-- node n levels up (h itself for 0), NULL above the root. A negative n
-- is an error in SQL Server as here.
CREATE OR REPLACE FUNCTION tsql.hierarchyid_getancestor(h text, n integer)
RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE AS $$
DECLARE
    i integer;
    seen integer := 0;
BEGIN
    IF n < 0 THEN
        RAISE EXCEPTION 'GetAncestor: the level argument must be >= 0 (got %)', n;
    END IF;
    IF n = 0 THEN
        RETURN h;
    END IF;
    -- The path ends with '/': the ancestor ends at the (n + 1)-th '/'
    -- counted from the end.
    FOR i IN REVERSE length(h) .. 1 LOOP
        IF substr(h, i, 1) = '/' THEN
            seen := seen + 1;
            IF seen = n + 1 THEN
                RETURN substr(h, 1, i);
            END IF;
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;

-- tsql.hierarchyid_isdescendantof(h, p) — hierarchyid::IsDescendantOf(p):
-- whether h is p or below it. Both paths end with '/', so p is an
-- ancestor exactly when it is a prefix of h.
CREATE OR REPLACE FUNCTION tsql.hierarchyid_isdescendantof(h text, p text)
RETURNS boolean
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT length(p) <= length(h) AND substr(h, 1, length(p)) = p
$$;

-- tsql.hierarchyid_getreparentedvalue(h, old_root, new_root) —
-- hierarchyid::GetReparentedValue(old_root, new_root): h with its
-- old_root prefix replaced by new_root.
CREATE OR REPLACE FUNCTION tsql.hierarchyid_getreparentedvalue(h text, old_root text, new_root text)
RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE AS $$
BEGIN
    IF NOT tsql.hierarchyid_isdescendantof(h, old_root) THEN
        RAISE EXCEPTION 'GetReparentedValue: % is not a descendant of %', h, old_root;
    END IF;
    RETURN new_root || substr(h, length(old_root) + 1);
END;
$$;

-- tsql.hierarchyid_lastlabel(h) — the last label of a non-root path
-- ('/1/3/' → '3', '/2.1/' → '2.1'), NULL for the root.
CREATE OR REPLACE FUNCTION tsql.hierarchyid_lastlabel(h text)
RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE AS $$
DECLARE
    i integer;
BEGIN
    IF length(h) < 2 THEN
        RETURN NULL;
    END IF;
    FOR i IN REVERSE length(h) - 1 .. 1 LOOP
        IF substr(h, i, 1) = '/' THEN
            RETURN substr(h, i + 1, length(h) - i - 1);
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;

-- tsql.hierarchyid_isint(s) — whether a label is a plain integer
-- (optional minus sign, then digits).
CREATE OR REPLACE FUNCTION tsql.hierarchyid_isint(s text)
RETURNS boolean
LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE AS $$
DECLARE
    i integer;
    c text;
BEGIN
    IF s = '' OR s = '-' THEN
        RETURN false;
    END IF;
    FOR i IN 1 .. length(s) LOOP
        c := substr(s, i, 1);
        IF NOT (c BETWEEN '0' AND '9' OR (i = 1 AND c = '-')) THEN
            RETURN false;
        END IF;
    END LOOP;
    RETURN true;
END;
$$;

-- tsql.hierarchyid_getdescendant(parent, c1, c2) —
-- hierarchyid::GetDescendant(c1, c2) for children with integer labels:
-- the first child (both NULL), the one after c1, the one before c2, or
-- one between two non-adjacent children. A child between two adjacent
-- labels needs SQL Server's fractional labels ('/1.1/'), which this
-- helper refuses rather than invent.
CREATE OR REPLACE FUNCTION tsql.hierarchyid_getdescendant(parent text, c1 text, c2 text)
RETURNS text
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
DECLARE
    l1 text;
    l2 text;
BEGIN
    IF parent IS NULL THEN
        RETURN NULL;
    END IF;
    IF c1 IS NOT NULL THEN
        IF tsql.hierarchyid_getancestor(c1, 1) IS DISTINCT FROM parent THEN
            RAISE EXCEPTION 'GetDescendant: % is not a child of %', c1, parent;
        END IF;
        l1 := tsql.hierarchyid_lastlabel(c1);
    END IF;
    IF c2 IS NOT NULL THEN
        IF tsql.hierarchyid_getancestor(c2, 1) IS DISTINCT FROM parent THEN
            RAISE EXCEPTION 'GetDescendant: % is not a child of %', c2, parent;
        END IF;
        l2 := tsql.hierarchyid_lastlabel(c2);
    END IF;
    IF (l1 IS NOT NULL AND NOT tsql.hierarchyid_isint(l1)) OR (l2 IS NOT NULL AND NOT tsql.hierarchyid_isint(l2)) THEN
        RAISE EXCEPTION 'GetDescendant: fractional hierarchyid labels are not supported (% / %)', c1, c2;
    END IF;
    IF l1 IS NULL AND l2 IS NULL THEN
        RETURN parent || '1/';
    ELSIF l2 IS NULL THEN
        RETURN parent || (l1::bigint + 1)::text || '/';
    ELSIF l1 IS NULL THEN
        RETURN parent || (l2::bigint - 1)::text || '/';
    ELSIF l1::bigint >= l2::bigint THEN
        RAISE EXCEPTION 'GetDescendant: % must be less than %', c1, c2;
    ELSIF l2::bigint - l1::bigint > 1 THEN
        RETURN parent || (l1::bigint + 1)::text || '/';
    END IF;
    RAISE EXCEPTION 'GetDescendant: no integer label between % and % (SQL Server would use a fractional label)', c1, c2;
END;
$$;
