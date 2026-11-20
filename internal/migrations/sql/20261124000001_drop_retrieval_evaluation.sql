-- The retrieval evaluation framework is gone: its golden question sets,
-- relevance labels, triage passes, and runs, and everything kept for them.
-- admin_audit_log keeps its entries: each records what an administrator did.

-- One statement, because eval_questions and eval_question_triage reference
-- each other.
DROP TABLE eval_question_triage, eval_question_triage_runs, eval_pool_candidates,
           eval_judgments, eval_label_runs, eval_runs, eval_golden_set_splits,
           eval_questions;
DROP FUNCTION eval_reject_split_reassignment();
DROP FUNCTION eval_reject_split_manifest_mutation();

-- The title-and-body hash that relevance labels recorded.
ALTER TABLE events DROP COLUMN eval_content_hash;

-- The jobs of the three kinds only evaluation started.
DELETE FROM admin_jobs WHERE kind IN ('eval_retrieval', 'eval_judge', 'eval_triage');

-- The model chosen for grading relevance labels is no longer a role.
UPDATE connector_settings
   SET settings = settings #- '{roles,eval_label_judge}', version = version + 1
 WHERE connector = 'models' AND settings -> 'roles' ? 'eval_label_judge';

-- The last models test no longer covers that role either; it passed if
-- every remaining role did. A test of that role alone is forgotten.
UPDATE connector_settings
   SET settings = CASE WHEN kept.roles = '[]'::jsonb THEN settings - 'models_test'
                       ELSE jsonb_set(settings, '{models_test}', (settings -> 'models_test') || jsonb_build_object(
                                'roles', kept.roles, 'passed', kept.passed)) END,
       version = version + 1
  FROM (SELECT COALESCE(jsonb_agg(role ORDER BY position), '[]'::jsonb) AS roles,
               COALESCE(bool_and((role ->> 'passed')::boolean), false) AS passed
          FROM connector_settings AS org,
               jsonb_array_elements(CASE WHEN jsonb_typeof(org.settings #> '{models_test,roles}') = 'array'
                                         THEN org.settings #> '{models_test,roles}' ELSE '[]'::jsonb END)
                   WITH ORDINALITY AS tested(role, position)
         WHERE org.connector = 'org'
           AND role ->> 'role' <> 'Evaluation grading') AS kept
 WHERE connector = 'org'
   AND settings #> '{models_test,roles}' @> '[{"role": "Evaluation grading"}]';
