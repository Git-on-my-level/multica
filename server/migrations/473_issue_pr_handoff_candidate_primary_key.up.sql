DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'issue_pr_handoff_candidate_pkey'
  ) THEN
    ALTER TABLE issue_pr_handoff_candidate
    ADD CONSTRAINT issue_pr_handoff_candidate_pkey
    PRIMARY KEY USING INDEX issue_pr_handoff_candidate_id_uidx;
  END IF;
END $$;
