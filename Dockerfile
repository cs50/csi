FROM alpine
LABEL maintainers="Kubernetes Authors"
LABEL description="HostPath Driver"
ARG binary=./bin/hostpathplugin

# Add util-linux to get a new version of losetup.
RUN apk add e2fsprogs e2fsprogs-extra util-linux
COPY ${binary} /hostpathplugin
ENTRYPOINT ["/hostpathplugin"]
