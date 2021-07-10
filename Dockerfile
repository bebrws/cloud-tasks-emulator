FROM debian

RUN apt-get update && apt-get install -y golang bash git build-essential

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN go build -o emulator .

CMD [ "./emulator" ]
