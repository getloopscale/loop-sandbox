-- 0016_deployment_identity.sql: one random id per database.
--
-- Every process of a deployment reads the same id and labels the runtime
-- resources it creates with it. The orphan reaper only touches resources that
-- carry the id of its own deployment, so two deployments that share a container
-- host do not destroy each other's sandboxes.
CREATE TABLE IF NOT EXISTS deployment_identity (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    id        TEXT    NOT NULL
);
INSERT INTO deployment_identity (id) VALUES (gen_random_uuid()::text) ON CONFLICT (singleton) DO NOTHING;
