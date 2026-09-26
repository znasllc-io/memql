-- Existing attribution fields are preserved: clearing them could erase an
-- explicit operator decision made after this repair. Membership attribution
-- is new in this release, so retire only that concept's field for older code.
UPDATE "MemoryNodes"
SET payload = payload - 'accountId'
WHERE concept = 'v1:identity:groupMembership' AND payload ? 'accountId';
