
-- tsql.like(subject, pattern [, escape_char]) — T-SQL LIKE:
--   %        any run of characters (possibly empty)
--   _        exactly one character
--   [abc]    one character of the set; [a-f] a range; [^a-f] any other
--   escape   the character after escape_char is literal
-- Case-insensitive (SQL Server's default collation): subject and pattern
-- are compared through lower(). Trailing blanks of the subject are
-- ignored, as SQL Server ignores them. A '[' without a closing ']' is a
-- literal character; a ']' right after '[' or '[^' is a member of the
-- set. Matching is the classic wildcard scan with one backtrack point
-- (the last %), character by character.
CREATE OR REPLACE FUNCTION tsql.like(subject text, pattern text, escape_char text)
RETURNS boolean
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
DECLARE
    s text;
    p text;
    esc text;
    slen integer;
    plen integer;
    si integer := 1;
    pp integer := 1;
    star_p integer := 0;
    star_s integer := 0;
    c text;
    sc text;
    lo text;
    hi text;
    j integer;
    k integer;
    close_at integer;
    neg boolean;
    hit boolean;
    matched boolean;
    is_star boolean;
    adv integer;
BEGIN
    IF subject IS NULL OR pattern IS NULL THEN
        RETURN NULL;
    END IF;
    s := rtrim(lower(subject), ' ');
    p := lower(pattern);
    esc := lower(escape_char);
    IF esc = '' THEN
        esc := NULL;
    END IF;
    slen := length(s);
    plen := length(p);
    WHILE si <= slen LOOP
        matched := false;
        is_star := false;
        adv := 1;
        IF pp <= plen THEN
            c := substr(p, pp, 1);
            sc := substr(s, si, 1);
            IF esc IS NOT NULL AND c = esc AND pp < plen THEN
                adv := 2;
                matched := sc = substr(p, pp + 1, 1);
            ELSIF c = '%' THEN
                is_star := true;
            ELSIF c = '_' THEN
                matched := true;
            ELSIF c = '[' THEN
                j := pp + 1;
                neg := false;
                IF j <= plen AND substr(p, j, 1) = '^' THEN
                    neg := true;
                    j := j + 1;
                END IF;
                close_at := 0;
                k := j + 1;
                WHILE k <= plen LOOP
                    IF substr(p, k, 1) = ']' THEN
                        close_at := k;
                        EXIT;
                    END IF;
                    k := k + 1;
                END LOOP;
                IF close_at = 0 THEN
                    matched := sc = '[';
                ELSE
                    adv := close_at - pp + 1;
                    hit := false;
                    k := j;
                    WHILE k < close_at LOOP
                        lo := substr(p, k, 1);
                        IF k + 2 < close_at AND substr(p, k + 1, 1) = '-' THEN
                            hi := substr(p, k + 2, 1);
                            IF sc COLLATE "C" >= lo COLLATE "C" AND sc COLLATE "C" <= hi COLLATE "C" THEN
                                hit := true;
                            END IF;
                            k := k + 3;
                        ELSE
                            IF sc = lo THEN
                                hit := true;
                            END IF;
                            k := k + 1;
                        END IF;
                    END LOOP;
                    matched := hit <> neg;
                END IF;
            ELSE
                matched := sc = c;
            END IF;
        END IF;
        IF is_star THEN
            star_p := pp;
            star_s := si;
            pp := pp + 1;
        ELSIF matched THEN
            si := si + 1;
            pp := pp + adv;
        ELSIF star_p > 0 THEN
            star_s := star_s + 1;
            si := star_s;
            pp := star_p + 1;
        ELSE
            RETURN false;
        END IF;
    END LOOP;
    WHILE pp <= plen AND substr(p, pp, 1) = '%' LOOP
        pp := pp + 1;
    END LOOP;
    RETURN pp > plen;
END;
$$;

CREATE OR REPLACE FUNCTION tsql.like(subject text, pattern text)
RETURNS boolean
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT tsql.like(subject, pattern, NULL::text);
$$;
