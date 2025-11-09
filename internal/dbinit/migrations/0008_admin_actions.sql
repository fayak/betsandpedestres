create table if not exists admin_actions (
  id              uuid primary key default gen_random_uuid(),
  admin_user_id   uuid not null references users(id) on delete cascade,
  target_user_id  uuid references users(id) on delete cascade,
  action          text not null,
  old_role        role_type,
  new_role        role_type,
  note            text,
  created_at      timestamptz not null default now()
);
create index if not exists idx_admin_actions_admin on admin_actions(admin_user_id);
create index if not exists idx_admin_actions_target on admin_actions(target_user_id);
