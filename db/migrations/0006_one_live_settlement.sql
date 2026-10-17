-- At most one live (pending or committed) settlement per unpaid intent, enforced by the database so
-- concurrent rating passes or relays can never queue a second debit for the same arrears.
CREATE UNIQUE INDEX transfer_intent_one_live_settlement ON transfer_intent (settles)
  WHERE settles IS NOT NULL AND status IN ('pending','committed');
