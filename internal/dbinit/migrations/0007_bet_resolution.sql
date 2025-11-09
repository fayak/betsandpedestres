-- Final outcome stored on bets + resolution time
alter table bets
  add column if not exists resolution_option_id uuid,
  add column if not exists resolved_at timestamptz;

alter table bets
  add constraint fk_bets_resolution_option
  foreign key (resolution_option_id) references bet_options(id)
  on delete set null;

-- Moderator votes for a bet’s outcome
create table if not exists bet_resolution_votes (
  bet_id      uuid not null references bets(id) on delete cascade,
  user_id     uuid not null references users(id) on delete cascade,
  option_id   uuid not null references bet_options(id) on delete cascade,
  created_at  timestamptz not null default now(),
  primary key (bet_id, user_id)
);

create index if not exists idx_brv_bet on bet_resolution_votes(bet_id);
