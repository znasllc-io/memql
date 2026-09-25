-- Older single-recipient sends stored the rule id in campaignId as well as
-- emailRuleId. Repair only that provably false current relationship: the rule
-- must exist and no real campaign may have that id. Historical payloads and
-- genuinely distinct campaign/rule references remain untouched.
WITH latest AS (
  SELECT DISTINCT ON (concept, id) concept, id, "createdAt", payload
  FROM "MemoryNodes"
  WHERE concept IN ('v1:campaigns:delivery', 'v1:campaigns:emailRule', 'v1:campaigns:campaign')
  ORDER BY concept, id, "createdAt" DESC
), repairs AS (
  SELECT child.id, child."createdAt"
  FROM latest child
  WHERE child.concept = 'v1:campaigns:delivery'
    AND COALESCE(child.payload->>'emailRuleId', '') <> ''
    AND regexp_replace(child.payload->>'campaignId', '^v1:campaigns:campaign:', '') =
        regexp_replace(child.payload->>'emailRuleId', '^v1:campaigns:emailRule:', '')
    AND EXISTS (
      SELECT 1 FROM latest rule WHERE rule.concept = 'v1:campaigns:emailRule'
        AND regexp_replace(rule.id, '^v1:campaigns:emailRule:', '') =
            regexp_replace(child.payload->>'emailRuleId', '^v1:campaigns:emailRule:', '')
    )
    AND NOT EXISTS (
      SELECT 1 FROM latest campaign WHERE campaign.concept = 'v1:campaigns:campaign'
        AND regexp_replace(campaign.id, '^v1:campaigns:campaign:', '') =
            regexp_replace(child.payload->>'campaignId', '^v1:campaigns:campaign:', '')
    )
)
UPDATE "MemoryNodes" child SET payload = jsonb_set(child.payload, '{campaignId}', '""'::jsonb, true)
FROM repairs WHERE child.concept = 'v1:campaigns:delivery'
  AND child.id = repairs.id AND child."createdAt" = repairs."createdAt";

-- Repair only derivable current child attribution. Never guess an organization
-- for an untied root, overwrite an explicit child organization, or relabel
-- historical versions. The relationship and both concepts must match; an
-- accountId key alone says nothing about the model a row belongs to.
--
-- Two passes allow consentEvent -> recipient -> audience to settle without
-- depending on UPDATE order. No account/schema fields are removed or made
-- required. Repeating the migration is a no-op after the first repair.
DO $$
DECLARE pass integer;
BEGIN
  FOR pass IN 1..2 LOOP
    WITH relationships(child_concept, parent_concept, parent_field) AS (VALUES
      ('v1:campaigns:recipient', 'v1:campaigns:audience', 'audienceId'),
      ('v1:campaigns:delivery', 'v1:campaigns:campaign', 'campaignId'),
      ('v1:campaigns:delivery', 'v1:campaigns:emailRule', 'emailRuleId'),
      ('v1:campaigns:delivery', 'v1:campaigns:recipient', 'recipientId'),
      ('v1:campaigns:engagementEvent', 'v1:campaigns:campaign', 'campaignId'),
      ('v1:campaigns:consentEvent', 'v1:campaigns:recipient', 'recipientId'),
      ('v1:platform:packageDeployment', 'v1:platform:package', 'packageId'),
      ('v1:platform:customDomain', 'v1:platform:site', 'siteId'),
      ('v1:identity:groupMembership', 'v1:identity:group', 'groupId')
    ), latest AS (
      -- Parameterize one indexed read per concept. A join against the small
      -- relationship CTE otherwise makes PostgreSQL scan the whole history
      -- table before filtering, including unrelated high-volume mesh rows.
      SELECT row.*
      FROM (SELECT child_concept AS concept FROM relationships
            UNION SELECT parent_concept FROM relationships) selected
      CROSS JOIN LATERAL (
        SELECT DISTINCT ON (id) concept, id, "createdAt", payload
        FROM "MemoryNodes"
        WHERE concept = selected.concept
        ORDER BY id, "createdAt" DESC
      ) row
    ), repairs AS (
      SELECT child.concept, child.id, child."createdAt", min(parent.payload->>'accountId') AS account_id
      FROM latest child
      JOIN relationships rel ON child.concept = rel.child_concept
        AND COALESCE(child.payload->>rel.parent_field, '') <> ''
      LEFT JOIN latest parent ON parent.concept = rel.parent_concept
        AND (parent.id = child.payload->>rel.parent_field
          OR parent.id = rel.parent_concept || ':' || (child.payload->>rel.parent_field))
      WHERE COALESCE(child.payload->>'accountId', '') = ''
      GROUP BY child.concept, child.id, child."createdAt"
      -- Every named parent must resolve and agree. In particular, a rule id
      -- colliding with a real campaign id cannot prove either organization.
      HAVING bool_and(COALESCE(parent.payload->>'accountId', '') <> '')
        AND count(DISTINCT regexp_replace(parent.payload->>'accountId', '^v1:accounts:account:', '')) = 1
    )
    UPDATE "MemoryNodes" child
    SET payload = jsonb_set(child.payload, '{accountId}', to_jsonb(repairs.account_id), true)
    FROM repairs
    WHERE child.concept = repairs.concept AND child.id = repairs.id AND child."createdAt" = repairs."createdAt";
  END LOOP;
END $$;
