-- A caller-declared lease can outlast the generic claim retention window.
ALTER TABLE automation_execution_claims ADD COLUMN IF NOT EXISTS expires_at timestamptz;
