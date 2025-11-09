-- Ensure each bet has at least 2 options at COMMIT time.
-- Works on initial creation (bet + options in one tx) and prevents deleting down to <2.

create or replace function enforce_min_2_options_on_bet() returns trigger as $$
declare
  cnt int;
begin
  select count(*) into cnt from bet_options where bet_id = NEW.id;
  if cnt < 2 then
    raise exception 'Bet % must have at least 2 options (found %)', NEW.id, cnt;
  end if;
  return NEW;
end;
$$ language plpgsql;

drop trigger if exists trg_bet_min_opts on bets;
create constraint trigger trg_bet_min_opts
after insert or update on bets
deferrable initially deferred
for each row execute function enforce_min_2_options_on_bet();

-- Also guard deletes/changes on bet_options that would drop below 2 for any bet.
create or replace function enforce_min_2_options_on_options() returns trigger as $$
declare
  target uuid;
  cnt int;
begin
  -- Determine which bet to check depending on op
  if tg_op = 'DELETE' then
    target := OLD.bet_id;
  else
    target := NEW.bet_id;
  end if;

  select count(*) into cnt from bet_options where bet_id = target;
  if cnt < 2 then
    raise exception 'Bet % must have at least 2 options (found %)', target, cnt;
  end if;
  return coalesce(NEW, OLD);
end;
$$ language plpgsql;

drop trigger if exists trg_betopt_min_opts_u on bet_options;
drop trigger if exists trg_betopt_min_opts_d on bet_options;

create constraint trigger trg_betopt_min_opts_u
after insert or update on bet_options
deferrable initially deferred
for each row execute function enforce_min_2_options_on_options();

create constraint trigger trg_betopt_min_opts_d
after delete on bet_options
deferrable initially deferred
for each row execute function enforce_min_2_options_on_options();

