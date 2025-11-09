do $$
begin
  if not exists (
    select 1 from pg_constraint
    where conname = 'uq_accounts_bet_escrow'
  ) then
    alter table accounts
      add constraint uq_accounts_bet_escrow
      unique (bet_id)
      deferrable initially immediate;
  end if;
end$$;

