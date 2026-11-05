# mashedkeys

Counts keystrokes, not which keys. It reads `/dev/input` keyboards and saves per-minute totals to SQLite:

- key presses (autorepeat ignored)
- estimated words: a separator (space/tab/enter) after at least one typed character, with backspace taking characters off the current word
- backspaces, deletes, enters
- time between keystrokes: an average (gaps of 2s or more count as pauses and are left out) plus a histogram `<100 / <200 / <500 / <1000 / <2000 / >=2000 ms`

Only counters are stored. No key codes, no order.

## Run

    make test
    make install      # builds, installs to /usr/local/bin, enables the systemd unit
    make local-dns    # http://mashedkeys.local on this machine only
    journalctl -u mashedkeys -f

## Portal and API (localhost only)

The portal is at http://mashedkeys.local (or http://127.0.0.1). The service listens on 127.0.0.1:80.


    curl localhost/api/summary                 # since_boot, today, all_time
    curl 'localhost:7777/api/series?period=hour'    # minute|hour|day|month, optional from/to (unix)
