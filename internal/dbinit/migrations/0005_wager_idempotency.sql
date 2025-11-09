-- Add an idempotency key to wagers so the same submit can't be applied twice.
alter table wagers add column if not exists idempotency_key text;

-- Existing rows (if any) get a generated value:
update wagers set idempotency_key = encode(gen_random_bytes(16), 'hex') where idempotency_key is null;

-- Now enforce uniqueness per user:
do $$
begin
  if not exists (
    select 1
    from pg_indexes
    where schemaname = 'public'
      and indexname = 'uq_wager_user_idemp'
  ) then
    create unique index uq_wager_user_idemp on wagers (user_id, idempotency_key);
  end if;
end$$;

-- Make it required for all future inserts:
alter table wagers alter column idempotency_key set not null;
