UPDATE session
SET selected_model = CASE
    WHEN selected_model = '' THEN ''
    WHEN selected_provider = '' THEN selected_model
    ELSE selected_provider || '/' || selected_model
END || CASE
    WHEN selected_model <> '' AND selected_variant <> '' THEN '/' || selected_variant
    ELSE ''
END;

ALTER TABLE session DROP COLUMN selected_provider;
ALTER TABLE session DROP COLUMN selected_variant;
