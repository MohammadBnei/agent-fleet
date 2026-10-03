FROM migrate/migrate:v4.20.1
RUN apk add --no-cache jq
COPY db/migrations /migrations
COPY migration/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
