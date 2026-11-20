-- The prompt draft: changes administrators have made to the live prompts
-- that are not live yet. The draft is the live release with these entries
-- applied, and going live clears them. A NULL body removes the prompt.
-- base_sha256 is the live text's hash when the entry was written, NULL when
-- live had no such prompt; an entry whose base no longer matches live was
-- written against text that has since changed.
CREATE TABLE prompt_draft_entries (
    prompt_key  text PRIMARY KEY,
    body        text,
    base_sha256 text,
    updated_by  text NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CHECK (btrim(prompt_key) <> ''),
    CHECK (body IS NULL OR (btrim(body) <> '' AND body = btrim(body))),
    CHECK (base_sha256 IS NULL OR base_sha256 ~ '^[0-9a-f]{64}$'),
    CHECK (btrim(updated_by) <> '')
);
