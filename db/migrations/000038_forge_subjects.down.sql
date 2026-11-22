UPDATE sessions SET thread_key = 'github/' || substring(thread_key from 7) WHERE thread_key LIKE 'forge/%';
UPDATE proposals SET thread_key = 'github/' || substring(thread_key from 7) WHERE thread_key LIKE 'forge/%';
UPDATE proposals SET dedup_key = regexp_replace(dedup_key, '^(event:[^:]*:)forge/', '\1github/')
  WHERE dedup_key ~ '^event:[^:]*:forge/';
UPDATE dispatch_decisions SET thread = 'github/' || substring(thread from 7) WHERE thread LIKE 'forge/%';
UPDATE decisions SET subject = 'github/' || substring(subject from 7) WHERE subject LIKE 'forge/%';
UPDATE decisions SET payload = jsonb_set(payload, '{subject,url}', to_jsonb(regexp_replace(
    regexp_replace(payload #>> '{subject,url}', '^https://git\.bnei\.dev/', 'https://github.com/'),
    '^(https://github\.com/[^/]+/[^/]+)/pulls/', '\1/pull/')))
  WHERE kind = 'card' AND state = 'open' AND payload #>> '{subject,url}' LIKE 'https://git.bnei.dev/%';
