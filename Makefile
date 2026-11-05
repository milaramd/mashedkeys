BIN := mashedkeys

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o $(BIN) .

test:
	go test ./...

install: build
	sudo install -m 755 $(BIN) /usr/local/bin/$(BIN)
	sudo install -m 644 deploy/mashedkeys.service /etc/systemd/system/mashedkeys.service
	sudo systemctl daemon-reload
	sudo systemctl enable --now mashedkeys
	sudo systemctl restart mashedkeys

uninstall:
	sudo systemctl disable --now mashedkeys
	sudo rm -f /usr/local/bin/$(BIN) /etc/systemd/system/mashedkeys.service
	sudo systemctl daemon-reload

.PHONY: build test install uninstall
