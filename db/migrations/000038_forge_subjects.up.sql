-- Build plan P4.4: the bridge's Forgejo route keys a thread forge/<owner>/<repo>/..., and core reads no github/ key
-- any more. The stored ones move with it, so a thread opened on GitHub continues under its forge key (the repos keep
-- their owner/name on Forgejo). The fleet is paused while this runs: no live thread. Sources and causation ids
-- (github.com/...:<id>) name events that happened on GitHub and stay.
UPDATE sessions SET thread_key = 'forge/' || substring(thread_key from 8) WHERE thread_key LIKE 'github/%';
UPDATE proposals SET thread_key = 'forge/' || substring(thread_key from 8) WHERE thread_key LIKE 'github/%';
-- An event proposal's dedup key is event:<definition>:<thread> (dispatch.proposalKey).
UPDATE proposals SET dedup_key = regexp_replace(dedup_key, '^(event:[^:]*:)github/', '\1forge/')
  WHERE dedup_key ~ '^event:[^:]*:github/';
UPDATE dispatch_decisions SET thread = 'forge/' || substring(thread from 8) WHERE thread LIKE 'github/%';
UPDATE decisions SET subject = 'forge/' || substring(subject from 8) WHERE subject LIKE 'github/%';
-- An open card's link moves with its key: core built subject.url from the subject (decisions.normalize), on GitHub.
-- ponytail: FORGE_URL's default, which SQL cannot read; a closed card keeps the link it was answered with.
UPDATE decisions SET payload = jsonb_set(payload, '{subject,url}', to_jsonb(regexp_replace(
    regexp_replace(payload #>> '{subject,url}', '^https://github\.com/', 'https://git.bnei.dev/'),
    '^(https://git\.bnei\.dev/[^/]+/[^/]+)/pull/', '\1/pulls/')))
  WHERE kind = 'card' AND state = 'open' AND payload #>> '{subject,url}' LIKE 'https://github.com/%';
