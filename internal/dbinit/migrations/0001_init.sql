-- Extensions
create extension if not exists pgcrypto;
create extension if not exists citext;

-- Enums
do $$
begin
  if not exists (select 1 from pg_type where typname = 'role_type') then
    create type role_type as enum ('unverified', 'user', 'moderator', 'admin');
  end if;
  if not exists (select 1 from pg_type where typname = 'bet_status') then
    create type bet_status as enum ('open', 'closed', 'resolved', 'cancelled');
  end if;
  if not exists (select 1 from pg_type where typname = 'tx_reason') then
    create type tx_reason as enum ('GIFT', 'AIRDROP', 'BET');
  end if;
end$$;

-- Users
create table if not exists users (
  id            uuid primary key default gen_random_uuid(),
  username      citext not null unique,
  display_name  text not null,
  password_hash text not null,
  role          role_type not null default 'unverified',
  created_at    timestamptz not null default now()
);

-- Bets (create BEFORE accounts to satisfy FK on accounts.bet_id)
create table if not exists bets (
  id              uuid primary key default gen_random_uuid(),
  creator_user_id uuid not null references users(id) on delete cascade,
  title           text not null,
  description     text,
  external_url    text,
  deadline        timestamptz,
  status          bet_status not null default 'open',
  created_at      timestamptz not null default now(),
  resolved_at     timestamptz,
  outcome_text    text
);
create index if not exists idx_bets_creator on bets(creator_user_id);
create index if not exists idx_bets_status on bets(status);
create index if not exists idx_bets_deadline on bets(deadline);

-- Accounts (user wallets OR per-bet escrow)
create table if not exists accounts (
  id          uuid primary key default gen_random_uuid(),
  user_id     uuid references users(id) on delete cascade,
  bet_id      uuid unique references bets(id) on delete cascade,
  name        text not null,
  is_default  boolean not null default false,
  created_at  timestamptz not null default now(),
  check ( (user_id is not null and bet_id is null) or (user_id is null and bet_id is not null) ),
  unique (user_id, is_default) deferrable initially immediate
);
create unique index if not exists idx_accounts_user_default_true
  on accounts(user_id) where is_default;

-- Wagers
create table if not exists wagers (
  id         uuid primary key default gen_random_uuid(),
  bet_id     uuid not null references bets(id) on delete cascade,
  user_id    uuid not null references users(id) on delete cascade,
  amount     bigint not null check (amount > 0),
  created_at timestamptz not null default now()
);
create index if not exists idx_wagers_bet on wagers(bet_id);
create index if not exists idx_wagers_user on wagers(user_id);

-- Moderator consensus
create table if not exists moderator_votes (
  bet_id       uuid not null references bets(id) on delete cascade,
  moderator_id uuid not null references users(id) on delete cascade,
  outcome_text text not null,
  voted_at     timestamptz not null default now(),
  primary key (bet_id, moderator_id)
);
create index if not exists idx_modvotes_bet on moderator_votes(bet_id);

create table if not exists bet_resolution (
  bet_id       uuid primary key references bets(id) on delete cascade,
  outcome_text text not null,
  method       text not null default 'moderator_consensus',
  decided_at   timestamptz not null default now()
);

-- Transactions + Ledger
create table if not exists transactions (
  id         uuid primary key default gen_random_uuid(),
  reason     tx_reason not null,
  bet_id     uuid references bets(id) on delete set null,
  note       text,
  created_at timestamptz not null default now(),
  prev_hash  bytea,
  hash       bytea
);
create index if not exists idx_tx_reason on transactions(reason);
create index if not exists idx_tx_bet on transactions(bet_id);
create unique index if not exists idx_tx_hash on transactions(hash);

create table if not exists ledger_entries (
  id         bigserial primary key,
  tx_id      uuid not null references transactions(id) on delete restrict,
  account_id uuid not null references accounts(id) on delete restrict,
  delta      bigint not null check (delta <> 0)
);
create index if not exists idx_entries_tx on ledger_entries(tx_id);
create index if not exists idx_entries_account on ledger_entries(account_id);

