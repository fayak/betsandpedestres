alter table comments
  add column if not exists parent_comment_id uuid references comments(id) on delete cascade;

create index if not exists idx_comments_parent on comments(parent_comment_id);
