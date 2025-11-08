-- 0002_txhash_and_nonneg.sql

-- 1) Update append-only guard to allow ONLY the hash trigger to update transactions
create or replace function forbid_mutations() returns trigger as $$
begin
  if tg_op in ('UPDATE','DELETE') then
    -- Allow a scoped UPDATE on transactions when the hash trigger sets a flag.
    if tg_table_name = 'transactions'
       and tg_op = 'UPDATE'
       and current_setting('bets.allow_tx_hash_update', true) = '1' then
      return NEW;
    end if;

    raise exception 'Append-only table: % not allowed on %', tg_op, tg_table_name;
  end if;
  return NEW;
end;
$$ language plpgsql;

-- 2) Wrap the hash UPDATE with the scoped flag
create or replace function tx_compute_hash_deferred() returns trigger as $$
declare
  payload bytea;
  entries_str text;
  prev bytea;
begin
  -- previous hash (by created_at then id for tie-break)
  select t.hash into prev
  from transactions t
  where (t.created_at, t.id) < (NEW.created_at, NEW.id)
  order by t.created_at desc, t.id desc
  limit 1;

  -- canonicalize entries
  select string_agg(format('%s:%s', e.account_id::text, e.delta::text), ';'
                    order by e.account_id::text, e.delta::text)
    into entries_str
  from ledger_entries e
  where e.tx_id = NEW.id;

  payload := digest(coalesce(NEW.reason::text,'') ||
                    coalesce(NEW.bet_id::text,'') ||
                    NEW.created_at::text ||
                    coalesce(entries_str,''), 'sha256');

  -- Allow a single, controlled UPDATE here
  perform set_config('bets.allow_tx_hash_update', '1', true);
  update transactions
     set prev_hash = prev,
         hash      = digest(coalesce(prev,'') || payload, 'sha256')
   where id = NEW.id;
  perform set_config('bets.allow_tx_hash_update', '0', true);

  return NEW;
end;
$$ language plpgsql;

-- 3) Enforce: non-house user wallets must never go negative
--    (deferred check runs at commit; examines accounts affected by the tx)
create or replace function enforce_nonnegative_user_wallets() returns trigger as $$
declare
  bad_count int;
begin
  with affected_accounts as (
    select distinct a.id as account_id, u.username
    from ledger_entries le
    join accounts a on a.id = le.account_id
    join users    u on u.id = a.user_id
    where le.tx_id = NEW.id
  ),
  negatives as (
    select aa.account_id
    from affected_accounts aa
    join ledger_entries le2 on le2.account_id = aa.account_id
    where aa.username <> 'house'        -- house exempt
    group by aa.account_id
    having sum(le2.delta) < 0           -- resulting balance would be negative
  )
  select count(*) into bad_count from negatives;

  if bad_count > 0 then
    raise exception 'User wallet would go negative (tx=%)', NEW.id;
  end if;

  return NEW;
end;
$$ language plpgsql;

drop trigger if exists trg_tx_nonneg_user_wallets on transactions;
create constraint trigger trg_tx_nonneg_user_wallets
after insert on transactions
deferrable initially deferred
for each row execute function enforce_nonnegative_user_wallets();

