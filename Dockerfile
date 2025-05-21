FROM scratch
COPY bin/ctrl /ctrl
ENTRYPOINT ["/ctrl"]
LABEL org.opencontainers.image.source https://github.com/galleybytes/infrakube-connector