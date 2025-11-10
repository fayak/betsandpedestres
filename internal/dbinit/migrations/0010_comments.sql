create table if not exists comments (
    id uuid primary key default gen_random_uuid(),
    bet_id uuid not null references bets(id) on delete cascade,
    user_id uuid not null references users(id) on delete cascade,
    content text not null,
    upvotes integer not null default 0,
    downvotes integer not null default 0,
    created_at timestamptz not null default now()
);

create index if not exists idx_comments_bet on comments(bet_id);
create index if not exists idx_comments_order on comments(bet_id, created_at desc);

create table if not exists comment_reactions (
    comment_id uuid not null references comments(id) on delete cascade,
    user_id uuid not null references users(id) on delete cascade,
    value smallint not null check (value in (-1, 1)),
    created_at timestamptz not null default now(),
    primary key (comment_id, user_id)
);
