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

    mashed-daemon start|stop|restart   # stop flushes pending counts first
    mashed-daemon enable|disable       # on/off at boot, and starts/stops it now

## Portal and API (localhost only)

The portal is at http://mashedkeys.local (or http://127.0.0.1). The service listens on 127.0.0.1:80.


    curl localhost/api/summary                 # since_boot, today, all_time
    curl 'localhost:7777/api/series?period=hour'    # minute|hour|day|month, optional from/to (unix)

`series` ranges are capped per period (minute 2 days, hour 400 days, day ~10 years, month 100 years); bad or reversed ranges get a 400. Requests time out after 10s, and past 16 API requests at once the rest get a 503 instead of queueing.

## Who can see the stats

The portal only answers to `localhost`, `127.0.0.1`, `[::1]` and `mashedkeys.local` (any other `Host` gets a 421), so a web page can't read it through DNS rebinding.

There is no login. Any local user or process on this machine can read the totals and boot time. This is fine on a single-user machine. On a shared machine, treat the counts as visible to everyone on it.

## If writes fail

If a write to the database fails (disk full, for example), the counts stay in memory and are retried on every flush. They're logged as failed and logged again once they're saved. Counts still unsaved when the process exits are lost, and the log says how many minutes.
