-- Record the constructor arguments a deploy actually used.
--
-- Stored in the same typed JSON form as soroforge.yaml and --args-file, so a
-- deployment can be reproduced after the config has moved on. NULL for
-- upgrades, which do not take constructor arguments, and for deploys that used
-- none.

ALTER TABLE deployments ADD COLUMN IF NOT EXISTS constructor_args TEXT;
