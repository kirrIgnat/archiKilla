CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE TABLE IF NOT EXISTS students (
 id bigserial PRIMARY KEY, name text NOT NULL, phone text NOT NULL DEFAULT '',
 subject text NOT NULL DEFAULT 'Музыка', notes text NOT NULL DEFAULT '',
 active boolean NOT NULL DEFAULT true, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS users (
 id bigserial PRIMARY KEY, login text NOT NULL UNIQUE, password_hash text NOT NULL,
 role text NOT NULL CHECK(role IN ('admin','student')),
 student_id bigint UNIQUE REFERENCES students(id),
 CHECK ((role='student' AND student_id IS NOT NULL) OR (role='admin' AND student_id IS NULL))
);
CREATE TABLE IF NOT EXISTS sessions (
 token_hash text PRIMARY KEY, user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 csrf text NOT NULL, expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expiry ON sessions(expires_at);
CREATE TABLE IF NOT EXISTS lessons (
 id bigserial PRIMARY KEY, student_id bigint NOT NULL REFERENCES students(id),
 title text NOT NULL, starts_at timestamptz NOT NULL, ends_at timestamptz NOT NULL,
 location text NOT NULL, price_kop bigint NOT NULL CHECK(price_kop BETWEEN 0 AND 100000000),
 status text NOT NULL DEFAULT 'planned' CHECK(status IN ('planned','completed','cancelled','missed')),
 notes text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT now(),
 CHECK(ends_at>starts_at),
 EXCLUDE USING gist (tstzrange(starts_at, ends_at, '[)') WITH &&) WHERE (status IN ('planned','completed'))
);
CREATE INDEX IF NOT EXISTS lessons_student_time ON lessons(student_id, starts_at);
CREATE TABLE IF NOT EXISTS homework (
 id bigserial PRIMARY KEY, student_id bigint NOT NULL REFERENCES students(id),
 title text NOT NULL, body text NOT NULL, resource_url text NOT NULL DEFAULT '',
 due_at timestamptz NOT NULL, status text NOT NULL DEFAULT 'assigned' CHECK(status IN ('assigned','submitted','reviewed')),
 answer text NOT NULL DEFAULT '', answer_url text NOT NULL DEFAULT '', feedback text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS homework_student_due ON homework(student_id, due_at);
CREATE TABLE IF NOT EXISTS payments (
 id bigserial PRIMARY KEY, student_id bigint NOT NULL REFERENCES students(id),
 amount_kop bigint NOT NULL CHECK(amount_kop BETWEEN 1 AND 100000000), paid_at timestamptz NOT NULL,
 method text NOT NULL CHECK(method IN ('cash','transfer','other')), note text NOT NULL DEFAULT '',
 request_id text UNIQUE NOT NULL, void_reason text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS payments_student ON payments(student_id);
CREATE TABLE IF NOT EXISTS telegram_links (
 student_id bigint PRIMARY KEY REFERENCES students(id), telegram_id bigint NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS telegram_invites (
 token_hash text PRIMARY KEY, student_id bigint NOT NULL UNIQUE REFERENCES students(id),
 expires_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS bot_state (key text PRIMARY KEY, value bigint NOT NULL);
