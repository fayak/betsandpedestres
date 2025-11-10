create table if not exists password_recoveries (
    user_id uuid primary key references users(id) on delete cascade,
    token text not null,
    expires_at timestamptz not null
);

create index if not exists idx_password_recoveries_expires on password_recoveries(expires_at);
