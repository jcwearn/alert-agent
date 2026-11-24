# Phase 1 image: the controller only, on distroless static. The runner role
# needs the claude CLI, kubectl, flux, kustomize and git, so Phase 2 moves this
# to a base that carries them -- one image serves both roles.
#
# Digests are pinned rather than tags; Renovate bumps them as reviewable PRs.

FROM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build

WORKDIR /src

# go.sum joins this line with the first dependency.
COPY go.mod ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/alert-agent ./cmd/alert-agent

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=build /out/alert-agent /alert-agent

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/alert-agent"]
CMD ["controller"]
