CREATE TEMP TABLE "MemoryNodes" (id text, concept text, payload jsonb, fixture_order integer GENERATED ALWAYS AS IDENTITY) ON COMMIT DROP;
INSERT INTO "MemoryNodes" (id, concept, payload) VALUES
('v1:platform:site:portal', 'v1:platform:site', '{"systemOwned":true,"bundleRef":"file:///app/portal"}'),
('v1:platform:site:portal', 'v1:platform:site', '{"systemOwned":true,"bundleRef":"file:///app/portal","ownerUserId":"","title":"Old version"}'),
('v1:platform:site:os', 'v1:platform:site', '{"systemOwned":true,"bundleRef":"file:///app/os"}'),
('v1:platform:site:customer', 'v1:platform:site', '{"systemOwned":true,"bundleRef":"file:///app/portal"}'),
('v1:platform:site:portal', 'v1:customer:site', '{"systemOwned":true,"bundleRef":"file:///app/portal"}'),
('v1:platform:site:portal', 'v1:platform:site', '{"systemOwned":false,"bundleRef":"file:///app/portal"}'),
('v1:platform:site:portal', 'v1:platform:site', '{"bundleRef":"file:///app/portal"}'),
('v1:platform:site:portal', 'v1:platform:site', '{"systemOwned":true,"bundleRef":"file:///app/customer"}'),
('v1:platform:site:portal', 'v1:platform:site', '{"systemOwned":true,"bundleRef":"file:///app/portal","ownerUserId":"customer"}');
-- Save the independent expected result, not the migration's WHERE clause.
CREATE TEMP TABLE expected_sites ON COMMIT DROP AS
SELECT * FROM "MemoryNodes" WHERE fixture_order > 2;
