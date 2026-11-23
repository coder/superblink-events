-- A delivery that names an account under another login than the row holds,
-- or under a login that another account holds, may follow a rename or be an
-- old body sent again. When GitHub, asked for the account, gives no login,
-- the store cannot tell which, so it quarantines the accounts: a row with
-- quarantined_at holds no login, so no name resolves through it, until GitHub
-- gives the account a login. It keeps the login that it held, so that a
-- delivery that names the login asks GitHub before an account takes it.
ALTER TABLE faucet_owners ADD COLUMN quarantined_at timestamptz;
DROP INDEX faucet_owners_login;
CREATE UNIQUE INDEX faucet_owners_login ON faucet_owners (lower(login)) WHERE quarantined_at IS NULL;
