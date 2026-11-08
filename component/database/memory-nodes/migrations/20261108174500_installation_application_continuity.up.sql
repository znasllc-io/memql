-- Application CRDs without a status subresource advance generation for status
-- writes. Retain the authenticated watch cursor under the existing preparation
-- owner/head locks so a replica change cannot erase an intervening real edit.
ALTER TABLE installation_preparations
    ADD COLUMN application_continuity jsonb NOT NULL DEFAULT '{}'::jsonb;
