-- Fix the URL seeded by 000008: it used the SSH form, but the provisioner
-- authenticates with `gh auth setup-git`, which wires HTTPS credentials
-- only. An SSH URL can never authenticate, so every thot task died with
-- "clone/fetch failed" and went failed_permanently.
--
-- Confirmed live 2026-08-11: every other repo in this table is HTTPS;
-- infra-bootstrap was the only SSH one, and the only one that couldn't
-- clone.
UPDATE repos
SET url = 'https://github.com/MohammadBnei/infra-bootstrap.git'
WHERE name = 'infra-bootstrap';
