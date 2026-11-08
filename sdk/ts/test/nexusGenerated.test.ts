import * as queries from "../src/client/generated_queries.js";
import { buildTrainingChunksBrowse } from "../src/client/generated_builtins.js";
import test from "node:test";
import assert from "node:assert/strict";
import {
  buildWorkGoalsBrowse, buildWorkGoalsBrowseOldest, buildWorkGoalSummaryForOwner,
  buildWorkGoalRunStatuses, buildWorkRunsBrowse, buildWorkRunsBrowseOldest,
  buildWorkRunSummariesForGoal, buildWorkApprovalsBrowse, buildWorkApprovalsForRuns,
  buildWorkRunSummariesByIds, buildNexusAutomationsBrowse, buildNexusAutomationSummaryById,
} from "../src/client/generated_queries.js";

// Mocked query methods cannot detect generated builders discarding arguments.
test("Nexus generated queries preserve search, facets and selected identities", () => {
  for (const build of [buildWorkGoalsBrowse, buildWorkGoalsBrowseOldest]) {
    const call = build({ search: "needle", includePipelines: true });
    assert.match(call, /search: "needle"/);
    assert.match(call, /includePipelines: true/);
  }
  for (const build of [buildWorkRunsBrowse, buildWorkRunsBrowseOldest]) {
    const call = build({ search: "needle", showFinished: true, conversationId: "conversation" });
    assert.match(call, /search: "needle"/);
    assert.match(call, /showFinished: true/);
    assert.match(call, /conversationId: "conversation"/);
  }
  assert.match(buildWorkGoalSummaryForOwner({ goalId: "selected" }), /goalId: "selected"/);
  assert.match(buildWorkGoalRunStatuses({ goalIds: ["visible"] }), /goalIds: \["visible"\]/);
  assert.match(buildWorkRunSummariesForGoal({ goalId: "selected" }), /goalId: "selected"/);
  assert.match(buildWorkApprovalsForRuns({ runIds: ["visible"] }), /runIds: \["visible"\]/);
  assert.match(buildWorkRunSummariesByIds({ runIds: ["visible"] }), /runIds: \["visible"\]/);
  assert.match(buildNexusAutomationSummaryById({ constructId: "selected" }), /constructId: "selected"/);
  const approvals = buildWorkApprovalsBrowse({ search: "More access", kindLabels: ["scopeElevation"], repositoryLabel: false, failureLabel: true });
  assert.match(approvals, /search: "More access"/);
  assert.match(approvals, /kindLabels: \["scopeElevation"\]/);
  assert.match(approvals, /repositoryLabel: false/);
  assert.match(approvals, /failureLabel: true/);
  const automations = buildNexusAutomationsBrowse({ search: "needle", origin: "learned", reuse: "proven", statusWords: ["active"], includeLearned: true, offLadder: true });
  for (const field of ["search", "origin", "reuse", "statusWords", "includeLearned", "offLadder"]) assert.match(automations, new RegExp(field + ": "));
});


test("resource directory builders retain search, facets and scoped detail arguments", () => {
  const calls: Array<[string, string[]]> = [
    [queries.buildTrainingAnalysisRunsForFiles({ fileIds: ["file"] }), ['fileIds: ["file"]']],
    [queries.buildWorkTrainingLessonHistory({ lessonId: "lesson" }), ['lessonId: "lesson"']],
    [queries.buildWorkTrainingSelectionsForLesson({ lessonId: "lesson" }), ['lessonId: "lesson"']],
    [queries.buildWorkTrainingDeliveryForSelections({ observationIds: ["observation"] }), ['observationIds: ["observation"]']],
    [queries.buildWorkSkillSelectionsForSkill({ skillId: "skill" }), ['skillId: "skill"']],
    [queries.buildTrainingFilesBrowse({ search: "needle" }), ['search: "needle"']],
    [queries.buildDocumentChunksForDomain({ domainId: "domain" }), ['domainId: "domain"']],
    [buildTrainingChunksBrowse({ search: "needle", pending: true, cursor: "next" }), ['search: "needle"', 'pending: true', 'cursor: "next"']],
    [queries.buildAllAgents({ search: "needle", includeInactive: true }), ['search: "needle"', 'includeInactive: true']],
    [queries.buildAuditEventsBrowse({ search: "needle", category: "access" }), ['search: "needle"', 'category: "access"']],
    [queries.buildOutboxDeadLettersBrowse({ search: "needle", target: "target" }), ['search: "needle"', 'target: "target"']],
  ];
  for (const [call, expected] of calls) for (const fragment of expected) assert.ok(call.includes(fragment), `${call} omitted ${fragment}`);
});
