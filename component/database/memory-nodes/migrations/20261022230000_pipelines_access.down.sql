-- Access decisions may have changed since migration. Never restore retired
-- grants or erase a person's newer decision during rollback.
SELECT 1;
