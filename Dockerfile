FROM golang:1.10
EXPOSE 8080

RUN go get \
  cloud.google.com/go/datastore \
  golang.org/x/net/http2

ENV WORKDIR_PATH /go/src/github.com/fika-io/push

RUN mkdir -p $WORKDIR_PATH
ADD . $WORKDIR_PATH
WORKDIR $WORKDIR_PATH

RUN go build .
CMD [ "/bin/bash", "-c", "$WORKDIR_PATH/push" ]
