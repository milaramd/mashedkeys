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

# mashedkeys.local -> 127.0.0.1 on this machine only. nss-mdns answers
# *.local before /etc/hosts, so "files" is moved ahead of mdns_minimal.
local-dns:
	grep -q '\smashedkeys\.local\b' /etc/hosts || echo '127.0.0.1 mashedkeys.local' | sudo tee -a /etc/hosts >/dev/null
	grep -qE '^hosts:.*files.*mdns_minimal' /etc/nsswitch.conf || \
		sudo sed -i.bak -E '/^hosts:/{s/ files\b//; s/^hosts:( mymachines)?/hosts:\1 files/}' /etc/nsswitch.conf
	grep '^hosts:' /etc/nsswitch.conf
	getent hosts mashedkeys.local

.PHONY: build test install uninstall local-dns
