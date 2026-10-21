FROM migrate/migrate:v4.20.1
# Unpinned installs below: docker.yml passes the ISO week, so a cached layer
# is refreshed at least weekly instead of living forever.
ARG CACHE_WEEK=none
RUN apk add --no-cache jq
COPY db/migrations /migrations
COPY migration/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
