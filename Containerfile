FROM golang:1.26 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY internal/ ./internal
COPY cmd/ ./cmd

RUN CGO_ENABLED=0 GOOS=linux go build -o /mysql-anonymizer ./cmd/mysql-anonymizer


FROM registry.access.redhat.com/ubi9/ubi-minimal

ARG REPO_FILE=mysql84-community-release-el9-4.noarch.rpm

RUN curl -L -O "https://dev.mysql.com/get/${REPO_FILE}" && \
    rpm -i ${REPO_FILE} && \
    rm ${REPO_FILE}

RUN microdnf install -y mysql-shell diffutils && \
    microdnf clean all

COPY --from=peakcom/s5cmd:v2.3.0 /s5cmd /usr/local/bin/

COPY --from=builder /mysql-anonymizer /usr/local/bin/
