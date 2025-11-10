alter table users
  add column if not exists telegram_notify boolean not null default false;