-- Enforce double-entry balance (deferred so entries can be inserted first)
create or replace function enforce_balanced_tx() returns trigger as $$
begin
  perform 1
  from (select coalesce(sum(delta),0) as s from ledger_entries where tx_id = NEW.id) t
  where t.s = 0;
  if not found then
    raise exception 'Transaction % is not balanced (sum of deltas <> 0)', NEW.id;
  end if;
  return NEW;
end; $$ language plpgsql;

drop trigger if exists trg_tx_balanced on transactions;
create constraint trigger trg_tx_balanced
after insert on transactions
deferrable initially deferred
for each row execute function enforce_balanced_tx();

-- Append-only protection
create or replace function forbid_mutations() returns trigger as $$
begin
  if tg_op in ('UPDATE','DELETE') then
    raise exception 'Append-only table: % not allowed on %', tg_op, tg_table_name;
  end if;
  return NEW;
end; $$ language plpgsql;

drop trigger if exists trg_tx_forbid_mutation on transactions;
create trigger trg_tx_forbid_mutation
before update or delete on transactions
for each row execute function forbid_mutations();

drop trigger if exists trg_entries_forbid_mutation on ledger_entries;
create trigger trg_entries_forbid_mutation
before update or delete on ledger_entries
for each row execute function forbid_mutations();

-- Hash chain computed at COMMIT-time (deferred), includes entries
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

  select string_agg(format('%s:%s', e.account_id::text, e.delta::text), ';'
                    order by e.account_id::text, e.delta::text)
    into entries_str
  from ledger_entries e
  where e.tx_id = NEW.id;

  payload := digest(coalesce(NEW.reason::text,'') ||
                    coalesce(NEW.bet_id::text,'') ||
                    NEW.created_at::text ||
                    coalesce(entries_str,''), 'sha256');

  update transactions
     set prev_hash = prev,
         hash      = digest(coalesce(prev,'') || payload, 'sha256')
   where id = NEW.id;

  return NEW;
end; $$ language plpgsql;

drop trigger if exists trg_tx_hash on transactions;
create constraint trigger trg_tx_hash
after insert on transactions
deferrable initially deferred
for each row execute function tx_compute_hash_deferred();

-- Auto-create default wallet for each new user
create or replace function create_default_wallet() returns trigger as $$
begin
  insert into accounts (user_id, name, is_default)
  values (NEW.id, 'wallet:' || NEW.username, true);
  return NEW;
end; $$ language plpgsql;

drop trigger if exists trg_users_default_wallet on users;
create trigger trg_users_default_wallet
after insert on users
for each row execute function create_default_wallet();

-- Views
create or replace view public_transactions as
select
  t.id,
  t.reason,
  t.bet_id,
  t.note,
  t.created_at,
  encode(t.prev_hash, 'hex') as prev_hash_hex,
  encode(t.hash, 'hex')      as hash_hex,
  jsonb_agg(
    jsonb_build_object(
      'account_id', e.account_id,
      'user_id', a.user_id,
      'delta', e.delta
    ) order by e.account_id
  ) as entries
from transactions t
join ledger_entries e on e.tx_id = t.id
join accounts a on a.id = e.account_id
group by t.id;

create or replace view user_balances as
select
  u.id as user_id,
  u.username,
  coalesce(sum(le.delta) filter (where a.user_id = u.id), 0) as balance
from users u
left join accounts a on a.user_id = u.id
left join ledger_entries le on le.account_id = a.id
group by u.id, u.username;

-- Cached balances (materialized)
drop materialized view if exists user_balances_mv;
create materialized view user_balances_mv as
select * from user_balances
with no data;
create unique index if not exists idx_user_balances_mv_uid on user_balances_mv(user_id);

-- Helper to refresh cached balances (optional use)
create or replace function refresh_user_balances() returns void as $$
begin
  execute 'refresh materialized view concurrently user_balances_mv';
end; $$ language plpgsql;

