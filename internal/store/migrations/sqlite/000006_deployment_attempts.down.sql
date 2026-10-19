-- Reverses 000006_deployment_attempts.up.sql. Failed attempts are diagnostic
-- data, not provenance; dropping them loses history nobody else depends on.
DROP INDEX IF EXISTS deployment_attempts_network_alias_created_at_idx;

DROP TABLE IF EXISTS deployment_attempts;
