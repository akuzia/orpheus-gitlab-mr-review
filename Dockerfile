FROM gcr.io/distroless/base as distroless

FROM scratch

COPY --from=distroless /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

WORKDIR /var/app
ADD ./bin/app /var/app/

CMD ["/var/app/app"]
