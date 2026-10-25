FROM gcr.io/distroless/static:nonroot
COPY bin/controller /controller
USER 65532:65532
ENTRYPOINT ["/controller"]
