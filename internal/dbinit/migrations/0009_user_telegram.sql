alter table users
    add column if not exists telegram_chat_id bigint;
