FROM golang:1.7.4

RUN mkdir -p /go/src/app
WORKDIR /go/src/app

COPY . /go/src/app

RUN make
