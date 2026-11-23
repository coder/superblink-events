DELETE FROM faucet_owners WHERE quarantined_at IS NOT NULL;
DROP INDEX faucet_owners_login;
CREATE UNIQUE INDEX faucet_owners_login ON faucet_owners (lower(login));
ALTER TABLE faucet_owners DROP COLUMN quarantined_at;
