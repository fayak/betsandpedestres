-- Bet options (2–10 per bet; enforce max at DB, min via app)
create table if not exists bet_options (
  id       uuid primary key default gen_random_uuid(),
  bet_id   uuid not null references bets(id) on delete cascade,
  label    text not null,
  position smallint not null check (position between 1 and 10),
  unique (bet_id, label)
);
create index if not exists idx_betopt_bet on bet_options(bet_id);

-- Optional guard to prevent >10 options per bet
create or replace function enforce_max_10_options() returns trigger as $$
declare
  cnt int;
begin
  select count(*) into cnt from bet_options where bet_id = NEW.bet_id;
  if cnt >= 10 then
    raise exception 'A bet cannot have more than 10 options';
  end if;
  return NEW;
end; $$ language plpgsql;

drop trigger if exists trg_betopt_max on bet_options;
create trigger trg_betopt_max
before insert on bet_options
for each row execute function enforce_max_10_options();

-- Wagers must pick an option
alter table wagers
  add column option_id uuid;
alter table wagers
  add constraint fk_wager_option
  foreign key (option_id) references bet_options(id) on delete restrict;

-- If existing wagers exist and you need to backfill, do it here; otherwise:
alter table wagers alter column option_id set not null;
create index if not exists idx_wagers_option on wagers(option_id);
